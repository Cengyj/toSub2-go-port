//go:build linux

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
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
	"tosub2-go-port/internal/privatestate"
	"tosub2-go-port/internal/protocol"
	"tosub2-go-port/internal/store"
)

func linuxPersistenceConfig(t *testing.T, base string) config.Config {
	t.Helper()
	root := t.TempDir()
	cfg := sessionAuditConfig(filepath.Join(root, "data"), base)
	keyPath := filepath.Join(root, "keys", "master.key")
	if err := credentials.InitializeFileKey(keyPath, filepath.Join(cfg.OutputRoot, "credentials")); err != nil {
		t.Fatal("initialize isolated credential key:", err)
	}
	t.Setenv("TOSUB2_CREDENTIAL_KEY_FILE", keyPath)
	return cfg
}

// Use the production factory rather than injecting a vault or setting its
// durable flag. This catches the Linux constructor's former memory fallback.
func linuxPersistenceManager(t *testing.T, cfg config.Config) (*jobs.Manager, jobs.Runner) {
	t.Helper()
	st, err := store.New(cfg.OutputRoot)
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	m := jobs.NewManager(t.Context(), 1, st, logger)
	configureAccountOperationLease(cfg, m)
	runner, err := makeJobRunner(cfg, m, logger)
	if err != nil {
		t.Fatal("construct production runner:", err)
	}
	m.SetRunner(runner)
	t.Cleanup(func() { _ = m.Shutdown(context.Background()) })
	return m, runner
}

func linuxPersistenceVault(t *testing.T, cfg config.Config) *credentials.SecureFileStore {
	t.Helper()
	vault, err := credentials.NewSecureFileStore(filepath.Join(cfg.OutputRoot, "credentials"))
	if err != nil {
		t.Fatal(err)
	}
	return vault
}

func linuxPersistenceSealedJournal(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var envelope struct {
		Format string `json:"private_state"`
	}
	if json.Unmarshal(raw, &envelope) != nil || envelope.Format != privatestate.Format {
		return errors.New("TOTP journal was not sealed by the production state protector")
	}
	for _, value := range []string{sessionAuditOld, sessionAuditNew, sessionAuditCookie, "synthetic-password", "otpauth://"} {
		if bytes.Contains(raw, []byte(value)) {
			return errors.New("TOTP journal contains plaintext private material")
		}
	}
	return nil
}

func linuxPersistenceChild(t *testing.T, cfg config.Config, mode string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestLinuxPersistenceProcessHelper$", "-test.timeout=25s")
	child.Env = append(os.Environ(), "TOSUB2_LINUX_PERSISTENCE_HELPER="+mode,
		"TOSUB2_LINUX_PERSISTENCE_ROOT="+cfg.OutputRoot, "TOSUB2_LINUX_PERSISTENCE_BASE="+cfg.ChatGPTBase)
	if output, err := child.CombinedOutput(); err != nil {
		t.Fatalf("fresh process %s failed: %v\n%s", mode, err, output)
	}
}

func TestLinuxPersistenceProductionTOTP(t *testing.T) {
	clearCLISensitiveEnvironment(t)
	for _, operation := range []string{"reset-2fa", "setup-2fa"} {
		t.Run(operation, func(t *testing.T) {
			factor := "old-factor"
			if operation == "setup-2fa" {
				factor = ""
			}
			remote := &sessionAuditRemote{t: t, sessionValid: true, factor: factor}
			var cfg config.Config
			var vault *credentials.SecureFileStore
			var protectedActivation atomic.Bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/backend-api/accounts/mfa/user/activate_enrollment" {
					pending, err := vault.Load(sessionAuditEmail)
					if err != nil || pending.PendingTOTPSecret != sessionAuditNew {
						t.Error("activation preceded durable pending credential readback")
					} else if err := linuxPersistenceSealedJournal(filepath.Join(cfg.OutputRoot, sessionAuditJobID, "totp-setup-result.json")); err != nil {
						t.Error(err)
					} else {
						protectedActivation.Store(true)
					}
				}
				remote.serveHTTP(w, r)
			}))
			defer server.Close()
			cfg = linuxPersistenceConfig(t, server.URL)
			m, _ := linuxPersistenceManager(t, cfg)
			vault = linuxPersistenceVault(t, cfg)
			sessionAuditImport(t, cfg, m)
			if _, err := m.QueueOperation(sessionAuditJobID, operation, nil); err != nil {
				t.Fatal(err)
			}
			m.Start()
			if err := sessionAuditWait(t, m); err != nil {
				t.Fatal("production Linux TOTP operation failed:", err)
			}
			if !protectedActivation.Load() {
				t.Fatal("activation never observed durable encrypted recovery material")
			}
			state := remote.snapshot()
			wantDisable := 0
			if operation == "reset-2fa" {
				wantDisable = 1
				sessionAuditAssertCommitted(t, cfg, m, vault, remote)
			}
			if state.disableCalls != wantDisable || state.enrollCalls != 1 || state.activateCalls != 1 || state.loginRequests != 1 {
				t.Fatal("unexpected authentication or repeated remote mutation", state)
			}
			if err := m.Shutdown(context.Background()); err != nil {
				t.Fatal(err)
			}
			linuxPersistenceChild(t, cfg, "verify-"+operation)
			if remote.snapshot() != state {
				t.Fatal("restarting the manager repeated a remote operation")
			}
		})
	}
}

