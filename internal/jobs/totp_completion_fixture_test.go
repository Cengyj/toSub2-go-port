package jobs

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/privatestate"
	"tosub2-go-port/internal/protocol"
)

const completionFixtureSeed = "JBSWY3DPEHPK3PXP"

var completionFixtureProtectors sync.Map

func registerCompletionFixture(t *testing.T, root string) {
	t.Helper()
	protector, err := credentials.NewMemoryStateProtector()
	if err != nil {
		t.Fatal(err)
	}
	actual, _ := completionFixtureProtectors.LoadOrStore(root, protector)
	if err := privatestate.Register(root, actual.(*credentials.MemoryStateProtector)); err != nil {
		t.Fatal(err)
	}
}

func beginTOTPResetFixture(t *testing.T, m *Manager, ctx context.Context, id string, operations ...string) error {
	t.Helper()
	operation := "fixture-op"
	if len(operations) > 0 {
		operation = operations[0]
	}
	j, _ := m.Get(id)
	registerCompletionFixture(t, m.store.Root)
	out := map[string]any{
		"version": 1, "operation_scope": "current_session", "email": j.Email, "operation_id": operation, "account_id": "fixture-account",
		"reset_totp_requested": true, "reset_totp_phase": "quarantine_intent", "old_totp_factor_id": "old-factor", "old_factor_invalidated": false,
		"mutation_schema": 1, "disable_dispatch": "not_sent", "enroll_dispatch": "not_sent", "activation_dispatch": "not_sent",
		"enroll_attempted": false, "activation_attempted": false, "activation_succeeded": false, "activation_verified": false, "credential_persisted": false,
	}
	if err := protocol.WritePrivateJSON(filepath.Join(m.store.Root, id, "totp-setup-result.json"), out); err != nil {
		t.Fatal(err)
	}
	return m.BeginTOTPReset(ctx, id, operation)
}

func completionFixtureResult(email, operation, secret, source string, persisted bool) map[string]any {
	material, _ := json.Marshal([]string{secret, "fixture-session", "fixture-new"})
	digest := sha256.Sum256(material)
	out := map[string]any{
		"version": 1, "operation_scope": "current_session", "email": email, "operation_id": operation, "account_id": "fixture-account",
		"reset_totp_requested": true, "reset_totp_status": "activated", "old_factor_invalidated": true, "old_totp_factor_id": "old-factor", "reset_totp_attempted_at": "2026-11-07T00:00:00Z",
		"enroll_attempted": true, "activation_attempted": true, "enrollment_validated": true, "secret": secret, "session_id": "fixture-session", "new_totp_factor_id": "fixture-new",
		"enrollment_factor":    map[string]any{"id": "fixture-new", "factor_type": "totp", "is_recovery": false},
		"activation_succeeded": true, "activation_verified": true, "activation_evidence_schema": 2, "activation_confirmation_source": source, "activation_attempt_id": "fixture-attempt",
		"credential_persisted": persisted, "reset_totp_phase": "activation_verified",
		"activation_evidence": map[string]any{"operation_id": operation, "account_id": "fixture-account", "email": email, "factor_id": "fixture-new", "enrollment_digest": hex.EncodeToString(digest[:]), "confirmed_at": "2026-11-07T00:00:00Z", "confirmation_source": source, "attempt_id": "fixture-attempt"},
	}
	if source == "state_read" {
		out["activation_confirmed_by_read"] = true
	} else {
		out["activation_acknowledged"] = true
		out["activation_receipt"] = map[string]any{"version": 1, "step": "activation", "operation_id": operation, "attempt_id": "fixture-attempt", "http_status": 200, "body_complete": true, "termination": "complete", "body_base64": base64.StdEncoding.EncodeToString([]byte("{\"success\":true}"))}
	}
	if persisted {
		out["reset_totp_phase"] = "credential_persisted"
	}
	return out
}

