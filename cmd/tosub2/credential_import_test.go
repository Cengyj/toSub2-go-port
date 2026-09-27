package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"tosub2-go-port/internal/config"
	"tosub2-go-port/internal/credentials"
)

func credentialImportFixture(t *testing.T, data string) (config.Config, string) {
	t.Helper()
	dir := t.TempDir()
	file := filepath.Join(dir, "fixture-private-emails.txt")
	if err := os.WriteFile(file, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Parse([]string{"--import-legacy-credentials=" + filepath.Join(dir, "fixture-private-source"), "--credential-emails-file=" + file, "--output-root=" + filepath.Join(dir, "not-created")})
	if err != nil {
		t.Fatal(err)
	}
	return cfg, file
}

func TestCredentialEmailFileSupportsLegacyAndCurrentMetadata(t *testing.T) {
	for _, fixture := range []struct {
		name, content string
		want          []string
	}{
		{"text", "\xef\xbb\xbf Owner@Example.TEST \r\n\r\nsecond@example.test\nowner@example.test\n", []string{"owner@example.test", "second@example.test"}},
		{"legacy-job", `{"version":1,"email":"Owner@Example.TEST","status":"completed","prompt":"ignore@example.test","password":"never-read-fixture"}`, []string{"owner@example.test"}},
		{"current-jobs", `{"version":1,"jobs":[{"email":"Owner@Example.TEST"},{"email":"second@example.test"},{"email":"owner@example.test"}]}`, []string{"owner@example.test", "second@example.test"}},
		{"email-array", `["Owner@Example.TEST","second@example.test","owner@example.test"]`, []string{"owner@example.test", "second@example.test"}},
		{"job-array", `[{"email":"Owner@Example.TEST"},{"email":"owner@example.test"}]`, []string{"owner@example.test"}},
		{"json-email", `"Owner@Example.TEST"`, []string{"owner@example.test"}},
		{"legacy-local-punctuation", "{owner}+tag@example.test\n", []string{"{owner}+tag@example.test"}},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			_, file := credentialImportFixture(t, fixture.content)
			got, err := readCredentialImportEmails(file)
			if err != nil || !reflect.DeepEqual(got, fixture.want) {
				t.Fatalf("email selection changed: %v", err)
			}
			after, err := os.ReadFile(file)
			if err != nil || string(after) != fixture.content {
				t.Fatal("email source was changed")
			}
		})
	}
}

func TestCredentialEmailFileRejectsInvalidInputBeforeOpeningDestination(t *testing.T) {
	for _, fixture := range []struct{ name, content string }{
		{"empty", " \n\t"},
		{"display-name", "Fixture Private <owner@example.test>"},
		{"secret-source-line", "owner@example.test----fixture-private-password"},
		{"partial-invalid", "owner@example.test\nfixture-private-invalid"},
		{"control", "owner\x00@example.test"},
		{"utf8", "owner@ex\xffample.test"},
		{"local-domain", "owner@localhost"},
		{"leading-dot", ".owner@example.test"},
		{"double-dot", "ow..ner@example.test"},
		{"bad-domain", "owner@-example.test"},
		{"bad-tld", "owner@example.1"},
		{"too-long-local", strings.Repeat("a", 65) + "@example.test"},
		{"missing-email", `{"credentials":{"email":"owner@example.test"}}`},
		{"nonstring-email", `{"email":12}`},
		{"ambiguous", `{"email":"owner@example.test","jobs":[]}`},
		{"invalid-jobs", `{"jobs":{}}`},
		{"empty-array", "[]"},
		{"nested-array", `[["owner@example.test"]]`},
		{"malformed-json", `{"email":"owner@example.test"`},
		{"concatenated-json", `{"email":"owner@example.test"}{}`},
		{"scalar", "null"},
		{"too-many", strings.Repeat("owner@example.test\n", maxCredentialImportEmails+1)},
		{"too-large", strings.Repeat("x", maxCredentialEmailFileBytes+1)},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			cfg, _ := credentialImportFixture(t, fixture.content)
			called := false
			var out bytes.Buffer
			handled, err := runCredentialImportWithDependencies(context.Background(), cfg, &out, credentialImportDependencies{newStore: func(config.Config) (credentials.Store, error) {
				called = true
				return credentials.NewMemoryStore(), nil
			}, importCredentials: func(context.Context, credentials.LegacyImportOptions, credentials.Store) (credentials.LegacyImportResult, error) {
				return credentials.LegacyImportResult{}, nil
			}})
			if !handled || err == nil || called {
				t.Fatal("invalid email file opened the credential destination or fell through")
			}
			if strings.Contains(err.Error()+out.String(), "fixture-private") || strings.Contains(err.Error()+out.String(), "owner@example.test") || strings.Contains(err.Error()+out.String(), cfg.CredentialEmailsFile) {
				t.Fatal("invalid input escaped into diagnostic output")
			}
			assertImportCounterOutput(t, out.Bytes(), credentials.LegacyImportResult{})
		})
	}
}