func TestLinuxPersistenceLocalCompletionAfterProcessRestart(t *testing.T) {
	clearCLISensitiveEnvironment(t)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Error(w, "local completion must not contact this endpoint", http.StatusInternalServerError)
	}))
	defer server.Close()
	cfg := linuxPersistenceConfig(t, server.URL)
	m, _ := linuxPersistenceManager(t, cfg)
	vault := linuxPersistenceVault(t, cfg)
	job := sessionAuditPrepare(t, cfg, m)
	const operation = "linux-completion-operation"
	runnerResetIntentJournal(t, cfg.OutputRoot, job.ID, job.Email, operation)
	intentPath := filepath.Join(cfg.OutputRoot, job.ID, "totp-setup-result.json")
	intent, err := protocol.ReadTOTPResetJournal(intentPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := privatestate.WriteJSON(t.Context(), intentPath, "totp-reset", job.Email, operation, intent); err != nil {
		t.Fatal(err)
	}
	if err := m.BeginTOTPReset(t.Context(), job.ID, operation); err != nil {
		t.Fatal(err)
	}
	path := runnerCompletionJournal(t, cfg.OutputRoot, job.ID, job.Email, operation, sessionAuditNew, "not_requested")
	journal, err := protocol.ReadTOTPResetJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	journal["credential_persisted"], journal["reset_totp_phase"] = false, "activation_verified"
	if err := privatestate.WriteJSON(t.Context(), path, "totp-reset", job.Email, operation, journal); err != nil {
		t.Fatal(err)
	}
	pending, err := vault.Load(job.Email)
	if err != nil {
		t.Fatal(err)
	}
	pending.PendingTOTPSecret = sessionAuditNew
	if err := vault.Save(job.Email, pending); err != nil {
		t.Fatal(err)
	}
	if err := linuxPersistenceSealedJournal(path); err != nil {
		t.Fatal(err)
	}
	if err := m.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	linuxPersistenceChild(t, cfg, "complete")
	linuxPersistenceChild(t, cfg, "verify-reset-2fa")
	if requests.Load() != 0 {
		t.Fatal("local completion or restore contacted the remote service")
	}
}

func TestLinuxPersistenceSetupAlreadyEnabledPreservesFactor(t *testing.T) {
	clearCLISensitiveEnvironment(t)
	remote := &sessionAuditRemote{t: t, sessionValid: true, factor: "old-factor"}
	server := httptest.NewServer(http.HandlerFunc(remote.serveHTTP))
	defer server.Close()
	cfg := linuxPersistenceConfig(t, server.URL)
	m, _ := linuxPersistenceManager(t, cfg)
	sessionAuditImport(t, cfg, m)
	if _, err := m.QueueOperation(sessionAuditJobID, "setup-2fa", nil); err != nil {
		t.Fatal(err)
	}
	m.Start()
	if err := sessionAuditWait(t, m); err != nil {
		t.Fatal("already-enabled setup could not save its protected result:", err)
	}
	saved, err := linuxPersistenceVault(t, cfg).Load(sessionAuditEmail)
	if err != nil || saved.TOTPSecret != sessionAuditOld || saved.PendingTOTPSecret != "" || saved.TOTPResetPending {
		t.Fatal("already-enabled setup changed existing credentials", err)
	}
	state := remote.snapshot()
	if state.factor != "old-factor" || state.disableCalls != 0 || state.enrollCalls != 0 || state.activateCalls != 0 || state.loginRequests != 1 {
		t.Fatal("already-enabled setup changed the remote factor", state)
	}
}

