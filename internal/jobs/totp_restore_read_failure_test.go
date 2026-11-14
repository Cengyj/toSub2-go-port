package jobs

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/protocol"
)

type journalChangingRecoveryVault struct {
	*memoryVault
	path, fault  string
	loads, saves int
	faultErr     error
}

func (v *journalChangingRecoveryVault) Load(email string) (credentials.Credentials, error) {
	current, err := v.memoryVault.Load(email)
	v.loads++
	if v.loads == 1 && err == nil {
		v.faultErr = os.Rename(v.path, v.path+".synthetic-preserved")
		if v.faultErr == nil {
			switch v.fault {
			case "directory":
				v.faultErr = os.Mkdir(v.path, 0700)
			case "malformed":
				v.faultErr = os.WriteFile(v.path, []byte("synthetic unreadable journal"), 0600)
			case "replacement":
				v.faultErr = protocol.WritePrivateJSON(v.path, map[string]any{"version": 1, "email": email, "activation_succeeded": true, "secret": "SYNTHETIC_DIFFERENT_SETUP"})
			}
		}
	}
	return current, err
}

func (v *journalChangingRecoveryVault) Save(email string, current credentials.Credentials) error {
	v.saves++
	return v.memoryVault.Save(email, current)
}

func writeSiblingRecoveryPassword(t *testing.T, root string, job *Job) string {
	t.Helper()
	path := filepath.Join(root, job.ID, "password-add-result.json")
	if err := protocol.WritePrivateJSON(path, map[string]any{"version": 1, "email": job.Email, "password": "Synthetic-recovered-password-123"}); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestTOTPStartupJournalChangeDuringVaultReadNeverRecoversSiblingPassword(t *testing.T) {
	for _, fault := range []string{"directory", "missing", "malformed", "replacement"} {
		t.Run(fault, func(t *testing.T) {
			m, vault, job, _, path := pendingResetFixture(t)
			passwordPath := writeSiblingRecoveryPassword(t, m.store.Root, job)
			passwordBefore, err := os.ReadFile(passwordPath)
			if err != nil {
				t.Fatal(err)
			}
			before, _ := vault.Load(job.Email)
			changing := &journalChangingRecoveryVault{memoryVault: vault, path: path, fault: fault}
			candidate, _ := m.Get(job.ID)
			recovered, err := recoverPrivateResult(t.Context(), m.store.Root, candidate, changing, nil)
			if changing.faultErr != nil || changing.loads != 1 {
				t.Fatal("journal fault did not occur at the first protected credential read", changing.faultErr, changing.loads)
			}
			after, _ := vault.Load(job.Email)
			passwordAfter, readErr := os.ReadFile(passwordPath)
			if err == nil || !recovered.totpReadOnly || !recovered.totpError || recovered.password || recovered.totp || changing.saves != 0 || !reflect.DeepEqual(before, after) {
				t.Fatal("changed reset journal allowed startup credential writes or non-read-only recovery", err)
			}
			if !candidate.TOTPRecoveryPending || !candidate.TOTPCredentialInvalidated || candidate.HasTOTPKey || candidate.passwordCleanup || candidate.totpSetupCleanup || readErr != nil || string(passwordAfter) != string(passwordBefore) {
				t.Fatal("journal read failure released quarantine or marked sibling evidence for cleanup", readErr)
			}
		})
	}
}

func TestTOTPStartupMissingJournalHonorsOnlyExplicitPendingState(t *testing.T) {
	for _, state := range []string{"recovery-pending", "credential-invalidated", "vault-pending", "ordinary-password", "completed-history"} {
		t.Run(state, func(t *testing.T) {
			m, vault, job, _ := resetManagerFixture(t)
			passwordPath := writeSiblingRecoveryPassword(t, m.store.Root, job)
			candidate, _ := m.Get(job.ID)
			before, _ := vault.Load(job.Email)
			switch state {
			case "recovery-pending":
				candidate.TOTPRecoveryPending = true
			case "credential-invalidated":
				candidate.TOTPCredentialInvalidated = true
			case "vault-pending":
				before.TOTPResetPending = true
				if err := vault.Save(job.Email, before); err != nil {
					t.Fatal(err)
				}
			case "completed-history":
				candidate.ResetTOTP, candidate.TOTPResetPhase = true, "credential_persisted"
			}
			counted := &unboundRestoreCountingVault{memoryVault: vault}
			recovered, err := recoverPrivateResult(t.Context(), m.store.Root, candidate, counted, nil)
			after, _ := vault.Load(job.Email)
			if state == "ordinary-password" || state == "completed-history" {
				if err != nil || recovered.totpReadOnly || !recovered.password || !candidate.passwordCleanup || counted.saves != 1 || after.Password != "Synthetic-recovered-password-123" || candidate.TOTPRecoveryPending {
					t.Fatal("ordinary password recovery or completed history was incorrectly quarantined", err)
				}
				return
			}
			if err == nil || !recovered.totpReadOnly || !recovered.totpError || recovered.password || recovered.totp || counted.saves != 0 || !reflect.DeepEqual(before, after) || candidate.passwordCleanup || candidate.totpSetupCleanup || !candidate.TOTPRecoveryPending || !candidate.TOTPCredentialInvalidated {
				t.Fatal("missing journal bypassed explicit pending reset protection", err)
			}
			if _, err := os.Stat(passwordPath); err != nil {
				t.Fatal("pending reset removed sibling password recovery evidence", err)
			}
		})
	}
}
