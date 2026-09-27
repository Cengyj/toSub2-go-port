package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"tosub2-go-port/internal/config"
	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/logging"
	"tosub2-go-port/internal/privatefile"
	"tosub2-go-port/internal/protocol"
	"tosub2-go-port/internal/store"
	"tosub2-go-port/internal/transport"
)

type refreshRecoveryDoer func(context.Context, transport.Request) (*transport.Response, error)

func (f refreshRecoveryDoer) Do(ctx context.Context, r transport.Request) (*transport.Response, error) {
	return f(ctx, r)
}

func refreshRotationFixture(t *testing.T, path string) []byte {
	t.Helper()
	raw := map[string]any{"type": "sub2api-data", "version": 1, "large_custom_id": json.Number("9007199254740993"), "proxies": []any{map[string]any{"name": "fixture-proxy"}}, "accounts": []any{
		map[string]any{"name": "First custom name", "priority": 0, "extra": map[string]any{"client_id": "fixture-client", "custom": "keep"},
			"credentials": map[string]any{"email": "first@example.test", "access_token": "fixture-old-access", "refresh_token": "fixture-old-refresh", "id_token": "fixture-opaque-id", "custom": "keep"}},
		map[string]any{"name": "Second custom name", "custom": json.Number("9007199254740995"), "credentials": map[string]any{"email": "second@example.test", "refresh_token": "fixture-second-refresh"}},
	}}
	if err := protocol.WritePrivateJSON(path, raw); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func assertRotatedFirstAccount(t *testing.T, path string, original []byte) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := decodeRefreshDocument(data)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := decodeRefreshDocument(original)
	accounts := got["accounts"].([]any)
	first := accounts[0].(map[string]any)
	creds := first["credentials"].(map[string]any)
	if len(accounts) != 2 || creds["access_token"] != "fixture-new-access" || creds["refresh_token"] != "fixture-rotated-refresh" || creds["id_token"] != "fixture-opaque-id" || first["name"] != "First custom name" || creds["custom"] != "keep" {
		t.Fatal("refresh changed first-account metadata or lost the rotating grant")
	}
	second, _ := json.Marshal(accounts[1])
	originalSecond, _ := json.Marshal(before["accounts"].([]any)[1])
	if !bytes.Equal(second, originalSecond) || got["large_custom_id"] != json.Number("9007199254740993") {
		t.Fatal("refresh changed later accounts or rounded unknown numeric metadata")
	}
}

func TestRefreshGrantRecoveryProcessHelper(t *testing.T) {
	if os.Getenv("TOSUB2_REFRESH_RECOVERY_HELPER") != "1" {
		t.Skip("separate-process recovery helper")
	}
	root := os.Getenv("TOSUB2_REFRESH_RECOVERY_ROOT")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := refreshExistingExportAt(ctx, transport.NewClient(nil), os.Getenv("TOSUB2_REFRESH_RECOVERY_BASE"), "fallback-client",
		filepath.Join(root, "source.json"), filepath.Join(root, "target.json"), "first@example.test")
	if os.Getenv("TOSUB2_REFRESH_RECOVERY_FAIL") == "1" {
		if err == nil || !isLocalRefreshFailure(err) || !strings.Contains(err.Error(), "REFRESH_OUTPUT_PENDING") {
			t.Fatal("accepted grant did not report a recoverable local output failure", err)
		}
	} else if err != nil {
		t.Fatal("saved refresh grant failed in a new process", err)
	}
}

