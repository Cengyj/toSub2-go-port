package jobs

import (
	"errors"
	"os"
	"reflect"
	"slices"
	"testing"
)

func TestTOTPExportGateChecksSameAccountEvidenceWithoutChangingIt(t *testing.T) {
	for _, state := range []string{"queued", "pending", "vault", "unknown", "completion"} {
		t.Run(state, func(t *testing.T) {
			m, vault, owner, peer := resetManagerFixture(t)
			path := ""
			switch state {
			case "unknown":
				m, vault, owner, peer, path = pendingResetFixture(t)
			case "completion":
				m, vault, owner, peer, path = pendingResetFixture(t)
				writeCompletionFixture(t, path)
				credential, _ := vault.Load(owner.Email)
				credential.TOTPResetPending = false
				if err := vault.Save(owner.Email, credential); err != nil {
					t.Fatal(err)
				}
				for _, j := range m.jobs {
					j.TOTPRecoveryPending, j.TOTPCredentialInvalidated = false, false
				}
			case "queued":
				if _, err := m.QueueOperation(owner.ID, "reset-2fa", nil); err != nil {
					t.Fatal(err)
				}
			case "pending":
				m.jobs[owner.ID].TOTPRecoveryPending = true
			case "vault":
				credential, _ := vault.Load(owner.Email)
				credential.TOTPResetPending = true
				if err := vault.Save(owner.Email, credential); err != nil {
					t.Fatal(err)
				}
			}
			beforeCredential, _ := vault.Load(owner.Email)
			beforeOwner, _ := m.Get(owner.ID)
			beforePeer, _ := m.Get(peer.ID)
			beforeQueue := append([]*Job(nil), m.pending...)
			var beforeJournal []byte
			if path != "" {
				beforeJournal, _ = os.ReadFile(path)
			}
			for _, id := range []string{owner.ID, peer.ID} {
				if err := m.CheckTOTPExport(id); !errors.Is(err, ErrTOTPRecoveryRequired) {
					t.Fatal("account recovery evidence allowed saved-authorization export", id, err)
				}
				if _, err := m.LoadCredentials(id); !errors.Is(err, ErrTOTPRecoveryRequired) {
					t.Fatal("account recovery evidence allowed source credential export", id, err)
				}
			}
			afterCredential, _ := vault.Load(owner.Email)
			afterOwner, _ := m.Get(owner.ID)
			afterPeer, _ := m.Get(peer.ID)
			if !reflect.DeepEqual(beforeCredential, afterCredential) || !reflect.DeepEqual(beforeOwner, afterOwner) || !reflect.DeepEqual(beforePeer, afterPeer) || !slices.Equal(beforeQueue, m.pending) {
				t.Fatal("read-only export gate changed account or queue state")
			}
			if path != "" {
				afterJournal, err := os.ReadFile(path)
				if err != nil || string(beforeJournal) != string(afterJournal) {
					t.Fatal("export gate consumed private recovery evidence", err)
				}
			}
			unrelated, err := m.Submit(Spec{ID: "unrelated-export", Email: "other@example.test", ImportOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			if err := m.CheckTOTPExport(unrelated.ID); err != nil {
				t.Fatal("another account was blocked", err)
			}
		})
	}
}

func TestTOTPExportGateAllowsFullyCompletedResetHistory(t *testing.T) {
	m, vault, job, peer := resetManagerFixture(t)
	if _, err := m.QueueOperation(job.ID, "reset-2fa", nil); err != nil {
		t.Fatal(err)
	}
	if err := beginTOTPResetFixture(t, m, t.Context(), job.ID, "completed-export-operation"); err != nil {
		t.Fatal(err)
	}
	current, _ := vault.Load(job.Email)
	current.TOTPSecret, current.TOTPResetPending = completionFixtureSeed, false
	if err := vault.Save(job.Email, current); err != nil {
		t.Fatal(err)
	}
	if err := commitTOTPResetFixture(t, m, t.Context(), job.ID, current); err != nil {
		t.Fatal(err)
	}
	if err := markCommittedResetFixture(t, m, job.ID, current.TOTPResetOperationID); err != nil {
		t.Fatal(err)
	}

	m.mu.Lock()
	m.removePendingLocked(job.ID)
	m.jobs[job.ID].Status, m.jobs[job.ID].OperationStatus, m.jobs[job.ID].TOTPWorkflowStatus = StatusSucceeded, "succeeded", "succeeded"
	m.mu.Unlock()
	for _, id := range []string{job.ID, peer.ID} {
		if err := m.CheckTOTPExport(id); err != nil {
			t.Fatal("completed historical reset blocked authorization export", err)
		}
	}
}
