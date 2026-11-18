package protocol

import "testing"

// A process that died after its write-ahead record cannot have observed a
// transport completion. Do not turn quarantine or a possible send into a 200.
func TestInterruptedResetLogDoesNotInventReceipts(t *testing.T) {
	out := map[string]any{"version": 1, "mutation_schema": 1, "support_schema": 1, "operation_scope": "current_session", "operation_id": "synthetic-operation", "account_id": "synthetic-account", "email": "fixture@example.test", "old_totp_factor_id": "old", "old_factor_invalidated": true, "reset_totp_requested": true, "reset_totp_attempted_at": nowRFC3339(), "disable_dispatch": "may_have_been_sent", "disable_attempt_id": "synthetic-attempt", "enroll_dispatch": "not_sent", "activation_dispatch": "not_sent", "enroll_attempted": false, "activation_attempted": false}
	d := InterruptedResetLog(out, "fixture@example.test", 9)
	if d == nil || d.LastTrusted != "identity" || d.Reason != "INTERRUPTED_WRITE" || d.JournalWrites.Disable.Possible != 1 || d.JournalWrites.Disable.Received != 0 || d.Writes != nil || d.Removal != "unknown" || d.HTTP != 0 || d.Pending != "not_performed" {
		t.Fatal("intent-only evidence promoted", d)
	}
	if InterruptedResetLog(out, "someone-else@example.test", 1) != nil {
		t.Fatal("cross-account report")
	}
	delete(out, "support_schema")
	if InterruptedResetLog(out, "fixture@example.test", 1) != nil {
		t.Fatal("legacy record acquired invented evidence")
	}
}