func TestRefreshRotationSurvivesOutputFailureAcrossProcesses(t *testing.T) {
	root := t.TempDir()
	source, target := filepath.Join(root, "source.json"), filepath.Join(root, "target.json")
	original := refreshRotationFixture(t, source)
	checkpoint := filepath.Join(root, "login-checkpoint.json")
	checkpointData := []byte("unrelated browser checkpoint must remain byte-identical")
	if err := os.WriteFile(checkpoint, checkpointData, 0600); err != nil {
		t.Fatal(err)
	}
	var grants atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth/token" || r.Method != http.MethodPost {
			t.Error("offline refresh recovery started unrelated authentication")
			w.WriteHeader(404)
			return
		}
		if grants.Add(1) != 1 {
			t.Error("a consumed refresh grant was repeated")
			w.WriteHeader(400)
			io.WriteString(w, "{\"error\":\"invalid_grant\"}")
			return
		}
		r.ParseForm()
		if r.Form.Get("refresh_token") != "fixture-old-refresh" || r.Form.Get("client_id") != "fixture-client" {
			t.Error("refresh used a later account or different OAuth client")
		}
		if err := os.Mkdir(target, 0700); err != nil {
			t.Error(err)
		}
		json.NewEncoder(w).Encode(protocol.TokenSet{AccessToken: "fixture-new-access", RefreshToken: "fixture-rotated-refresh"})
	}))
	defer server.Close()
	run := func(fail bool) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestRefreshGrantRecoveryProcessHelper$")
		cmd.Env = append(os.Environ(), "TOSUB2_REFRESH_RECOVERY_HELPER=1", "TOSUB2_REFRESH_RECOVERY_ROOT="+root, "TOSUB2_REFRESH_RECOVERY_BASE="+server.URL, "TOSUB2_REFRESH_RECOVERY_FAIL=0")
		if fail {
			cmd.Env[len(cmd.Env)-1] = "TOSUB2_REFRESH_RECOVERY_FAIL=1"
		}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("refresh recovery subprocess failed: %v\n%s", err, out)
		}
	}
	run(true)
	f, err := os.Open(refreshRecoveryPath(target))
	if err != nil {
		t.Fatal("accepted rotation was not retained privately", err)
	}
	err = privatefile.Verify(f)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	run(false)
	assertRotatedFirstAccount(t, target, original)
	unchanged, _ := os.ReadFile(source)
	checkpointAfter, _ := os.ReadFile(checkpoint)
	if grants.Load() != 1 || !bytes.Equal(unchanged, original) || !bytes.Equal(checkpointData, checkpointAfter) {
		t.Fatal("recovery replayed the grant or changed the original source/checkpoint")
	}
	if _, err := os.Stat(refreshRecoveryPath(target)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("completed refresh retained its recovery material", err)
	}
}

func pendingRotationFixture(t *testing.T) (string, string, string) {
	t.Helper()
	root := t.TempDir()
	source, target := filepath.Join(root, "source.json"), filepath.Join(root, "target.json")
	refreshRotationFixture(t, source)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	doer := refreshRecoveryDoer(func(context.Context, transport.Request) (*transport.Response, error) {
		cancel()
		return &transport.Response{StatusCode: 200, Body: []byte("{\"access_token\":\"fixture-new-access\",\"refresh_token\":\"fixture-rotated-refresh\"}")}, nil
	})
	err := refreshExistingExportAt(ctx, doer, "https://auth.example.test", "fallback-client", source, target, "first@example.test")
	if !isLocalRefreshFailure(err) || !errors.Is(err, context.Canceled) {
		t.Fatal("accepted refresh did not preserve cancellation and recovery data", err)
	}
	return root, source, target
}

