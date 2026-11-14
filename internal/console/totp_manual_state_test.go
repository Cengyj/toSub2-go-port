package console

import (
	"encoding/base64"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/protocol"
)

func recoveryAPIFixture(t *testing.T) (*Server, *jobs.Manager, *jobs.Job) {
	s, m, j, _ := recoveryAPIWithVault(t)
	return s, m, j
}

func recoveryAPIWithVault(t *testing.T) (*Server, *jobs.Manager, *jobs.Job, *monitorMemoryVault) {
	t.Helper()
	s, m := newMonitorTestServer(t)
	vault := &monitorMemoryVault{values: map[string]credentials.Credentials{}}
	m.SetCredentialStore(vault)
	j := importTOTPActionFixture(t, m, "recoverable-api", true)
	if _, err := m.QueueOperation(j.ID, "reset-2fa", nil); err != nil {
		t.Fatal(err)
	}
	if err := beginConsoleTOTPReset(t, s.cfg.OutputRoot, m, t.Context(), j.ID, "fixture-operation"); err != nil {
		t.Fatal(err)
	}
	journal := map[string]any{
		"version": 1, "operation_scope": "current_session", "email": j.Email, "operation_id": "fixture-operation", "account_id": "fixture-account",
		"reset_totp_requested": true, "old_factor_invalidated": true, "old_totp_factor_id": "fixture-old",
		"reset_totp_attempted_at": "2026-11-05T00:00:00Z", "enroll_attempted": false, "activation_attempted": false,
		"reset_totp_phase": "recovery_required", "reset_totp_last_phase": "disable_intent", "reset_totp_status": "unknown",
		"activation_succeeded": false, "activation_verified": false, "credential_persisted": false,
	}
	if err := protocol.WritePrivateJSON(filepath.Join(s.cfg.OutputRoot, j.ID, "totp-setup-result.json"), journal); err != nil {
		t.Fatal(err)
	}
	if err := m.UpdateTOTPResetResult(j.ID, journal, errors.New("synthetic lost response")); err != nil {
		t.Fatal(err)
	}
	if err := m.FinishTOTPWorkflow(j.ID, errors.New("synthetic stopped reset")); err != nil {
		t.Fatal(err)
	}
	m.Emit(jobs.JobEvent{JobID: j.ID, Status: jobs.StatusFailed, Stage: "totp_reset_disable"})
	m = restoreCompletedTOTPFixtureManager(t, s, vault)
	return s, m, j, vault
}

func TestTOTPPublicRecoveryStateOmitsPrivateReceiptsAndCookies(t *testing.T) {
	s, m, job := recoveryAPIFixture(t)
	path := filepath.Join(s.cfg.OutputRoot, job.ID, "totp-setup-result.json")
	journal, err := protocol.ReadTOTPResetJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	const privateBody = "private-response-receipt-canary private-enrollment-canary"
	encodedBody := base64.StdEncoding.EncodeToString([]byte(privateBody))
	journal["disable_attempt_id"] = "private-attempt-canary"
	journal["disable_receipt"] = map[string]any{"version": 1, "step": "disable", "operation_id": "fixture-operation", "attempt_id": "private-attempt-canary", "http_status": 200, "body_complete": true, "body_base64": encodedBody, "termination": "complete"}
	if err := protocol.WritePrivateJSON(path, journal); err != nil {
		t.Fatal(err)
	}
	if err := m.UpdateTOTPResetResult(job.ID, journal, nil); err != nil {
		t.Fatal(err)
	}
	privateValues := []string{"private-recovery-cookie-fixture", "private-response-receipt-canary", "private-enrollment-canary", "private-attempt-canary", encodedBody, "disable_receipt", "body_base64", "recovery_checkpoint"}
	for _, route := range []string{"/api/jobs/" + job.ID, "/api/jobs?pageSize=10"} {
		response := frontendRequest(s, http.MethodGet, route, nil, "test")
		if response.Code != http.StatusOK {
			t.Fatal("public recovery state unavailable", response.Code)
		}
		for _, private := range privateValues {
			if strings.Contains(response.Body.String(), private) {
				t.Fatal("public recovery DTO exposed a private cookie or response receipt", route)
			}
		}
	}
	public := completionPublicJob(t, s, job.ID)
	if public["totpRecoveryPending"] != true || public["canResetTotp"] != false {
		t.Fatal("public state hid unsaved data or released the recovery gate")
	}
	metadata, err := os.ReadFile(filepath.Join(s.cfg.OutputRoot, "job-meta.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range privateValues {
		if strings.Contains(string(metadata), private) {
			t.Fatal("public metadata exposed a private cookie or response receipt")
		}
	}
}

func preparedTOTPAPIFixture(t *testing.T) (*Server, *jobs.Manager, *jobs.Job) {
	t.Helper()
	s, m := newMonitorTestServer(t)
	vault := &monitorMemoryVault{values: map[string]credentials.Credentials{}}
	m.SetCredentialStore(vault)
	job := importTOTPActionFixture(t, m, "prepared-api", true)
	if _, err := m.QueueOperation(job.ID, "reset-2fa", nil); err != nil {
		t.Fatal(err)
	}
	registerCompletionFixture(t, s.cfg.OutputRoot)
	journal := map[string]any{
		"version": 1, "operation_scope": "current_session", "mutation_schema": 1, "email": job.Email, "operation_id": "prepared-operation", "account_id": "prepared-account",
		"reset_totp_requested": true, "old_factor_invalidated": false, "old_totp_factor_id": "prepared-old",
		"enroll_attempted": false, "activation_attempted": false, "disable_dispatch": "not_sent", "enroll_dispatch": "not_sent", "activation_dispatch": "not_sent",
		"reset_totp_phase": "prepared", "reset_totp_status": "failed",
		"activation_succeeded": false, "activation_verified": false, "credential_persisted": false,
	}
	if err := protocol.WritePrivateJSON(filepath.Join(s.cfg.OutputRoot, job.ID, "totp-setup-result.json"), journal); err != nil {
		t.Fatal(err)
	}
	if err := m.UpdateTOTPResetResult(job.ID, journal, errors.New("synthetic stopped preparation")); err != nil {
		t.Fatal(err)
	}
	if err := m.FinishTOTPWorkflow(job.ID, errors.New("synthetic stopped preparation")); err != nil {
		t.Fatal(err)
	}
	m.Emit(jobs.JobEvent{JobID: job.ID, Status: jobs.StatusFailed, Stage: "totp_reset_read"})
	m = restoreCompletedTOTPFixtureManager(t, s, vault)
	return s, m, job
}