// A child process inherits only private file paths and normal environment,
// never an in-memory vault, protector key, or live authentication session.
func TestLinuxPersistenceProcessHelper(t *testing.T) {
	mode := os.Getenv("TOSUB2_LINUX_PERSISTENCE_HELPER")
	if mode == "" {
		return
	}
	root, base := os.Getenv("TOSUB2_LINUX_PERSISTENCE_ROOT"), os.Getenv("TOSUB2_LINUX_PERSISTENCE_BASE")
	if root == "" || base == "" || mode != "complete" && mode != "verify-reset-2fa" && mode != "verify-setup-2fa" {
		t.Fatal("invalid isolated process fixture")
	}
	cfg := sessionAuditConfig(root, base)
	// Confirm restore and local completion never construct an authentication
	// transport even when its configuration would be invalid.
	cfg.TLSProfile = "invalid-for-local-completion"
	m, _ := linuxPersistenceManager(t, cfg)
	if err := m.Restore(t.Context()); err != nil {
		t.Fatal("restore encrypted operation evidence:", err)
	}
	vault := linuxPersistenceVault(t, cfg)
	if mode == "complete" {
		pending, err := vault.Load(sessionAuditEmail)
		job, ok := m.Get(sessionAuditJobID)
		if err != nil || !ok || !pending.TOTPResetPending || pending.PendingTOTPSecret != sessionAuditNew || pending.TOTPSecret != sessionAuditOld || !job.TOTPRecoveryPending {
			t.Fatal("fresh process did not retain pending activated credentials")
		}
		if _, err := m.QueueOperation(sessionAuditJobID, "complete-2fa", nil); err != nil {
			t.Fatal("queue explicit local completion:", err)
		}
		m.Start()
		if err := sessionAuditWait(t, m); err != nil {
			t.Fatal("complete encrypted recovery in a fresh process:", err)
		}
	}
	saved, err := vault.Load(sessionAuditEmail)
	job, ok := m.Get(sessionAuditJobID)
	if err != nil || !ok || saved.TOTPSecret != sessionAuditNew || saved.PendingTOTPSecret != "" || saved.PendingTOTPURI != "" || saved.TOTPResetPending || !job.HasTOTPKey || job.TOTPRecoveryPending {
		t.Fatal("persisted factor was not available after process restart")
	}
	if mode != "verify-setup-2fa" {
		path := filepath.Join(root, sessionAuditJobID, "totp-setup-result.json")
		journal, err := protocol.ReadTOTPResetCompletion(path, sessionAuditEmail)
		if err != nil || !job.TOTPResetCommitted || journal["credential_persisted"] != true || journal["operation_id"] != saved.TOTPResetOperationID {
			t.Fatal("fresh process lost durable completion evidence", err)
		}
		if err := linuxPersistenceSealedJournal(path); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLinuxPersistenceMissingKeyFailsBeforeNetwork(t *testing.T) {
	clearCLISensitiveEnvironment(t)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	for _, configured := range []bool{false, true} {
		t.Run(map[bool]string{false: "unconfigured", true: "missing-configured-key"}[configured], func(t *testing.T) {
			cfg := sessionAuditConfig(t.TempDir(), server.URL)
			keyPath := ""
			if configured {
				keyPath = filepath.Join(t.TempDir(), "missing.key")
			}
			t.Setenv("TOSUB2_CREDENTIAL_KEY_FILE", keyPath)
			st, err := store.New(cfg.OutputRoot)
			if err != nil {
				t.Fatal(err)
			}
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			m := jobs.NewManager(t.Context(), 1, st, logger)
			defer m.Shutdown(context.Background())
			runner, err := makeJobRunner(cfg, m, logger)
			if configured {
				if err == nil || runner != nil {
					t.Fatal("invalid configured key silently fell back to memory")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			job := sessionAuditPrepare(t, cfg, m)
			if err := runner(t.Context(), job, func(jobs.JobEvent) {}); err == nil || !strings.Contains(err.Error(), "TOTP_RESET_PERSISTENCE_REQUIRED") {
				t.Fatal("unconfigured memory store bypassed the durable reset guard", err)
			}
		})
	}
	if requests.Load() != 0 {
		t.Fatal("missing credential key allowed a network request")
	}
}

func TestLinuxPersistenceConsoleRejectsInvalidKey(t *testing.T) {
	clearCLISensitiveEnvironment(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	cfg := sessionAuditConfig(t.TempDir(), "http://127.0.0.1:1")
	cfg.Host, cfg.Port = "127.0.0.1", port
	t.Setenv("TOSUB2_CREDENTIAL_KEY_FILE", filepath.Join(t.TempDir(), "missing.key"))
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	var ready bool
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	err = runConsole(ctx, cfg, logger, func() {
		ready = true
		cancel()
	})
	if err == nil || ready || !strings.Contains(err.Error(), "credential") {
		t.Fatal("console admitted requests with an invalid configured credential key", err)
	}
	listener, err = net.Listen("tcp", cfg.Addr())
	if err != nil {
		t.Fatal("failed console startup retained its listener:", err)
	}
	_ = listener.Close()
}

func TestLinuxPersistenceCLIConfiguredKeyValidatesBeforeNetwork(t *testing.T) {
	clearCLISensitiveEnvironment(t)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	cfg := sessionAuditConfig(t.TempDir(), server.URL)
	parsed, err := config.Parse([]string{"--email=" + sessionAuditEmail, "--setup-totp"})
	if err != nil {
		t.Fatal(err)
	}
	cfg.CLI = parsed.CLI
	t.Setenv("TOSUB2_CREDENTIAL_KEY_FILE", filepath.Join(t.TempDir(), "missing.key"))
	// The platform check may admit configured Linux storage, but the real
	// constructor still has to validate it before any authentication work.
	if err := validateCLICredentialDelivery(cfg.CLI, "linux", ""); err != nil {
		t.Fatal("CLI ignored configured Linux credential storage:", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	handled, err := runCLIWithIO(ctx, cfg, logger, strings.NewReader(""), io.Discard)
	if !handled || err == nil || !strings.Contains(err.Error(), "credential") || requests.Load() != 0 {
		t.Fatal("CLI accepted an invalid configured key or contacted the remote service", err)
	}
}