func TestRefreshRecoveryRejectsChangedSourceOrBinding(t *testing.T) {
	for _, scenario := range []string{"source-content", "source-path", "target-path", "email", "auth-origin", "client", "recovery-version", "source-version", "hash", "target-content"} {
		t.Run(scenario, func(t *testing.T) {
			root, source, target := pendingRotationFixture(t)
			base, email := "https://auth.example.test", "first@example.test"
			file, err := os.Open(refreshRecoveryPath(target))
			if err != nil {
				t.Fatal(err)
			}
			var rec refreshGrantRecovery
			d := json.NewDecoder(file)
			d.UseNumber()
			err = d.Decode(&rec)
			file.Close()
			if err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "source-content":
				changed, _ := os.ReadFile(source)
				changed = bytes.Replace(changed, []byte("First custom name"), []byte("Changed source name"), 1)
				if err := os.WriteFile(source, changed, 0600); err != nil {
					t.Fatal(err)
				}
			case "source-path":
				source = filepath.Join(root, "different-source.json")
				refreshRotationFixture(t, source)
			case "target-path":
				target = filepath.Join(root, "different-target.json")
			case "email":
				email = "second@example.test"
			case "auth-origin":
				base = "https://different.example.test"
			case "client":
				rec.ClientID = "different-client"
			case "recovery-version":
				rec.Version = 2
			case "source-version":
				rec.Source["version"] = 2
				rec.SourceSHA256, _ = refreshDocumentHash(rec.Source)
			case "hash":
				rec.SourceSHA256 = "not-the-original-source"
			case "target-content":
				if err := os.WriteFile(target, []byte("user created a different destination"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := protocol.WritePrivateJSON(refreshRecoveryPath(target), rec); err != nil {
				t.Fatal(err)
			}
			handled, err := finishPendingRefreshAt(context.Background(), base, "fallback-client", source, target, email)
			if !handled || !isLocalRefreshFailure(err) || strings.Contains(err.Error(), "fixture-rotated") {
				t.Fatal("changed recovery identity was accepted, disclosed, or allowed network fallback")
			}
			if _, err := os.Stat(refreshRecoveryPath(target)); err != nil {
				t.Fatal("failed recovery discarded accepted grant", err)
			}
		})
	}
}

func TestRefreshRecoveryUnsavedGrantStopsAutomaticRetry(t *testing.T) {
	root := t.TempDir()
	source, target := filepath.Join(root, "source.json"), filepath.Join(root, "target.json")
	original := refreshRotationFixture(t, source)
	var calls int
	doer := refreshRecoveryDoer(func(context.Context, transport.Request) (*transport.Response, error) {
		calls++
		if err := os.Mkdir(refreshRecoveryPath(target), 0700); err != nil {
			t.Fatal(err)
		}
		return &transport.Response{StatusCode: 200, Body: []byte("{\"access_token\":\"fixture-new-access\",\"refresh_token\":\"fixture-rotated-refresh\"}")}, nil
	})
	err := refreshExistingExportAt(context.Background(), doer, "https://auth.example.test", "client", source, target, "first@example.test")
	if !isLocalRefreshFailure(err) || !strings.Contains(err.Error(), "REFRESH_GRANT_UNSAVED") {
		t.Fatal("unpersisted accepted grant did not explicitly prohibit automatic retry", err)
	}
	if err := refreshExistingExportAt(context.Background(), doer, "https://auth.example.test", "client", source, target, "first@example.test"); !isLocalRefreshFailure(err) || calls != 1 {
		t.Fatal("blocked recovery triggered another refresh request", err)
	}
	after, _ := os.ReadFile(source)
	if !bytes.Equal(original, after) {
		t.Fatal("failed grant persistence overwrote the original input")
	}
}

func TestRefreshRecoveryFinishesAlreadyWrittenResult(t *testing.T) {
	_, source, target := pendingRotationFixture(t)
	original, _ := os.ReadFile(source)
	data, err := os.ReadFile(refreshRecoveryPath(target))
	if err != nil {
		t.Fatal(err)
	}
	var rec refreshGrantRecovery
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	if err := d.Decode(&rec); err != nil {
		t.Fatal(err)
	}
	updated := updateRefreshedDocument(rec.Source, rec.Tokens, rec.ClientID)
	updated["exported_at"] = rec.AcceptedAt
	if err := protocol.WritePrivateJSON(target, updated); err != nil {
		t.Fatal(err)
	}
	// Emulate termination after output rename but before sidecar removal.
	before, _ := os.ReadFile(target)
	handled, err := finishPendingRefreshAt(context.Background(), "https://auth.example.test", "fallback-client", source, target, "first@example.test")
	if err != nil || !handled {
		t.Fatal("already-written accepted output was not recognized", err)
	}
	after, _ := os.ReadFile(target)
	if !bytes.Equal(before, after) {
		t.Fatal("already-written result was unnecessarily regenerated")
	}
	assertRotatedFirstAccount(t, target, original)
	if _, err := os.Stat(refreshRecoveryPath(target)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("completed output recovery was not cleaned up", err)
	}
}

func TestRefreshSameFileCancellationRecoversWithoutReissuingGrant(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "job", "sub2api-import-oauth.json")
	original := refreshRotationFixture(t, path)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var grants int
	doer := refreshRecoveryDoer(func(context.Context, transport.Request) (*transport.Response, error) {
		grants++
		cancel()
		return &transport.Response{StatusCode: 200, Body: []byte("{\"access_token\":\"fixture-new-access\",\"refresh_token\":\"fixture-rotated-refresh\"}")}, nil
	})
	if err := refreshExistingExport(ctx, doer, "https://auth.example.test", "fallback-client", root, "job", ""); !errors.Is(err, context.Canceled) || !isLocalRefreshFailure(err) {
		t.Fatal("accepted in-place refresh did not retain its canceled grant", err)
	}
	before, _ := os.ReadFile(path)
	if !bytes.Equal(before, original) {
		t.Fatal("canceled refresh unexpectedly replaced output")
	}
	if err := refreshExistingExport(context.Background(), doer, "https://auth.example.test", "fallback-client", root, "job", ""); err != nil {
		t.Fatal(err)
	}
	if grants != 1 {
		t.Fatal("in-place recovery replayed the consumed grant")
	}
	assertRotatedFirstAccount(t, path, original)
}

func TestCLIRefreshSelectsOnlyFirstAccount(t *testing.T) {
	source := filepath.Join(t.TempDir(), "source.json")
	refreshRotationFixture(t, source)
	for _, email := range []string{"", "FIRST@EXAMPLE.TEST"} {
		got, err := cliAccountEmail(config.CLIConfig{RefreshSub2API: source, Email: email})
		if err != nil || got != "first@example.test" {
			t.Fatal("multi-account refresh did not select its first account", err)
		}
	}
	if _, err := cliAccountEmail(config.CLIConfig{RefreshSub2API: source, Email: "second@example.test"}); err == nil {
		t.Fatal("explicit later account incorrectly selected the first account's grant")
	}
}

func TestCLIRefreshRecoversWithNewRandomJobID(t *testing.T) {
	clearCLISensitiveEnvironment(t)
	root := t.TempDir()
	source, target := filepath.Join(root, "source.json"), filepath.Join(root, "target.json")
	original := refreshRotationFixture(t, source)
	var requests, grants atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method == http.MethodGet && r.URL.Path == "/" {
			w.WriteHeader(200)
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != "/oauth/token" || grants.Add(1) != 1 {
			t.Error("CLI recovery replayed a rotating grant or restarted login")
			w.WriteHeader(400)
			return
		}
		if err := os.Mkdir(target, 0700); err != nil {
			t.Error(err)
		}
		json.NewEncoder(w).Encode(protocol.TokenSet{AccessToken: "fixture-new-access", RefreshToken: "fixture-rotated-refresh"})
	}))
	defer server.Close()
	cfg, err := config.Parse([]string{"--refresh-sub2api=" + source, "--sub2api-out=" + target, "--output-root=" + filepath.Join(root, "jobs"),
		"--auth-base=" + server.URL, "--chatgpt-base=http://127.0.0.1:1", "--native-http", "--browser=false"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var output, logs bytes.Buffer
	if handled, err := runCLIWithIO(ctx, cfg, logging.NewWithWriter(&logs), strings.NewReader(""), &output); !handled || err == nil || !strings.Contains(err.Error(), "REFRESH_OUTPUT_PENDING") {
		t.Fatal("first CLI run did not preserve the accepted grant on output failure", err)
	}
	if caches, err := filepath.Glob(filepath.Join(cfg.OutputRoot, "cli-*", "sub2api-import-oauth.json")); err != nil || len(caches) != 0 {
		t.Fatal("failed refresh left the consumed source token in a job download cache", err)
	}
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	beforeRequests := requests.Load()
	// An offline restart must bypass all network, proxy, TLS and SDK setup.
	cfg.CLI.NativeHTTP = false
	cfg.TLSProfile = "retired-unavailable-profile"
	cfg.ProxyURL = "http://127.0.0.1:1"
	if handled, err := runCLIWithIO(ctx, cfg, logging.NewWithWriter(&logs), strings.NewReader(""), &output); !handled || err != nil {
		t.Fatal("second CLI invocation did not recover the stable private grant", err)
	}
	paths, _ := filepath.Glob(filepath.Join(cfg.OutputRoot, "cli-*", "job-meta.json"))
	if len(paths) != 2 || grants.Load() != 1 || requests.Load() != beforeRequests {
		t.Fatal("CLI retry reused a job or unnecessarily opened a network session")
	}
	assertRotatedFirstAccount(t, target, original)
	after, _ := os.ReadFile(source)
	if !bytes.Equal(after, original) {
		t.Fatal("CLI refresh changed the original multi-account source")
	}
	for _, secret := range []string{"fixture-old-refresh", "fixture-rotated-refresh", "fixture-new-access", "fixture-second-refresh"} {
		if strings.Contains(output.String()+logs.String(), secret) {
			t.Fatal("CLI recovery leaked a token in public output")
		}
	}
}

func TestCLIRefreshCacheFailureNeverLeavesOldGrant(t *testing.T) {
	clearCLISensitiveEnvironment(t)
	root := t.TempDir()
	source, target := filepath.Join(root, "source.json"), filepath.Join(root, "target.json")
	outputRoot := filepath.Join(root, "jobs")
	original := refreshRotationFixture(t, source)
	var cachePath atomic.Value
	var grants atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/" {
			w.WriteHeader(200)
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != "/oauth/token" || grants.Add(1) != 1 {
			t.Error("cache failure triggered repeated refresh or unrelated authentication")
			w.WriteHeader(400)
			return
		}
		dirs, err := filepath.Glob(filepath.Join(outputRoot, "cli-*"))
		if err != nil || len(dirs) != 1 {
			t.Error("CLI job directory missing before grant", err)
			w.WriteHeader(500)
			return
		}
		path := filepath.Join(dirs[0], "sub2api-import-oauth.json")
		cachePath.Store(path)
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Error("CLI pre-staged old source credentials before refreshing", err)
		}
		// Only the optional job cache is unavailable. The stable user target
		// and its durable grant recovery remain writable.
		if err := os.Mkdir(path, 0700); err != nil {
			t.Error(err)
		}
		json.NewEncoder(w).Encode(protocol.TokenSet{AccessToken: "fixture-new-access", RefreshToken: "fixture-rotated-refresh"})
	}))
	defer server.Close()
	cfg, err := config.Parse([]string{"--refresh-sub2api=" + source, "--sub2api-out=" + target, "--output-root=" + outputRoot,
		"--auth-base=" + server.URL, "--chatgpt-base=http://127.0.0.1:1", "--native-http", "--browser=false"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var output, logs bytes.Buffer
	if handled, err := runCLIWithIO(ctx, cfg, logging.NewWithWriter(&logs), strings.NewReader(""), &output); !handled || err != nil {
		t.Fatal("optional job cache failure turned durable refresh success into failure", err)
	}
	assertRotatedFirstAccount(t, target, original)
	path, _ := cachePath.Load().(string)
	if path == "" || grants.Load() != 1 {
		t.Fatal("cache regression did not exercise the production grant")
	}
	if _, err := os.ReadFile(path); err == nil {
		t.Fatal("failed optional cache retained readable old credentials")
	}
	metadata, err := os.ReadFile(filepath.Join(filepath.Dir(path), "job-meta.json"))
	if err != nil || strings.Contains(string(metadata), "fixture-old-refresh") || strings.Contains(string(metadata), "fixture-rotated-refresh") {
		t.Fatal("job metadata missing or exposed refresh credentials", err)
	}
	st, err := store.New(outputRoot)
	if err != nil {
		t.Fatal(err)
	}
	manager := jobs.NewManager(ctx, 1, st, nil)
	manager.SetCredentialStore(credentials.NewMemoryStore())
	if err := manager.SyncOutputs(ctx, true); err != nil {
		t.Fatal(err)
	}
	for _, job := range manager.List() {
		if job.ResultSaved {
			t.Fatal("output discovery exposed a stale CLI download after cache failure")
		}
	}
}
