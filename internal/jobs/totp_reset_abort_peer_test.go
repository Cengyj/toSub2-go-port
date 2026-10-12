package jobs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"tosub2-go-port/internal/protocol"
)

func TestTOTPResetAbortNeverClearsIndependentPeerRecovery(t *testing.T) {
	for _, begun := range []bool{false, true} {
		name := "before-intent"
		if begun {
			name = "after-intent"
		}
		t.Run(name, func(t *testing.T) {
			m, vault, j, peer := resetManagerFixture(t)
			original, _ := vault.Load(j.Email)
			if _, err := m.QueueOperation(j.ID, "reset-2fa", nil); err != nil {
				t.Fatal(err)
			}
			if begun {
				if err := m.BeginTOTPReset(t.Context(), j.ID); err != nil {
					t.Fatal(err)
				}
			}
			peerPath := filepath.Join(m.store.Root, peer.ID, "totp-setup-result.json")
			if err := protocol.WritePrivateJSON(peerPath, map[string]any{
				"version": 1, "email": j.Email, "reset_totp_requested": true,
				"reset_totp_phase": "recovery_required", "old_factor_invalidated": true,
			}); err != nil {
				t.Fatal(err)
			}
			if !begun {
				if err := m.BeginTOTPReset(t.Context(), j.ID); !errors.Is(err, ErrTOTPRecoveryRequired) {
					t.Fatal("begin did not retain independent recovery evidence", err)
				}
			}
			path := filepath.Join(m.store.Root, j.ID, "totp-setup-result.json")
			if err := protocol.WritePrivateJSON(path, initialResetJournal(j.Email)); err != nil {
				t.Fatal(err)
			}
			beforeCredential, _ := vault.Load(j.Email)
			beforeMetadata, err := os.ReadFile(m.store.Path("job-meta.json"))
			if err != nil {
				t.Fatal(err)
			}
			beforeJob, _ := m.Get(j.ID)
			beforePeer, _ := m.Get(peer.ID)
			if err := m.AbortTOTPResetBeforeSend(t.Context(), j.ID, original); !errors.Is(err, ErrTOTPRecoveryRequired) {
				t.Fatal("safe own journal released independent peer recovery", err)
			}
			afterCredential, _ := vault.Load(j.Email)
			afterMetadata, err := os.ReadFile(m.store.Path("job-meta.json"))
			if err != nil || !reflect.DeepEqual(beforeCredential, afterCredential) || string(beforeMetadata) != string(afterMetadata) {
				t.Fatal("rejected abort changed durable account state")
			}
			afterJob, _ := m.Get(j.ID)
			afterPeer, _ := m.Get(peer.ID)
			if !reflect.DeepEqual(beforeJob, afterJob) || !reflect.DeepEqual(beforePeer, afterPeer) {
				t.Fatal("rejected abort cleared account quarantine or changed metadata")
			}
		})
	}
}

