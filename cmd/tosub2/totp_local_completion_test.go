package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"tosub2-go-port/internal/accountlock"
	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/privatestate"
	"tosub2-go-port/internal/protocol"
)

func TestTOTPRemovedRemoteOperationsNeverCreateTransport(t *testing.T) {
	clearCLISensitiveEnvironment(t)
	for _, operation := range []string{"inspect-2fa", "recover-2fa", "reauthenticate", " REAUTHENTICATE ", "inspect_2fa", "recover_2fa", "inspect-totp", "recover-totp", "totp_recover", "totp_reset", "reset_totp", "reset-2fa-recovery", "setup-2fa-recover"} {
		t.Run(operation, func(t *testing.T) {
			var requests atomic.Int32
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); w.WriteHeader(http.StatusForbidden) }))
			defer remote.Close()
			cfg := sessionAuditConfig(t.TempDir(), remote.URL)
			vault := &resetRunnerVault{MemoryStore: credentials.NewMemoryStore()}
			m, _, logger := sessionAuditManager(t, cfg, vault)
			job, err := m.Submit(jobs.Spec{ID: sessionAuditJobID, Email: sessionAuditEmail, ImportOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			job.Operation = operation
			err = makeJobRunnerWithCredentials(cfg, m, logger, vault, true)(t.Context(), job, func(jobs.JobEvent) {})
			if err == nil || !strings.Contains(err.Error(), "TOTP_OPERATION_REMOVED") && !strings.Contains(err.Error(), "TOTP_OPERATION_UNSUPPORTED") || requests.Load() != 0 {
				t.Fatalf("removed operation reached a transport: count=%d err=%v", requests.Load(), err)
			}
		})
	}
}

func TestTOTPCompletionFailsClosedForUnknownOrConflictingEvidence(t *testing.T) {
	for _, scenario := range []string{"unknown", "missing-material", "different-account", "different-operation", "different-factor", "missing-pending-key", "different-pending-key", "different-pending-uri"} {
		t.Run(scenario, func(t *testing.T) {
			cfg := sessionAuditConfig(t.TempDir(), "http://127.0.0.1:1")
			vault := &resetRunnerVault{MemoryStore: credentials.NewMemoryStore()}
			m, _, _ := sessionAuditManager(t, cfg, vault)
			job, err := m.Submit(jobs.Spec{ID: sessionAuditJobID, Email: sessionAuditEmail, ImportOnly: true, Input: map[string]any{"totpSecret": sessionAuditOld}})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = m.QueueOperation(job.ID, "reset-2fa", nil); err != nil {
				t.Fatal(err)
			}
			runnerResetIntentJournal(t, cfg.OutputRoot, job.ID, job.Email, "completion-operation")
			if err = m.BeginTOTPReset(t.Context(), job.ID, "completion-operation"); err != nil {
				t.Fatal(err)
			}
			path := runnerCompletionJournal(t, cfg.OutputRoot, job.ID, job.Email, "completion-operation", sessionAuditNew, "not_requested")
			record, err := protocol.ReadTOTPResetJournal(path)
			if err != nil {
				t.Fatal(err)
			}
			record["credential_persisted"], record["reset_totp_phase"] = false, "activation_verified"
			current, _ := vault.Load(job.Email)
			current.PendingTOTPSecret = sessionAuditNew
			switch scenario {
			case "unknown":
				record["activation_verified"] = false
			case "missing-material":
				delete(record, "session_id")
			case "different-account":
				record["email"] = "another@example.test"
			case "different-operation":
				record["operation_id"] = "another-operation"
			case "different-factor":
				record["new_totp_factor_id"] = "another-factor"
			case "missing-pending-key":
				current.PendingTOTPSecret = ""
			case "different-pending-key":
				current.PendingTOTPSecret = sessionAuditOld
			case "different-pending-uri":
				current.PendingTOTPURI = "otpauth://totp/unrelated"
			}
			if err := protocol.WritePrivateJSON(path, record); err != nil {
				t.Fatal(err)
			}
			if err := vault.Save(job.Email, current); err != nil {
				t.Fatal(err)
			}
			job, _ = m.Get(job.ID)
			if err := finalizeTOTPResetLocally(context.Background(), path, m, vault, job); err == nil {
				t.Fatal("invalid local completion was accepted")
			}
			after, _ := vault.Load(job.Email)
			if !reflect.DeepEqual(current, after) {
				t.Fatal("invalid evidence changed protected credentials")
			}
		})
	}
}

func TestTOTPCompletionPreservesLegacyJournalWithoutNetwork(t *testing.T) {
	clearCLISensitiveEnvironment(t)
	remote := &sessionAuditRemote{t: t, sessionValid: true, factor: "old-factor"}
	server := httptest.NewServer(http.HandlerFunc(remote.serveHTTP))
	defer server.Close()
	cfg := sessionAuditConfig(t.TempDir(), server.URL)
	vault := &resetRunnerVault{MemoryStore: credentials.NewMemoryStore()}
	vault.failCommit.Store(true)
	m, _, logger := sessionAuditManager(t, cfg, vault)
	sessionAuditPrepare(t, cfg, m)
	if err := sessionAuditExecute(t, cfg, m, logger, vault); err == nil {
		t.Fatal("injected vault failure did not stop local completion")
	}
	path := filepath.Join(cfg.OutputRoot, sessionAuditJobID, "totp-setup-result.json")
	record, err := protocol.ReadTOTPResetJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	delete(record, "operation_scope")
	record["recovery_schema"], record["recovery_checkpoint"] = 1, map[string]any{"stage": "email_verified", "cookies": []any{map[string]any{"name": "old", "value": "synthetic-old"}}}
	operation, _ := record["operation_id"].(string)
	if err := privatestate.WriteJSON(t.Context(), path, "totp-reset", sessionAuditEmail, operation, record); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	calls := remote.snapshot()
	if _, err := m.QueueOperation(sessionAuditJobID, "complete-2fa", nil); err != nil {
		t.Fatal(err)
	}
	if err := sessionAuditWait(t, m); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("local completion rewrote legacy Cookie provenance", err)
	}
	if remote.snapshot() != calls {
		t.Fatal("legacy local completion contacted the remote service")
	}
	current, err := vault.Load(sessionAuditEmail)
	done, _ := m.Get(sessionAuditJobID)
	if err != nil || current.TOTPSecret != sessionAuditNew || current.TOTPResetPending || !done.TOTPResetCommitted || done.TOTPRecoveryPending {
		t.Fatal("legacy evidence did not permit reliable local key completion", err)
	}
}

