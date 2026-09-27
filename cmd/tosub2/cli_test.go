package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"tosub2-go-port/internal/config"
	"tosub2-go-port/internal/logging"
	"tosub2-go-port/internal/protocol"
	"tosub2-go-port/internal/transport"
)

func TestCLIHelpAndConsoleMode(t *testing.T) {
	var out bytes.Buffer
	cfg, err := config.Parse([]string{"--help"})
	if err != nil {
		t.Fatal(err)
	}
	handled, err := runCLIWithIO(context.Background(), cfg, nil, strings.NewReader(""), &out)
	if err != nil || !handled || !strings.Contains(out.String(), "--resume-checkpoint") || !strings.Contains(out.String(), "--output-mode both|session|sub2api") {
		t.Fatalf("help: %v %v %s", handled, err, out.String())
	}
	cfg, err = config.Parse(nil)
	if err != nil {
		t.Fatal(err)
	}
	handled, err = runCLIWithIO(context.Background(), cfg, nil, strings.NewReader(""), io.Discard)
	if handled || err != nil {
		t.Fatalf("console incorrectly handled: %v %v", handled, err)
	}
}
func TestCLIInputPreservesPrivatePathsAndModes(t *testing.T) {
	cfg, err := config.Parse([]string{"--email=owner@example.test", "--resume-checkpoint=saved-checkpoint.json", "--out=private-session.json", "--sub2api-name=personal", "--workspace-id=work", "--concurrency=4", "--priority=2", "--rate-multiplier=0.5", "--no-sub2api-export", "--web-only"})
	if err != nil {
		t.Fatal(err)
	}
	got := cliInput(cfg.CLI, "job-directory")
	if got["checkpointPath"] != "saved-checkpoint.json" || got["sessionOutputPath"] != "private-session.json" || got["outputMode"] != "session" || got["webOnly"] != true || got["accountName"] != "personal" || got["workspaceId"] != "work" || got["sub2apiConcurrency"] != 4 || got["priority"] != 2 || got["rateMultiplier"] != 0.5 {
		t.Fatalf("CLI runner options changed: %#v", got)
	}
}
func TestCLICheckpointAndRefreshAccountMatching(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "checkpoint.json")
	if err := protocol.WriteCheckpoint(path, protocol.Checkpoint{Version: 1, Stage: "email_verified", Email: "owner@example.test"}); err != nil {
		t.Fatal(err)
	}
	got, err := cliAccountEmail(config.CLIConfig{ResumeCheckpoint: path})
	if err != nil || got != "owner@example.test" {
		t.Fatalf("resume: %q %v", got, err)
	}
	if _, err = cliAccountEmail(config.CLIConfig{Email: "someone-else@example.test", ResumeCheckpoint: path}); err == nil {
		t.Fatal("cross-account checkpoint accepted")
	}
	source := filepath.Join(dir, "source.json")
	writeCLIRefreshFixture(t, source)
	if _, err = cliAccountEmail(config.CLIConfig{Email: "someone-else@example.test", RefreshSub2API: source}); err == nil {
		t.Fatal("cross-account refresh accepted")
	}
}
func TestCLIRefreshEndToEnd(t *testing.T) {
	t.Run("fingerprint-default", func(t *testing.T) { testCLIRefreshEndToEnd(t, false) })
	t.Run("native-explicit", func(t *testing.T) { testCLIRefreshEndToEnd(t, true) })
}
func testCLIRefreshEndToEnd(t *testing.T, native bool) {
	clearCLISensitiveEnvironment(t)
	var refreshCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if native && (r.UserAgent() != transport.NativeUserAgent || r.Header.Get("Sec-CH-UA") != "") {
			t.Error("explicit native requests falsely advertise Chromium")
		}
		if r.Method == http.MethodGet {
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.URL.Path != "/oauth/token" {
			http.NotFound(w, r)
			return
		}
		refreshCalls.Add(1)
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		if r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != "synthetic-refresh-old" {
			t.Error("CLI failed to send refresh grant")
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"access_token":"synthetic-access-new","refresh_token":"synthetic-refresh-new","expires_in":3600}`)
	}))
	defer server.Close()
	dir := t.TempDir()
	source := filepath.Join(dir, "source.json")
	target := filepath.Join(dir, "result.json")
	writeCLIRefreshFixture(t, source)
	args := []string{"--refresh-sub2api=" + source, "--sub2api-out=" + target, "--output-root=" + filepath.Join(dir, "jobs"), "--chatgpt-base=http://127.0.0.1:1", "--auth-base=" + server.URL, "--browser=false", "--verbose"}
	if native {
		args = append(args, "--native-http")
	}
	cfg, err := config.Parse(args)
	if err != nil {
		t.Fatal(err)
	}
	var out, logs bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	handled, err := runCLIWithIO(ctx, cfg, logging.NewWithWriter(&logs), strings.NewReader(""), &out)
	if err != nil || !handled {
		t.Fatalf("CLI refresh: handled=%v err=%v logs=%s", handled, err, logs.String())
	}
	if refreshCalls.Load() != 1 {
		t.Fatalf("refresh calls=%d", refreshCalls.Load())
	}
	b, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if err = json.Unmarshal(b, &result); err != nil {
		t.Fatal(err)
	}
	account := result["accounts"].([]any)[0].(map[string]any)
	creds := account["credentials"].(map[string]any)
	if result["custom"] != "preserve" || account["proxy_id"] != float64(7) || account["concurrency"] != float64(4) || account["priority"] != float64(8) || account["extra"].(map[string]any)["custom"] != "keep" || creds["custom"] != "keep" || creds["access_token"] != "synthetic-access-new" || creds["refresh_token"] != "synthetic-refresh-new" {
		t.Fatal("CLI refresh lost existing metadata or tokens")
	}
	if !strings.Contains(out.String(), "OAuth import saved") {
		t.Fatal("missing completion output")
	}
	for _, secret := range []string{"synthetic-refresh-old", "synthetic-refresh-new", "synthetic-access-new"} {
		if strings.Contains(out.String()+logs.String(), secret) {
			t.Fatal("CLI output leaked token")
		}
	}
	if runtime.GOOS != "windows" {
		info, _ := os.Stat(target)
		if info.Mode().Perm() != 0600 {
			t.Fatalf("private OAuth permissions %o", info.Mode().Perm())
		}
	}
	paths, _ := filepath.Glob(filepath.Join(cfg.OutputRoot, "*", "job-meta.json"))
	if len(paths) != 1 {
		t.Fatalf("isolated CLI metadata missing: %v", paths)
	}
	meta, _ := os.ReadFile(paths[0])
	if strings.Contains(string(meta), "synthetic-refresh") {
		t.Fatal("metadata leaked refresh token")
	}
	if _, err = os.Stat(filepath.Join(cfg.OutputRoot, "job-meta.json")); !os.IsNotExist(err) {
		t.Fatal("CLI replaced console metadata")
	}
}
func TestCLIRefreshCancellation(t *testing.T) {
	clearCLISensitiveEnvironment(t)
	requested := make(chan struct{})
	aborted := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.WriteHeader(200)
			return
		}
		io.Copy(io.Discard, r.Body)
		close(requested)
		<-r.Context().Done()
		close(aborted)
	}))
	defer server.Close()
	dir := t.TempDir()
	source := filepath.Join(dir, "source.json")
	writeCLIRefreshFixture(t, source)
	cfg, err := config.Parse([]string{"--refresh-sub2api=" + source, "--output-root=" + filepath.Join(dir, "jobs"), "--chatgpt-base=" + server.URL, "--auth-base=" + server.URL, "--browser=false"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := runCLIWithIO(ctx, cfg, nil, strings.NewReader(""), io.Discard); done <- err }()
	select {
	case <-requested:
	case <-time.After(5 * time.Second):
		t.Fatal("CLI never requested token refresh")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("CLI cancellation: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("CLI cancellation failed to stop task")
	}
	select {
	case <-aborted:
	case <-time.After(2 * time.Second):
		t.Fatal("token request context was not canceled")
	}
	original, _ := os.ReadFile(source)
	if !strings.Contains(string(original), "synthetic-refresh-old") {
		t.Fatal("cancellation destroyed source import")
	}
}
func writeCLIRefreshFixture(t *testing.T, path string) {
	t.Helper()
	jwt := "e30." + base64.RawURLEncoding.EncodeToString([]byte(`{"email":"owner@example.test","https://api.openai.com/auth":{"chatgpt_account_id":"fixture-account"}}`)) + ".x"
	value := map[string]any{"version": 1, "type": "sub2api-data", "custom": "preserve", "proxies": []any{}, "accounts": []any{map[string]any{"name": "original-name", "proxy_id": 7, "concurrency": 4, "priority": 8, "rate_multiplier": 0.7, "extra": map[string]any{"custom": "keep"}, "credentials": map[string]any{"email": "owner@example.test", "access_token": "synthetic-access-old", "refresh_token": "synthetic-refresh-old", "id_token": jwt, "custom": "keep"}}}}
	if err := protocol.WritePrivateJSON(path, value); err != nil {
		t.Fatal(err)
	}
}
func clearCLISensitiveEnvironment(t *testing.T) {
	t.Helper()
	for _, key := range []string{"CHATGPT_LOGIN_PASSWORD", "CHATGPT_TOTP_SECRET", "CHATGPT_NEW_PASSWORD", "CHATGPT_MAIL_API_URL", "CHATGPT_PROXY_URL", "CHATGPT_PROXY_URLS"} {
		t.Setenv(key, "")
	}
}