func TestTOTPResetAbortStillPermitsStrictlyInitialPeerJournal(t *testing.T) {
	m, vault, j, peer := resetManagerFixture(t)
	original, _ := vault.Load(j.Email)
	if _, err := m.QueueOperation(j.ID, "reset-2fa", nil); err != nil {
		t.Fatal(err)
	}
	if err := m.BeginTOTPReset(t.Context(), j.ID); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{j.ID, peer.ID} {
		if err := protocol.WritePrivateJSON(filepath.Join(m.store.Root, id, "totp-setup-result.json"), initialResetJournal(j.Email)); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.AbortTOTPResetBeforeSend(t.Context(), j.ID, original); err != nil {
		t.Fatal("strictly unmodified peer journal blocked a safe abort", err)
	}
	for _, id := range []string{j.ID, peer.ID} {
		got, _ := m.Get(id)
		if got.TOTPRecoveryPending || got.TOTPCredentialInvalidated || !got.HasTOTPKey || !got.TOTPKnownEnabled {
			t.Fatal("safe abort retained quarantine")
		}
	}
}

func TestTOTPResetAbortNeverClearsPreexistingPeerRecoveryWithoutJournal(t *testing.T) {
	m, vault, j, peer := resetManagerFixture(t)
	original, _ := vault.Load(j.Email)
	if _, err := m.QueueOperation(j.ID, "reset-2fa", nil); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	quarantineTOTP(m.jobs[peer.ID])
	m.persistLocked()
	m.mu.Unlock()
	if err := m.BeginTOTPReset(t.Context(), j.ID); !errors.Is(err, ErrTOTPRecoveryRequired) {
		t.Fatal("begin did not retain preexisting account recovery", err)
	}
	if err := protocol.WritePrivateJSON(filepath.Join(m.store.Root, j.ID, "totp-setup-result.json"), initialResetJournal(j.Email)); err != nil {
		t.Fatal(err)
	}
	if err := m.AbortTOTPResetBeforeSend(t.Context(), j.ID, original); !errors.Is(err, ErrTOTPRecoveryRequired) {
		t.Fatal("unstarted reset released preexisting peer quarantine", err)
	}
	got, _ := m.Get(peer.ID)
	current, _ := vault.Load(j.Email)
	if !got.TOTPRecoveryPending || !got.TOTPCredentialInvalidated || !reflect.DeepEqual(current, original) {
		t.Fatal("rejected abort changed independent recovery or the vault")
	}
}

func TestTOTPResetAbortPreservesKnownFactorWithoutLocalSeedAcrossRestart(t *testing.T) {
	m, vault, j, peer := resetManagerFixture(t)
	original, _ := vault.Load(j.Email)
	original.TOTPSecret = ""
	if err := vault.Save(j.Email, original); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	for _, related := range m.jobs {
		related.HasTOTPKey, related.TOTPKnownEnabled = false, true
		delete(related.Input, "totpSecret")
	}
	m.persistLocked()
	m.mu.Unlock()
	if _, err := m.QueueOperation(j.ID, "reset-2fa", nil); err != nil {
		t.Fatal(err)
	}
	if err := m.BeginTOTPReset(t.Context(), j.ID); err != nil {
		t.Fatal(err)
	}
	if err := protocol.WritePrivateJSON(filepath.Join(m.store.Root, j.ID, "totp-setup-result.json"), initialResetJournal(j.Email)); err != nil {
		t.Fatal(err)
	}
	if err := m.AbortTOTPResetBeforeSend(t.Context(), j.ID, original); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	m.jobs[j.ID].Status = StatusFailed
	m.removePendingLocked(j.ID)
	m.persistLocked()
	m.mu.Unlock()
	restarted := NewManager(context.Background(), 1, m.store, nil)
	restarted.SetCredentialStore(vault)
	if err := restarted.Restore(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{j.ID, peer.ID} {
		got, _ := restarted.Get(id)
		if got.TOTPRecoveryPending || got.TOTPCredentialInvalidated || got.HasTOTPKey || !got.TOTPKnownEnabled {
			t.Fatal("unsent abort lost knowledge of the unchanged remote factor")
		}
	}
	if _, err := restarted.QueueOperation(j.ID, "reset-2fa", nil); err != nil {
		t.Fatal("known remote factor without a local seed could not retry", err)
	}
}

func TestTOTPResetSafeAbortJournalAloneDoesNotReleaseQuarantineAfterCrash(t *testing.T) {
	m, vault, j, peer := resetManagerFixture(t)
	if _, err := m.QueueOperation(j.ID, "reset-2fa", nil); err != nil {
		t.Fatal(err)
	}
	if err := m.BeginTOTPReset(t.Context(), j.ID); err != nil {
		t.Fatal(err)
	}
	// Simulate a process stopping between proof persistence and vault/metadata
	// restoration. No credential-restoration acknowledgement was completed.
	if err := protocol.WritePrivateJSON(filepath.Join(m.store.Root, j.ID, "totp-setup-result.json"), initialResetJournal(j.Email)); err != nil {
		t.Fatal(err)
	}
	restarted := NewManager(context.Background(), 1, m.store, nil)
	restarted.SetCredentialStore(vault)
	if err := restarted.Restore(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{j.ID, peer.ID} {
		got, _ := restarted.Get(id)
		if !got.TOTPRecoveryPending || !got.TOTPCredentialInvalidated || got.HasTOTPKey {
			t.Fatal("partial local abort was treated as acknowledged restoration")
		}
	}
	if _, err := restarted.QueueOperation(j.ID, "reset-2fa", nil); !errors.Is(err, ErrTOTPRecoveryRequired) {
		t.Fatal("partial abort replayed a reset after restart", err)
	}
}