func TestCredentialEmailFileRejectsNonregularOrMissingInputs(t *testing.T) {
	cfg, file := credentialImportFixture(t, "owner@example.test")
	for _, path := range []string{filepath.Dir(file), filepath.Join(filepath.Dir(file), "missing-private-input")} {
		if _, err := readCredentialImportEmails(path); err == nil || strings.Contains(err.Error(), path) {
			t.Fatal("unsafe file was accepted or exposed its name")
		}
	}
	link := filepath.Join(filepath.Dir(file), "email-link")
	if err := os.Symlink(file, link); err != nil {
		t.Skip("platform does not permit a test symlink")
	}
	if _, err := readCredentialImportEmails(link); err == nil {
		t.Fatal("symlink input silently changed source selection")
	}
	if _, err := os.Stat(cfg.OutputRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("file validation created a destination")
	}
}

func TestOfflineCredentialImportDispatchUsesOnlyInjectedImportAndCounts(t *testing.T) {
	cfg, _ := credentialImportFixture(t, "Owner@example.test\nsecond@example.test\nowner@example.test\nthird@example.test")
	dest := credentials.NewMemoryStore()
	want := credentials.LegacyImportResult{Requested: 3, Imported: 2, SkippedExisting: 1}
	stores, imports := 0, 0
	var out bytes.Buffer
	handled, err := runCredentialImportWithDependencies(context.Background(), cfg, &out, credentialImportDependencies{
		newStore: func(got config.Config) (credentials.Store, error) {
			stores++
			if got.OutputRoot != cfg.OutputRoot {
				t.Error("destination changed")
			}
			return dest, nil
		},
		importCredentials: func(ctx context.Context, opts credentials.LegacyImportOptions, store credentials.Store) (credentials.LegacyImportResult, error) {
			imports++
			if ctx.Err() != nil || opts.SourceRoot != cfg.ImportLegacyCredentials || store != dest || !reflect.DeepEqual(opts.Emails, []string{"owner@example.test", "second@example.test", "third@example.test"}) {
				t.Error("offline import selection changed")
			}
			return want, nil
		},
	})
	if err != nil || !handled || stores != 1 || imports != 1 {
		t.Fatalf("offline dispatch failed: %v", err)
	}
	assertImportCounterOutput(t, out.Bytes(), want)
	if _, err := os.Stat(cfg.OutputRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("offline dispatch created a job/console directory")
	}
}