func TestTOTPCompletionUsesConfiguredAccountLockWithoutNetwork(t *testing.T) {
	clearCLISensitiveEnvironment(t)
	remote := &sessionAuditRemote{t: t, sessionValid: true, factor: "old-factor"}
	server := httptest.NewServer(http.HandlerFunc(remote.serveHTTP))
	defer server.Close()
	cfg := sessionAuditConfig(t.TempDir(), server.URL)
	vault := &resetRunnerVault{MemoryStore: credentials.NewMemoryStore()}
	vault.failCommit.Store(true)
	m, _, logger := sessionAuditManager(t, cfg, vault)
	sessionAuditPrepare(t, cfg, m)
	if err := sessionAuditExecute(t, cfg, m, logger, vault); err == nil {
		t.Fatal("injected key save failure did not leave local completion pending")
	}
	calls := remote.snapshot()
	release, err := accountlock.Acquire(server.URL, sessionAuditEmail)
	if err != nil {
		t.Fatal("runner leaked its ordinary account lock", err)
	}
	if _, err := m.QueueOperation(sessionAuditJobID, "complete-2fa", nil); err != nil {
		_ = release()
		t.Fatal(err)
	}
	if err := sessionAuditWait(t, m); err == nil {
		_ = release()
		t.Fatal("local completion ignored the current account lock")
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
	wrongOrigin, err := accountlock.Acquire("https://chatgpt.com", sessionAuditEmail)
	if err != nil {
		t.Fatal(err)
	}
	defer wrongOrigin()
	if _, err := m.QueueOperation(sessionAuditJobID, "complete-2fa", nil); err != nil {
		t.Fatal(err)
	}
	if err := sessionAuditWait(t, m); err != nil {
		t.Fatal("local completion did not use the configured account origin", err)
	}
	if remote.snapshot() != calls {
		t.Fatal("local completion sent an authentication or confirmation request")
	}
	current, err := vault.Load(sessionAuditEmail)
	done, _ := m.Get(sessionAuditJobID)
	if err != nil || current.TOTPSecret != sessionAuditNew || current.TOTPResetPending || !done.TOTPResetCommitted {
		t.Fatal("confirmed key was not reliably completed", err)
	}
	assertNoAccountSessions(t, cfg.OutputRoot)
}

func TestTOTPNewResetArchivesWithConfiguredAccountLock(t *testing.T) {
	clearCLISensitiveEnvironment(t)
	remote := &sessionAuditRemote{t: t, sessionValid: true, factor: "old-factor"}
	server := httptest.NewServer(http.HandlerFunc(remote.serveHTTP))
	defer server.Close()
	cfg := sessionAuditConfig(t.TempDir(), server.URL)
	vault := &resetRunnerVault{MemoryStore: credentials.NewMemoryStore()}
	m, _, logger := sessionAuditManager(t, cfg, vault)
	sessionAuditPrepare(t, cfg, m)
	if err := sessionAuditExecute(t, cfg, m, logger, vault); err != nil {
		t.Fatal(err)
	}
	if err := m.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Local admission is tested without starting a second scheduler.
	restarted, _, _ := sessionAuditManager(t, cfg, vault)
	if err := restarted.Restore(t.Context()); err != nil {
		t.Fatal(err)
	}
	before := remote.snapshot()
	release, err := accountlock.Acquire(server.URL, sessionAuditEmail)
	if err != nil {
		t.Fatal(err)
	}
	_, admissionErr := restarted.QueueOperation(sessionAuditJobID, "reset-2fa", map[string]any{"totpIntentId": "explicit-next-reset-intent"})
	if err := release(); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(admissionErr, accountlock.ErrBusy) {
		t.Fatal("new reset archive ignored the account operation lock", admissionErr)
	}
	wrongOrigin, err := accountlock.Acquire("https://chatgpt.com", sessionAuditEmail)
	if err != nil {
		t.Fatal(err)
	}
	defer wrongOrigin()
	if _, err := restarted.QueueOperation(sessionAuditJobID, "reset-2fa", map[string]any{"totpIntentId": "explicit-next-reset-intent"}); err != nil {
		t.Fatal("new reset archive did not use the configured account origin", err)
	}
	if remote.snapshot() != before {
		t.Fatal("local archive made a remote request")
	}
	archives, err := filepath.Glob(filepath.Join(cfg.OutputRoot, sessionAuditJobID, "totp-completed-*.json"))
	if err != nil || len(archives) != 1 {
		t.Fatal("completed evidence was not archived exactly once", err)
	}
	if _, err := protocol.ReadTOTPResetCompletion(archives[0], sessionAuditEmail); err != nil {
		t.Fatal("archived evidence failed readback", err)
	}
	assertNoAccountSessions(t, cfg.OutputRoot)
}
