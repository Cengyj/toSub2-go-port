package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"tosub2-go-port/internal/protocol"
)

// Completion fixtures carry the same account, operation and material binding
// as real read-confirmed activations, without any saved Cookie or session jar.
func runnerCompletionJournal(t *testing.T, root, id, email, operation, secret, logout string) string {
	t.Helper()
	material, _ := json.Marshal([]string{secret, "fixture-enrollment", "fixture-new-factor"})
	digest := sha256.Sum256(material)
	out := map[string]any{
		"version": 1, "email": email, "operation_scope": "current_session", "operation_id": operation, "account_id": "fixture-account",
		"reset_totp_requested": true, "old_factor_invalidated": true, "old_totp_factor_id": "fixture-old-factor",
		"reset_totp_attempted_at": "2026-11-07T00:00:00Z", "enroll_attempted": true, "activation_attempted": true,
		"enrollment_validated": true, "secret": secret, "session_id": "fixture-enrollment", "new_totp_factor_id": "fixture-new-factor",
		"enrollment_factor":    map[string]any{"id": "fixture-new-factor", "factor_type": "totp", "is_recovery": false},
		"activation_succeeded": true, "activation_verified": true, "activation_confirmed_by_read": true, "credential_persisted": true,
		"activation_confirmation_source": "state_read", "activation_evidence_schema": 2, "activation_attempt_id": "fixture-activation-attempt",
		"activation_evidence": map[string]any{"operation_id": operation, "account_id": "fixture-account", "email": strings.ToLower(strings.TrimSpace(email)), "factor_id": "fixture-new-factor", "enrollment_digest": hex.EncodeToString(digest[:]), "confirmed_at": "2026-11-07T00:00:00Z", "confirmation_source": "state_read", "attempt_id": "fixture-activation-attempt"},
		"reset_totp_status":   "succeeded", "reset_totp_phase": "credential_persisted",
		"logout_all_devices_requested": logout != "not_requested", "logout_all_devices_status": logout,
	}
	path := filepath.Join(root, id, "totp-setup-result.json")
	if err := protocol.WritePrivateJSON(path, out); err != nil {
		t.Fatal(err)
	}
	if _, err := protocol.ReadTOTPResetCompletion(path, email); err != nil {
		t.Fatal("invalid completion fixture", err)
	}
	return path
}

func runnerResetIntentJournal(t *testing.T, root, id, email, operation string) {
	t.Helper()
	out := map[string]any{
		"version": 1, "email": email, "operation_scope": "current_session", "operation_id": operation,
		"account_id": "fixture-account", "old_totp_factor_id": "fixture-old-factor", "reset_totp_requested": true,
		"reset_totp_phase": "quarantine_intent", "old_factor_invalidated": false,
		"enroll_attempted": false, "activation_attempted": false,
		"mutation_schema": 1, "disable_dispatch": "not_sent", "enroll_dispatch": "not_sent", "activation_dispatch": "not_sent",
	}
	if err := protocol.WritePrivateJSON(filepath.Join(root, id, "totp-setup-result.json"), out); err != nil {
		t.Fatal(err)
	}
}