func TestOfflineCredentialImportFailuresRemainNonzeroAndPrivate(t *testing.T) {
	for _, test := range []struct {
		name      string
		result    credentials.LegacyImportResult
		failure   error
		wantCause error
	}{
		{"partial", credentials.LegacyImportResult{Requested: 2, Imported: 1, Failed: 1}, errors.New("fixture-private-source owner@example.test fixture-private-secret"), nil},
		{"failed-counter", credentials.LegacyImportResult{Requested: 2, Imported: 1, Failed: 1}, nil, credentials.ErrLegacyImportIncomplete},
		{"missing-counter", credentials.LegacyImportResult{Requested: 2, Imported: 1, Missing: 1}, nil, credentials.ErrLegacyImportIncomplete},
		{"unfinished-counter", credentials.LegacyImportResult{Requested: 2, Imported: 1, Unprocessed: 1}, nil, credentials.ErrLegacyImportIncomplete},
		{"canceled", credentials.LegacyImportResult{Requested: 2, Imported: 1, Unprocessed: 1}, fmt.Errorf("private cause: %w", context.Canceled), context.Canceled},
		{"timeout", credentials.LegacyImportResult{Requested: 2, Unprocessed: 2}, context.DeadlineExceeded, context.DeadlineExceeded},
		{"unsupported", credentials.LegacyImportResult{Requested: 2, Unprocessed: 2}, credentials.ErrUnavailable, credentials.ErrUnavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg, _ := credentialImportFixture(t, "owner@example.test\nsecond@example.test")
			var out bytes.Buffer
			handled, err := runCredentialImportWithDependencies(context.Background(), cfg, &out, credentialImportDependencies{newStore: func(config.Config) (credentials.Store, error) { return credentials.NewMemoryStore(), nil }, importCredentials: func(context.Context, credentials.LegacyImportOptions, credentials.Store) (credentials.LegacyImportResult, error) {
				return test.result, test.failure
			}})
			if !handled || err == nil || (test.wantCause != nil && !errors.Is(err, test.wantCause)) {
				t.Fatalf("import failure lost exit cause: %v", err)
			}
			if strings.Contains(err.Error()+out.String(), "fixture-private") || strings.Contains(err.Error()+out.String(), "owner@example.test") {
				t.Fatal("import error exposed source or account")
			}
			assertImportCounterOutput(t, out.Bytes(), test.result)
		})
	}
}

func TestOfflineCredentialImportValidationAndCancellationDoNotTouchStore(t *testing.T) {
	cfg, _ := credentialImportFixture(t, "owner@example.test")
	for _, test := range []struct {
		name     string
		cfg      config.Config
		canceled bool
		handled  bool
	}{
		{"console", config.Config{}, false, false},
		{"help", func() config.Config { c := cfg; c.CLI.Help = true; return c }(), false, false},
		{"account-conflict", func() config.Config { c := cfg; c.CLI.Email = "owner@example.test"; return c }(), false, true},
		{"service-conflict", func() config.Config { c := cfg; c.WindowsService = true; return c }(), false, true},
		{"canceled", cfg, true, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if test.canceled {
				cancel()
			}
			called := false
			var out bytes.Buffer
			handled, err := runCredentialImportWithDependencies(ctx, test.cfg, &out, credentialImportDependencies{newStore: func(config.Config) (credentials.Store, error) {
				called = true
				return credentials.NewMemoryStore(), nil
			}, importCredentials: func(context.Context, credentials.LegacyImportOptions, credentials.Store) (credentials.LegacyImportResult, error) {
				called = true
				return credentials.LegacyImportResult{}, nil
			}})
			if called || handled != test.handled || (handled && err == nil) || (!handled && err != nil) {
				t.Fatal("offline mode validation started dependencies or fell through")
			}
			if !handled && out.Len() != 0 {
				t.Fatal("ordinary mode wrote import counters")
			}
		})
	}
}

func TestOfflineCredentialImportFactoryRequiresFrozenIsolatedNamespace(t *testing.T) {
	cfg := config.Config{OutputRoot: filepath.Join(t.TempDir(), "outputs")}
	t.Setenv("TOSUB2_MAC_CREDENTIAL_ROOT", "")
	for _, namespace := range []string{"", "com.local.chatgpt-onboarding.credentials", "fixture-private-invalid"} {
		t.Setenv("TOSUB2_MAC_KEYCHAIN_SERVICE", namespace)
		if _, err := newCredentialImportStore(cfg); err == nil || strings.Contains(err.Error(), "fixture-private") {
			t.Fatal("nonisolated namespace accepted or disclosed")
		}
	}
	if runtime.GOOS != "darwin" {
		return
	}
	root := filepath.Join(t.TempDir(), "private-credentials")
	t.Setenv("TOSUB2_MAC_KEYCHAIN_SERVICE", "com.local.chatgpt-onboarding.credentials.local.fixture-offline")
	t.Setenv("TOSUB2_MAC_CREDENTIAL_ROOT", root)
	store, err := newCredentialImportStore(cfg)
	secure, ok := store.(*credentials.SecureFileStore)
	if err != nil || !ok || secure.Root != root {
		t.Fatalf("isolated environment did not construct the destination: %v", err)
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("store construction accessed credential storage")
	}
}

