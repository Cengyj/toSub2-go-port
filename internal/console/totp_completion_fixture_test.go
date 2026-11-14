package console

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"sync"
	"testing"
	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/privatestate"
	"tosub2-go-port/internal/protocol"
)

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

func writeCommittedResetFixture(t *testing.T, root string, m *jobs.Manager, vault credentials.Store, id, operation string) {
	t.Helper()
	registerCompletionFixture(t, root)
	j, _ := m.Get(id)
	c, err := vault.Load(j.Email)
	if err != nil {
		t.Fatal(err)
	}
	out := completionFixtureResult(j.Email, operation, c.TOTPSecret, "business_receipt", true)
	out["logout_all_devices_requested"], out["logout_all_devices_status"] = j.LogoutAllDevicesAfterTOTP, j.LogoutAllDevicesStatus
	path := filepath.Join(root, id, "totp-setup-result.json")
	if err := protocol.WritePrivateJSON(path, out); err != nil {
		t.Fatal(err)
	}
	if _, err := protocol.ReadTOTPResetCompletion(path, j.Email); err != nil {
		t.Fatal(err)
	}
}

func finishConsoleTOTPReset(t *testing.T, root string, m *jobs.Manager, id, operation string) error {
	t.Helper()
	j, _ := m.Get(id)
	path := filepath.Join(root, id, "totp-setup-result.json")
	out, err := protocol.ReadTOTPResetCompletion(path, j.Email)
	if err != nil {
		return err
	}
	out["logout_all_devices_status"] = j.LogoutAllDevicesStatus
	if err := protocol.WritePrivateJSON(path, out); err != nil {
		return err
	}
	return m.MarkTOTPResetCommitted(context.Background(), id, operation)
}

func beginConsoleTOTPReset(t *testing.T, root string, m *jobs.Manager, ctx context.Context, id string, operations ...string) error {
	t.Helper()
	operation := "console-fixture-operation"
	if len(operations) > 0 {
		operation = operations[0]
	}
	j, _ := m.Get(id)
	registerCompletionFixture(t, root)
	out := map[string]any{
		"version": 1, "operation_scope": "current_session", "mutation_schema": 1,
		"email": j.Email, "operation_id": operation, "account_id": "fixture-account",
		"reset_totp_requested": true, "reset_totp_phase": "quarantine_intent", "reset_totp_status": "pending",
		"old_totp_factor_id": "fixture-old", "old_factor_invalidated": false,
		"disable_dispatch": "not_sent", "enroll_dispatch": "not_sent", "activation_dispatch": "not_sent",
		"enroll_attempted": false, "activation_attempted": false,
	}
	if err := protocol.WritePrivateJSON(filepath.Join(root, id, "totp-setup-result.json"), out); err != nil {
		return err
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