func commitTOTPResetFixture(t *testing.T, m *Manager, ctx context.Context, id string, c credentials.Credentials) error {
	t.Helper()
	j, _ := m.Get(id)
	out := completionFixtureResult(j.Email, c.TOTPResetOperationID, c.TOTPSecret, "business_receipt", false)
	if err := protocol.WritePrivateJSON(filepath.Join(m.store.Root, id, "totp-setup-result.json"), out); err != nil {
		t.Fatal(err)
	}
	return m.CommitTOTPReset(ctx, id, c)
}

func writeCommittedResetFixture(t *testing.T, m *Manager, id, operation string) {
	t.Helper()
	registerCompletionFixture(t, m.store.Root)
	j, _ := m.Get(id)
	c, err := m.vault.Load(j.Email)
	if err != nil {
		t.Fatal(err)
	}
	out := completionFixtureResult(j.Email, operation, c.TOTPSecret, "business_receipt", true)
	out["logout_all_devices_requested"], out["logout_all_devices_status"] = j.LogoutAllDevicesAfterTOTP, j.LogoutAllDevicesStatus
	path := filepath.Join(m.store.Root, id, "totp-setup-result.json")
	if err := protocol.WritePrivateJSON(path, out); err != nil {
		t.Fatal(err)
	}
	if _, err := protocol.ReadTOTPResetCompletion(path, j.Email); err != nil {
		t.Fatal(err)
	}
}
func markCommittedResetFixture(t *testing.T, m *Manager, id, operation string) error {
	t.Helper()
	writeCommittedResetFixture(t, m, id, operation)
	return m.MarkTOTPResetCommitted(t.Context(), id, operation)
}

func uncertainResetJournal(t *testing.T, m *Manager, j *Job) string {
	t.Helper()
	current, _ := m.Get(j.ID)
	out := map[string]any{"version": 1, "operation_scope": "current_session", "email": j.Email, "operation_id": current.TOTPResetOperationID, "account_id": "fixture-account", "reset_totp_requested": true, "old_factor_invalidated": true, "old_totp_factor_id": "old-factor", "reset_totp_attempted_at": "2026-11-07T00:00:00Z", "enroll_attempted": false, "activation_attempted": false, "reset_totp_phase": "manual_review_required", "reset_totp_last_phase": "disable_intent", "reset_totp_status": "unknown", "activation_succeeded": false, "activation_verified": false, "credential_persisted": false}
	path := filepath.Join(m.store.Root, j.ID, "totp-setup-result.json")
	if err := protocol.WritePrivateJSON(path, out); err != nil {
		t.Fatal(err)
	}
	if err := m.UpdateTOTPResetResult(j.ID, out, errors.New("synthetic lost response")); err != nil {
		t.Fatal(err)
	}
	return path
}
func pendingResetFixture(t *testing.T) (*Manager, *memoryVault, *Job, *Job, string) {
	t.Helper()
	m, v, j, p := resetManagerFixture(t)
	if _, err := m.QueueOperation(j.ID, "reset-2fa", map[string]any{"logoutAllDevicesAfterTotp": true, "reloginAfterTotp": true}); err != nil {
		t.Fatal(err)
	}
	if err := beginTOTPResetFixture(t, m, t.Context(), j.ID, "fixture-op"); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	m.removePendingLocked(j.ID)
	m.jobs[j.ID].Status = StatusFailed
	m.jobs[j.ID].OperationStatus = "failed"
	m.jobs[j.ID].TOTPWorkflowStatus = "failed"
	m.mu.Unlock()
	path := uncertainResetJournal(t, m, j)
	return m, v, j, p, path
}
func writeCompletionFixture(t *testing.T, path string) map[string]any {
	t.Helper()
	prior, err := protocol.ReadTOTPResetJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	out := completionFixtureResult(resultString(prior, "email"), resultString(prior, "operation_id"), completionFixtureSeed, "business_receipt", false)
	if err := protocol.WritePrivateJSON(path, out); err != nil {
		t.Fatal(err)
	}
	return out
}

type unboundRestoreCountingVault struct {
	*memoryVault
	saves int
}

func (v *unboundRestoreCountingVault) Save(email string, c credentials.Credentials) error {
	v.saves++
	return v.memoryVault.Save(email, c)
}