func assertImportCounterOutput(t *testing.T, data []byte, want credentials.LegacyImportResult) {
	t.Helper()
	var got credentials.LegacyImportResult
	var fields map[string]any
	if json.Unmarshal(data, &got) != nil || json.Unmarshal(data, &fields) != nil || got != want || len(fields) != 6 {
		t.Fatal("offline command output contains something other than the six counters")
	}
	for _, key := range []string{"requested", "imported", "skippedExisting", "missing", "failed", "unprocessed"} {
		if _, ok := fields[key].(float64); !ok {
			t.Fatal("offline counter is missing or not numeric")
		}
	}
}

func TestOfflineCredentialImportProcessHelper(t *testing.T) {
	if os.Getenv("TOSUB2_OFFLINE_IMPORT_PROCESS_FIXTURE") != "1" {
		t.Skip("test-only subprocess entry")
	}
	var args []string
	if json.Unmarshal([]byte(os.Getenv("TOSUB2_OFFLINE_IMPORT_ARGS")), &args) != nil {
		os.Exit(99)
	}
	os.Args = append([]string{os.Args[0]}, args...)
	main()
	os.Exit(0)
}

func TestOfflineCredentialImportProcessExitsBeforeConsoleAndJobRunner(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	dir := t.TempDir()
	root := filepath.Join(dir, "must-not-create-jobs")
	args := []string{"--import-legacy-credentials=" + filepath.Join(dir, "fixture-private-source"), "--credential-emails-file=" + filepath.Join(dir, "fixture-private-missing-list"), "--output-root=" + root}
	encoded, _ := json.Marshal(args)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestOfflineCredentialImportProcessHelper$")
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + dir, "TOSUB2_OFFLINE_IMPORT_PROCESS_FIXTURE=1", "TOSUB2_OFFLINE_IMPORT_ARGS=" + string(encoded), "ONBOARDING_HOST=127.0.0.1", "ONBOARDING_PORT=" + strconv.Itoa(listener.Addr().(*net.TCPAddr).Port), "TOSUB2_BROWSER_ENABLED=1", "CHROME_PATH=" + filepath.Join(dir, "must-not-start-chrome"), "CHATGPT_BASE=http://127.0.0.1:1", "AUTH_BASE=http://127.0.0.1:1"}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err = cmd.Run()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 || ctx.Err() != nil {
		t.Fatalf("offline failure did not exit immediately with code 1: %v", err)
	}
	assertImportCounterOutput(t, stdout.Bytes(), credentials.LegacyImportResult{})
	if !strings.Contains(stderr.String(), "credential selection file") || strings.Contains(stderr.String(), "HTTP listener") || strings.Contains(stderr.String(), "fixture-private") || strings.Contains(stderr.String(), dir) {
		t.Fatalf("offline diagnostic check: safeCategory=%t service=%t privateMarker=%t path=%t", strings.Contains(stderr.String(), "credential selection file"), strings.Contains(stderr.String(), "HTTP listener"), strings.Contains(stderr.String(), "fixture-private"), strings.Contains(stderr.String(), dir))
	}
	if _, err = os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("main created a job store before offline import")
	}
}

func TestOfflineCredentialImportOutputWriteFailureIsNonzero(t *testing.T) {
	cfg, _ := credentialImportFixture(t, "owner@example.test")
	_, err := runCredentialImportWithDependencies(context.Background(), cfg, importCounterErrorWriter{}, credentialImportDependencies{newStore: func(config.Config) (credentials.Store, error) { return nil, errors.New("fixture-private") }})
	if err == nil || strings.Contains(err.Error(), "fixture-private") {
		t.Fatal("writer failure was hidden or leaked")
	}
}

type importCounterErrorWriter struct{}

func (importCounterErrorWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }
