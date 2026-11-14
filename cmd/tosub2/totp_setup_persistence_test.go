package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"tosub2-go-port/internal/config"
	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/logging"
	"tosub2-go-port/internal/store"
)

func TestTOTPSetupRejectsNonDurableCredentialsBeforeNetwork(t *testing.T) {
	for _, operation := range []string{"setup-2fa", "setup_2fa", "setup-totp", "totp_setup"} {
		t.Run(operation, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				http.Error(w, "unexpected request", http.StatusInternalServerError)
			}))
			defer server.Close()

			cfg := config.Default()
			cfg.OutputRoot = t.TempDir()
			cfg.ChatGPTBase = server.URL
			cfg.AuthBase = server.URL
			st, err := store.New(cfg.OutputRoot)
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			logger := logging.NewWithWriter(io.Discard)
			manager := jobs.NewManager(ctx, 1, st, logger)
			job, err := manager.Submit(jobs.Spec{ID: "setup", Email: "owner@example.test", Operation: operation})
			if err != nil {
				t.Fatal(err)
			}
			runner := makeJobRunnerWithCredentials(cfg, manager, logger, credentials.NewMemoryStore(), false)
			err = runner(ctx, job, func(jobs.JobEvent) {})
			if err == nil || !strings.Contains(err.Error(), "TOTP_SETUP_PERSISTENCE_REQUIRED") {
				t.Fatalf("expected persistence error, got %v", err)
			}
			if got := requests.Load(); got != 0 {
				t.Fatalf("sent %d requests before rejecting setup", got)
			}
		})
	}
}

type setupReadbackFaultVault struct {
	*credentials.MemoryStore
	phase  string
	faults atomic.Int32
}

func (v *setupReadbackFaultVault) Save(email string, value credentials.Credentials) error {
	if v.phase == "enrollment" && value.PendingTOTPSecret == sessionAuditNew && value.TOTPSecret != sessionAuditNew || v.phase == "activation" && value.TOTPSecret == sessionAuditNew && value.PendingTOTPSecret == "" {
		v.faults.Add(1)
		return nil // A successful write call must not substitute for exact readback.
	}
	return v.MemoryStore.Save(email, value)
}

func TestTOTPSetupReadbackFailureDoesNotReportCompletion(t *testing.T) {
	clearCLISensitiveEnvironment(t)
	for _, phase := range []string{"enrollment", "activation"} {
		t.Run(phase, func(t *testing.T) {
			remote := &sessionAuditRemote{t: t, sessionValid: true, factor: ""}
			server := httptest.NewServer(http.HandlerFunc(remote.serveHTTP))
			defer server.Close()
			cfg := sessionAuditConfig(t.TempDir(), server.URL)
			vault := &setupReadbackFaultVault{MemoryStore: credentials.NewMemoryStore(), phase: phase}
			m, _, logger := sessionAuditManager(t, cfg, vault)
			sessionAuditImport(t, cfg, m)
			if _, err := m.QueueOperation(sessionAuditJobID, "setup-2fa", nil); err != nil {
				t.Fatal(err)
			}
			if err := sessionAuditExecute(t, cfg, m, logger, vault); err == nil {
				t.Fatal("unconfirmed credential save reported success")
			}
			state := remote.snapshot()
			wantActivation := 0
			if phase == "activation" {
				wantActivation = 1
			}
			if vault.faults.Load() != 1 || state.disableCalls != 0 || state.enrollCalls != 1 || state.activateCalls != wantActivation {
				t.Fatal("persistence failure crossed or repeated mutation boundary", state)
			}
			job, _ := m.Get(sessionAuditJobID)
			if job.OperationStatus == "succeeded" {
				t.Fatal("readback failure marked setup complete")
			}
		})
	}
}

func TestTOTPSetupSuccessfulReceiptSavesWithoutConfirmationGET(t *testing.T) {
	clearCLISensitiveEnvironment(t)
	remote := &sessionAuditRemote{t: t, sessionValid: true, factor: ""}
	var redundantReads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/backend-api/accounts/mfa_info" && remote.snapshot().factor == "new-factor" {
			redundantReads.Add(1)
			http.Error(w, "synthetic post-activation status unavailable", http.StatusServiceUnavailable)
			return
		}
		remote.serveHTTP(w, r)
	}))
	defer server.Close()
	cfg := sessionAuditConfig(t.TempDir(), server.URL)
	vault := &setupReadbackFaultVault{MemoryStore: credentials.NewMemoryStore()}
	m, _, logger := sessionAuditManager(t, cfg, vault)
	sessionAuditImport(t, cfg, m)
	if _, err := m.QueueOperation(sessionAuditJobID, "setup-2fa", nil); err != nil {
		t.Fatal(err)
	}
	if err := sessionAuditExecute(t, cfg, m, logger, vault); err != nil {
		t.Fatal(err)
	}
	state := remote.snapshot()
	if redundantReads.Load() != 0 || state.infoReads != 1 || state.disableCalls != 0 || state.enrollCalls != 1 || state.activateCalls != 1 {
		t.Fatal("successful setup repeated a request or queried confirmation", state)
	}
	saved, err := vault.Load(sessionAuditEmail)
	if err != nil || saved.TOTPSecret != sessionAuditNew || saved.PendingTOTPSecret != "" || saved.PendingTOTPURI != "" {
		t.Fatal("successful setup did not persist the new key", err)
	}
}
