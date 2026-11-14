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

func strictWorkflowExportFixture(t *testing.T) (*Manager, *memoryVault, *Job, *Job) {
	t.Helper()
	m, vault, owner, peer := resetManagerFixture(t)
	t.Cleanup(func() { _ = m.Shutdown(context.Background()) })
	values := map[string]any{"reloginAfterTotp": true, "uploadAfterTotp": true, "sub2apiConfig": map[string]any{"baseUrl": "https://workflow.example.test", "adminApiKey": "synthetic-admin"}}
	if _, err := m.QueueOperation(owner.ID, "reset-2fa", values); err != nil {
		t.Fatal(err)
	}
	if err := m.StartTOTPWorkflowStep(t.Context(), owner.ID, "totp"); err != nil {
		t.Fatal(err)
	}
	if err := beginTOTPResetFixture(t, m, t.Context(), owner.ID, "workflow-export-operation"); err != nil {
		t.Fatal(err)
	}
	c, err := vault.Load(owner.Email)
	if err != nil {
		t.Fatal(err)
	}
	c.TOTPSecret, c.TOTPResetPending = "JBSWY3DPEHPK3PXP", false
	if err := vault.Save(owner.Email, c); err != nil {
		t.Fatal(err)
	}
	for _, action := range []func() error{
		func() error { return commitTOTPResetFixture(t, m, t.Context(), owner.ID, c) },
		func() error { return markCommittedResetFixture(t, m, owner.ID, c.TOTPResetOperationID) },

		func() error { return m.StartTOTPWorkflowStep(t.Context(), owner.ID, "relogin") },
	} {
		if err := action(); err != nil {
			t.Fatal(err)
		}
	}
	m.UpdateResult(owner.ID, true, "synthetic fresh authorization")
	if err := m.CompleteTOTPWorkflowStep(t.Context(), owner.ID, "relogin"); err != nil {
		t.Fatal(err)
	}
	if err := m.StartTOTPWorkflowStep(t.Context(), owner.ID, "upload"); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	m.removePendingLocked(owner.ID)
	m.active[owner.ID] = func() {}
	m.jobs[owner.ID].Status = StatusRunning
	m.mu.Unlock()
	return m, vault, owner, peer
}

func TestStrictTOTPWorkflowExportExemptsOnlyLegalOwnActiveUpload(t *testing.T) {
	for _, scenario := range []string{"valid", "own-queued", "wrong-step", "not-requested", "unsaved", "logout-pending", "completion-operation", "peer-active", "peer-queued", "recovery-pending", "credential-invalidated", "own-retained-journal", "peer-unknown-journal", "peer-completion-journal", "vault-pending", "vault-unavailable", "vault-changed-operation"} {
		t.Run(scenario, func(t *testing.T) {
			m, vault, owner, peer := strictWorkflowExportFixture(t)
			j, p := m.jobs[owner.ID], m.jobs[peer.ID]
			journalPath := ""
			switch scenario {
			case "own-queued":
				delete(m.active, owner.ID)
				m.pending = append(m.pending, j)
			case "wrong-step":
				j.TOTPWorkflowStep = "relogin"
			case "not-requested":
				j.UploadAfterTOTP = false
			case "unsaved":
				j.ResultSaved = false
			case "logout-pending":
				j.LogoutAllDevicesAfterTOTP, j.LogoutAllDevicesStatus = true, "pending"
			case "completion-operation":
				j.Operation = "complete-2fa"
			case "peer-active":
				p.Operation = "reset-2fa"
				m.active[p.ID] = func() {}
			case "peer-queued":
				p.Operation = "reset-2fa"
				m.pending = append(m.pending, p)
			case "recovery-pending":
				j.TOTPRecoveryPending = true
			case "credential-invalidated":
				j.TOTPCredentialInvalidated = true
			case "own-retained-journal":
				journalPath = filepath.Join(m.store.Root, owner.ID, "totp-setup-result.json")
				if err := protocol.WritePrivateJSON(journalPath, map[string]any{"retained": "synthetic"}); err != nil {
					t.Fatal(err)
				}
			case "peer-unknown-journal", "peer-completion-journal":
				var source string
				if scenario == "peer-unknown-journal" {
					_, _, _, _, source = pendingResetFixture(t)
				} else {
					_, _, _, _, source = pendingResetFixture(t)
					writeCompletionFixture(t, source)
				}
				data, err := os.ReadFile(source)
				if err != nil {
					t.Fatal(err)
				}
				journalPath = filepath.Join(m.store.Root, peer.ID, "totp-setup-result.json")
				if err := protocol.WritePrivateJSON(journalPath, map[string]any{}); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(journalPath, data, 0600); err != nil {
					t.Fatal(err)
				}
			case "vault-pending", "vault-changed-operation":
				c, _ := vault.Load(owner.Email)
				if scenario == "vault-pending" {
					c.TOTPResetPending = true
				} else {
					c.TOTPResetOperationID = "another-completed-operation"
				}
				if err := vault.Save(owner.Email, c); err != nil {
					t.Fatal(err)
				}
			case "vault-unavailable":
				m.SetCredentialStore(&strictReviewUnavailableVault{Store: vault})
			}
			beforeOwner, beforePeer := cloneJob(j), cloneJob(p)
			beforeCredential, _ := vault.Load(owner.Email)
			var beforeJournal []byte
			if journalPath != "" {
				beforeJournal, _ = os.ReadFile(journalPath)
			}
			err := m.CheckTOTPWorkflowExport(owner.ID)
			if scenario == "valid" {
				if err != nil {
					t.Fatal("completed reset's legal own active upload was blocked", err)
				}
			} else {
				want := ErrTOTPRecoveryPending
				if scenario == "vault-unavailable" {
					want = ErrTOTPExportUnavailable
				}
				if !errors.Is(err, want) {
					t.Fatal("workflow exemption bypassed a recovery boundary", err)
				}
			}
			if err := m.CheckTOTPExport(owner.ID); !errors.Is(err, ErrTOTPRecoveryPending) {
				t.Fatal("generic export acquired the workflow exemption", err)
			}
			afterCredential, _ := vault.Load(owner.Email)
			if !reflect.DeepEqual(beforeOwner, j) || !reflect.DeepEqual(beforePeer, p) || !reflect.DeepEqual(beforeCredential, afterCredential) {
				t.Fatal("workflow export gate changed account or vault state")
			}
			if journalPath != "" {
				afterJournal, err := os.ReadFile(journalPath)
				if err != nil || string(beforeJournal) != string(afterJournal) {
					t.Fatal("workflow export gate changed recovery evidence", err)
				}
			}
		})
	}
}
