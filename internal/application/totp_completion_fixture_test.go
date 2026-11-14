package application

import (
	"path/filepath"
	"sync"
	"testing"
	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/privatestate"
	"tosub2-go-port/internal/protocol"
)

var completionFixtureProtectors sync.Map

// The manager fixtures must provide the same durable protocol evidence as the
// runner. Metadata flags alone are deliberately insufficient for cleanup.
func writeCommittedResetFixture(t *testing.T, root string, m *jobs.Manager, vault credentials.Store, id, operation string) {
	t.Helper()
	protector, err := credentials.NewMemoryStateProtector()
	if err != nil {
		t.Fatal(err)
	}
	actual, _ := completionFixtureProtectors.LoadOrStore(root, protector)
	if err := privatestate.Register(root, actual.(*credentials.MemoryStateProtector)); err != nil {
		t.Fatal(err)
	}
	j, _ := m.Get(id)
	c, err := vault.Load(j.Email)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]any{
		"version": 1, "operation_scope": "current_session", "email": j.Email, "operation_id": operation, "account_id": "fixture-account",
		"reset_totp_requested": true, "old_factor_invalidated": true, "old_totp_factor_id": "old-factor", "reset_totp_attempted_at": "2026-11-07T00:00:00Z",
		"enroll_attempted": true, "activation_attempted": true, "enrollment_validated": true, "secret": c.TOTPSecret, "session_id": "fixture-session", "new_totp_factor_id": "fixture-new",
		"enrollment_factor":    map[string]any{"id": "fixture-new", "factor_type": "totp", "is_recovery": false},
		"activation_succeeded": true, "activation_verified": true, "activation_confirmed_by_read": true, "credential_persisted": true, "reset_totp_phase": "credential_persisted",
		"logout_all_devices_requested": j.LogoutAllDevicesAfterTOTP, "logout_all_devices_status": j.LogoutAllDevicesStatus,
	}
	path := filepath.Join(root, id, "totp-setup-result.json")
	if err := protocol.WritePrivateJSON(path, out); err != nil {
		t.Fatal(err)
	}
	if _, err := protocol.ReadTOTPResetCompletion(path, j.Email); err != nil {
		t.Fatal(err)
	}
}
