package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/protocol"
)

func TestManualSetupActivationDoesNotCommitUnverifiedSeedOrReplayAtRestart(t *testing.T) {
	clearCLISensitiveEnvironment(t)
	remote := &sessionAuditRemote{t: t, sessionValid: true}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/backend-api/accounts/mfa/user/activate_enrollment" {
			remote.serveHTTP(w, r)
			return
		}
		remote.mu.Lock()
		defer remote.mu.Unlock()
		remote.requests++
		remote.activateCalls++
		var input map[string]any
		cookie, cookieErr := r.Cookie("audit-session-token")
		if cookieErr != nil || cookie.Value != sessionAuditCookie || !remote.enrollmentExists || json.NewDecoder(r.Body).Decode(&input) != nil || input["session_id"] != "synthetic-enrollment-session" {
			t.Error("activation lost this operation's session/enrollment binding")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if remote.activateCalls == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": "invalid_code"}})
			return
		}
		if remote.activateCalls != 2 || input["code"] != "123456" {
			t.Error("unexpected manual activation or replay")
		}
		remote.factor = "new-factor"
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true})
	}))
	defer server.Close()
	cfg := sessionAuditConfig(t.TempDir(), server.URL)
	vault := credentials.NewMemoryStore()
	m, _, logger := sessionAuditManager(t, cfg, vault)
	sessionAuditImport(t, cfg, m)
	if _, err := m.QueueOperation(sessionAuditJobID, "setup-2fa", nil); err != nil {
		t.Fatal(err)
	}
	events, unsubscribe := m.Subscribe(128)
	defer unsubscribe()
	inputDone := make(chan bool, 1)
	go func() {
		for event := range events {
			if (event.Stage == "totp_setup" || event.Stage == "totp_setup_otp") && event.Status == jobs.StatusWaitingInput {
				inputDone <- m.InputForStage(sessionAuditJobID, "totp_setup_otp", map[string]any{"totp_setup_otp": "123456"})
				return
			}
		}
	}()
	err := sessionAuditExecute(t, cfg, m, logger, vault)
	if err == nil || !strings.Contains(err.Error(), "TOTP_SETUP_SEED_UNVERIFIED") {
		t.Fatal("unverified seed reported local success", err)
	}
	select {
	case accepted := <-inputDone:
		if !accepted {
			t.Fatal("manual activation input was rejected")
		}
	default:
		t.Fatal("manual activation was not requested")
	}
	state := remote.snapshot()
	if state.factor != "new-factor" || state.enrollCalls != 1 || state.activateCalls != 2 || state.disableCalls != 0 {
		t.Fatalf("wrong mutation sequence: %+v", state)
	}
	check := func(manager *jobs.Manager) {
		t.Helper()
		saved, err := vault.Load(sessionAuditEmail)
		job, ok := manager.Get(sessionAuditJobID)
		if err != nil || !ok || saved.TOTPSecret != sessionAuditOld || saved.PendingTOTPSecret != sessionAuditNew || saved.PendingTOTPFactorID != "new-factor" || !job.TOTPKnownEnabled || job.HasTOTPKey || job.TOTPSetupError == "" {
			t.Fatal("remote activation was conflated with saved-seed verification", err)
		}
		if _, err := manager.LoadCredentials(sessionAuditJobID); !errors.Is(err, jobs.ErrTOTPSetupVerificationPending) {
			t.Fatal("private view exposed a historical seed as current", err)
		}
	}
	check(m)
	path := filepath.Join(cfg.OutputRoot, sessionAuditJobID, "totp-setup-result.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	record, err := protocol.ReadTOTPResetJournal(path)
	if err != nil || record["activation_succeeded"] != true || protocol.SetupTOTPSeedVerified(record) || record["activation_mode"] != "manual" {
		t.Fatal("private evidence lost independent activation result", err)
	}
	if err := m.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	restored, _, _ := sessionAuditManager(t, cfg, vault)
	if err := restored.Restore(t.Context()); err != nil {
		t.Fatal(err)
	}
	check(restored)
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) || remote.snapshot() != state {
		t.Fatal("restart changed evidence or repeated a mutation", err)
	}
}
