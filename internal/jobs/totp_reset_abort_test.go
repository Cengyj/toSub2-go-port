package jobs

import (
	"context"
	"path/filepath"
	"testing"

	"tosub2-go-port/internal/protocol"
)

func TestTOTPResetUnsentAbortRestoresCredentialAndPeersAcrossRestart(t *testing.T) {
	m, vault, j, peer := resetManagerFixture(t)
	original, _ := vault.Load(j.Email)
	_, _ = m.QueueOperation(j.ID, "reset-2fa", nil)
	if err := m.BeginTOTPReset(t.Context(), j.ID); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(m.store.Root, j.ID, "totp-setup-result.json")
	if err := m.AbortTOTPResetBeforeSend(t.Context(), j.ID, original); err == nil {
		t.Fatal("abort accepted missing safe journal")
	}
	result := map[string]any{"version": 1, "email": j.Email, "reset_totp_requested": true, "reset_totp_phase": "initial", "reset_totp_status": "failed", "activation_succeeded": false, "activation_verified": false, "credential_persisted": false, "old_factor_invalidated": false, "reset_totp_attempted_at": nil, "logout_all_devices_attempted_at": nil}
	if err := protocol.WritePrivateJSON(path, result); err != nil {
		t.Fatal(err)
	}
	if err := m.AbortTOTPResetBeforeSend(t.Context(), j.ID, original); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{j.ID, peer.ID} {
		got, _ := m.Get(id)
		if got.TOTPRecoveryPending || got.TOTPCredentialInvalidated || !got.HasTOTPKey || !got.TOTPKnownEnabled || got.Input["totpSecret"] != original.TOTPSecret {
			t.Fatal("old credential or peer remained quarantined")
		}
	}
	saved, _ := vault.Load(j.Email)
	if saved.TOTPResetPending || saved.TOTPSecret != original.TOTPSecret {
		t.Fatal("old vault credential was not restored")
	}
	m.mu.Lock()
	m.jobs[j.ID].Status = StatusFailed
	m.removePendingLocked(j.ID)
	m.persistLocked()
	m.mu.Unlock()
	restarted := NewManager(context.Background(), 1, m.store, nil)
	restarted.SetCredentialStore(vault)
	if err := restarted.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{j.ID, peer.ID} {
		got, _ := restarted.Get(id)
		if got.TOTPRecoveryPending || got.TOTPCredentialInvalidated || !got.HasTOTPKey {
			t.Fatal("restart recreated quarantine after a proven-unsent abort")
		}
	}
	if _, err := restarted.QueueOperation(j.ID, "reset-2fa", nil); err != nil {
		t.Fatal("explicit reset could not restart", err)
	}
}

func TestTOTPResetAbortRejectsMutationEvidenceAndChangedVault(t *testing.T) {
	for _, scenario := range []string{"changed-vault", "uncertain-journal", "later-phase"} {
		t.Run(scenario, func(t *testing.T) {
			m, vault, j, _ := resetManagerFixture(t)
			original, _ := vault.Load(j.Email)
			_, _ = m.QueueOperation(j.ID, "reset-2fa", nil)
			if err := m.BeginTOTPReset(t.Context(), j.ID); err != nil {
				t.Fatal(err)
			}
			result := map[string]any{"version": 1, "email": j.Email, "reset_totp_requested": true, "reset_totp_phase": "initial", "activation_succeeded": false, "activation_verified": false, "credential_persisted": false, "old_factor_invalidated": false}
			switch scenario {
			case "changed-vault":
				current, _ := vault.Load(j.Email)
				current.TOTPSecret = "DIFFERENTSEEDCANARY"
				_ = vault.Save(j.Email, current)
			case "uncertain-journal":
				result["reset_totp_phase"] = "recovery_required"
			case "later-phase":
				m.mu.Lock()
				m.jobs[j.ID].TOTPResetPhase = "enrolled"
				m.mu.Unlock()
			}
			_ = protocol.WritePrivateJSON(filepath.Join(m.store.Root, j.ID, "totp-setup-result.json"), result)
			if err := m.AbortTOTPResetBeforeSend(t.Context(), j.ID, original); err == nil {
				t.Fatal("abort accepted unknown or changed state")
			}
			got, _ := m.Get(j.ID)
			if !got.TOTPRecoveryPending {
				t.Fatal("failed abort cleared quarantine")
			}
		})
	}
}
