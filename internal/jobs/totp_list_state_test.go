package jobs

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"tosub2-go-port/internal/protocol"
	"tosub2-go-port/internal/store"
)

func assertTOTPListMatchesIndividual(t *testing.T, manager *Manager, ids ...string) map[string]TOTPListState {
	t.Helper()
	states := manager.TOTPListStates(ids)
	for _, id := range ids {
		state := states[id]
		if state.CompletionPending != manager.TOTPResetCompletionPending(id) || state.ExportPending != manager.TOTPExportPending(id) {
			t.Fatal("batched projection changed account recovery boundary", id)
		}
		if state.CompletionPending {
			want := manager.CanCompleteTOTPReset(id)
			if (want == nil) != (state.CompletionError == nil) || want != nil && want.Error() != state.CompletionError.Error() {
				t.Fatal("batched completion projection changed eligibility", id)
			}
		}
	}
	return states
}

func TestTOTPListStatesReadNewJournalAndCoverEntireEmailHistory(t *testing.T) {
	m, vault, owner, peer := resetManagerFixture(t)
	counted := &strictReviewCountingVault{Store: vault}
	m.SetCredentialStore(counted)
	assertTOTPListMatchesIndividual(t, m, owner.ID, peer.ID)
	if counted.loads != 0 {
		t.Fatal("healthy list read protected credentials")
	}
	path := filepath.Join(m.store.Root, owner.ID, "totp-setup-result.json")
	if err := protocol.WritePrivateJSON(path, map[string]any{"version": 1, "email": owner.Email, "synthetic_unknown_record": true}); err != nil {
		t.Fatal(err)
	}
	// A sibling journal written by another process must affect the selected
	// newest record even though its public metadata has never changed.
	states := assertTOTPListMatchesIndividual(t, m, peer.ID)
	if !states[peer.ID].ExportPending || states[peer.ID].CompletionPending || counted.loads != 0 {
		t.Fatal("new historical journal was hidden by list projection")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if assertTOTPListMatchesIndividual(t, m, peer.ID)[peer.ID].ExportPending {
		t.Fatal("one list call cached a removed journal into a later call")
	}
	if _, err := m.QueueOperation(owner.ID, "reset-2fa", nil); err != nil {
		t.Fatal(err)
	}
	if !assertTOTPListMatchesIndividual(t, m, peer.ID)[peer.ID].ExportPending {
		t.Fatal("sibling queued reset did not block list export")
	}
}

func TestTOTPListCompletionUsesExactExistingEvidenceAndCredentialGate(t *testing.T) {
	m, vault, owner, peer, path := localCompletionFixture(t, "business_receipt")
	states := assertTOTPListMatchesIndividual(t, m, owner.ID, peer.ID)
	if !states[owner.ID].CompletionPending || states[owner.ID].CompletionError != nil || !states[peer.ID].ExportPending {
		t.Fatal("valid local completion was not projected")
	}
	current, _ := vault.Load(owner.Email)
	current.TOTPResetOperationID = "another-client-operation"
	if err := vault.Save(owner.Email, current); err != nil {
		t.Fatal(err)
	}
	states = assertTOTPListMatchesIndividual(t, m, owner.ID, peer.ID)
	if states[owner.ID].CompletionError == nil {
		t.Fatal("stale list completion bypassed changed protected binding")
	}
	if err := os.WriteFile(path, []byte("synthetic corrupted journal"), 0600); err != nil {
		t.Fatal(err)
	}
	states = assertTOTPListMatchesIndividual(t, m, owner.ID, peer.ID)
	if states[owner.ID].CompletionPending || !states[peer.ID].ExportPending {
		t.Fatal("corrupted completion became safe or eligible")
	}
}

func TestTOTPListOperationInFlightFollowsSameEmailOwnership(t *testing.T) {
	for _, operation := range []string{"reset-2fa", "complete-2fa"} {
		t.Run(operation, func(t *testing.T) {
			var m *Manager
			var owner, peer *Job
			completion := operation == "complete-2fa"
			if completion {
				m, _, owner, peer, _ = localCompletionFixture(t, "business_receipt")
			} else {
				m, _, owner, peer = resetManagerFixture(t)
			}
			other, err := m.Submit(Spec{ID: "unrelated", Email: "unrelated@example.test", ImportOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			queued, err := m.QueueOperation(owner.ID, operation, nil)
			if err != nil {
				t.Fatal(err)
			}
			assertOwned := func(want bool) {
				t.Helper()
				states := assertTOTPListMatchesIndividual(t, m, owner.ID, peer.ID, other.ID)
				for _, id := range []string{owner.ID, peer.ID} {
					if states[id].OperationInFlight != want || !states[id].ExportPending {
						t.Fatalf("account ownership and export isolation diverged for %s: %+v", id, states[id])
					}
				}
				if states[owner.ID].CompletionPending != completion || states[peer.ID].CompletionPending {
					t.Fatal("ownership projection changed operation-bound local completion evidence")
				}
				if states[other.ID].OperationInFlight || states[other.ID].ExportPending {
					t.Fatal("same-email ownership leaked into an unrelated account")
				}
				if want && (states[owner.ID].OwnOperationAt.IsZero() || !states[owner.ID].OwnOperationAt.Equal(queued.LastOperationAt)) || !want && !states[owner.ID].OwnOperationAt.IsZero() {
					t.Fatal("own operation generation did not follow actual runner ownership")
				}
				if !states[peer.ID].OwnOperationAt.IsZero() || !states[other.ID].OwnOperationAt.IsZero() {
					t.Fatal("another row inherited the owner's operation generation")
				}
			}
			// Selecting only the retained peer must still find the queued owner.
			if state := m.TOTPListStates([]string{peer.ID})[peer.ID]; !state.OperationInFlight || !state.OwnOperationAt.IsZero() {
				t.Fatal("selected peer omitted the queued reset on its account")
			}
			assertOwned(true)

			started := make(chan func(JobEvent), 1)
			allowReturn := make(chan struct{})
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(allowReturn) }) }
			m.SetRunner(func(ctx context.Context, _ *Job, emit func(JobEvent)) error {
				started <- emit
				<-ctx.Done()
				<-allowReturn
				return ctx.Err()
			})
			t.Cleanup(func() {
				release()
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				if err := m.Shutdown(ctx); err != nil {
					t.Error(err)
				}
			})
			m.Start()
			var emit func(JobEvent)
			select {
			case emit = <-started:
			case <-time.After(time.Second):
				t.Fatal("synthetic reset runner did not start")
			}
			assertOwned(true)
			if err := m.StartTOTPWorkflowStep(t.Context(), owner.ID, "totp"); err != nil {
				t.Fatal(err)
			}
			if !completion {
				if err := beginTOTPResetFixture(t, m, t.Context(), owner.ID); err != nil {
					t.Fatal(err)
				}
			}
			emit(JobEvent{Stage: "totp_setup_otp", Status: StatusWaitingInput})
			waiting, _ := m.Get(owner.ID)
			if waiting.OperationStatus != "waiting" || !waiting.TOTPRecoveryPending || !waiting.TOTPCredentialInvalidated {
				t.Fatal("synthetic waiting state did not retain normal reset isolation")
			}
			assertOwned(true)
			if canceled, err := m.CancelWithError(owner.ID); !canceled || err != nil {
				t.Fatalf("cancel: accepted=%v error=%v", canceled, err)
			}
			// A stop request does not release the runner while it is cleaning up.
			assertOwned(true)
			release()
			waitForStatus(t, m, owner.ID, StatusFailed)
			assertOwned(false)
			// Retained metadata must not invent an owner, even if its display
			// status is stale. The unknown/confirmed evidence stays protected.
			for _, status := range []string{StatusQueued, StatusRunning, StatusWaitingInput} {
				m.Emit(JobEvent{JobID: owner.ID, Stage: "totp_setup_otp", Status: status})
				assertOwned(false)
			}
		})
	}
}

func TestTOTPListQueuedResetCancellationReleasesOnlyExecutionOwnership(t *testing.T) {
	m, _, owner, peer := resetManagerFixture(t)
	if _, err := m.QueueOperation(owner.ID, "reset-2fa", nil); err != nil {
		t.Fatal(err)
	}
	if canceled, err := m.CancelWithError(owner.ID); !canceled || err != nil {
		t.Fatalf("cancel: accepted=%v error=%v", canceled, err)
	}
	states := assertTOTPListMatchesIndividual(t, m, owner.ID, peer.ID)
	for _, id := range []string{owner.ID, peer.ID} {
		if states[id].OperationInFlight || !states[id].OwnOperationAt.IsZero() || states[id].ExportPending || states[id].CompletionPending {
			t.Fatal("canceling before execution invented mutation or recovery evidence", id)
		}
	}
}

func TestTOTPListSetupOwnershipDoesNotChangeExistingJournalGate(t *testing.T) {
	for _, operation := range []string{"setup-2fa", "setup_2fa", "setup-totp", "totp_setup"} {
		t.Run(operation, func(t *testing.T) {
			st, err := store.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			m := NewManager(t.Context(), 1, st, nil)
			m.SetCredentialStore(&memoryVault{})
			owner, err := m.Submit(Spec{ID: "setup", Email: "setup@example.test", ImportOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			peer, err := m.Submit(Spec{ID: "setup-peer", Email: owner.Email, ImportOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			queued, err := m.QueueOperation(owner.ID, operation, nil)
			if err != nil {
				t.Fatal(err)
			}
			assertState := func(inFlight, blocked bool) {
				t.Helper()
				states := assertTOTPListMatchesIndividual(t, m, owner.ID, peer.ID)
				for _, id := range []string{owner.ID, peer.ID} {
					if states[id].OperationInFlight != inFlight || states[id].ExportPending != blocked || states[id].CompletionPending {
						t.Fatalf("setup ownership changed the existing recovery boundary for %s: %+v", id, states[id])
					}
				}
				if inFlight && (states[owner.ID].OwnOperationAt.IsZero() || !states[owner.ID].OwnOperationAt.Equal(queued.LastOperationAt)) || !inFlight && !states[owner.ID].OwnOperationAt.IsZero() {
					t.Fatal("setup generation outlived its own queue or runner")
				}
				if !states[peer.ID].OwnOperationAt.IsZero() {
					t.Fatal("setup peer inherited the owner's operation generation")
				}
			}
			assertState(true, false)
			started := make(chan func(JobEvent), 1)
			allowReturn := make(chan struct{})
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(allowReturn) }) }
			m.SetRunner(func(ctx context.Context, _ *Job, emit func(JobEvent)) error {
				started <- emit
				<-ctx.Done()
				<-allowReturn
				return ctx.Err()
			})
			t.Cleanup(func() {
				release()
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				if err := m.Shutdown(ctx); err != nil {
					t.Error(err)
				}
			})
			m.Start()
			var emit func(JobEvent)
			select {
			case emit = <-started:
			case <-time.After(time.Second):
				t.Fatal("synthetic setup runner did not start")
			}
			assertState(true, false)
			registerCompletionFixture(t, st.Root)
			path := filepath.Join(st.Root, owner.ID, "totp-setup-result.json")
			candidate := map[string]any{
				"version": 1, "email": owner.Email, "operation_id": "setup-list-operation", "already_enabled": false,
				"setup_evidence_schema": 1, "enrollment_dispatch": "may_have_been_sent", "activation_dispatch": "not_sent",
				"activation_mode": "automatic", "activation_succeeded": false, "seed_verified": false,
				"secret": completionFixtureSeed, "session_id": "setup-list-session", "new_totp_factor_id": "setup-list-factor",
			}
			if err := protocol.WritePrivateJSON(path, candidate); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			emit(JobEvent{Stage: "totp_setup_otp", Status: StatusWaitingInput})
			waiting, _ := m.Get(owner.ID)
			if waiting.OperationStatus != "waiting" {
				t.Fatal("synthetic setup did not enter its manual code wait")
			}
			assertState(true, true)
			if canceled, err := m.CancelWithError(owner.ID); !canceled || err != nil {
				t.Fatalf("cancel: accepted=%v error=%v", canceled, err)
			}
			assertState(true, true)
			release()
			waitForStatus(t, m, owner.ID, StatusImported)
			assertState(false, true)
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("setup presentation consumed or changed protected candidate evidence", err)
			}
		})
	}
}

func TestTOTPListOwnOperationAtFollowsNewAttemptOnSameJob(t *testing.T) {
	m, _, owner, peer := resetManagerFixture(t)
	first, err := m.QueueOperation(owner.ID, "reset-2fa", nil)
	if err != nil {
		t.Fatal(err)
	}
	firstState := m.TOTPListStates([]string{owner.ID})[owner.ID]
	if firstState.OwnOperationAt.IsZero() || !firstState.OwnOperationAt.Equal(first.LastOperationAt) {
		t.Fatal("first real admission did not publish its own operation generation")
	}
	if canceled, err := m.CancelWithError(owner.ID); !canceled || err != nil {
		t.Fatalf("cancel: accepted=%v error=%v", canceled, err)
	}
	if !m.TOTPListStates([]string{owner.ID})[owner.ID].OwnOperationAt.IsZero() {
		t.Fatal("ended admission retained its operation ownership timestamp")
	}
	second, err := m.QueueOperation(owner.ID, "reset-2fa", nil)
	if err != nil {
		t.Fatal(err)
	}
	states := assertTOTPListMatchesIndividual(t, m, owner.ID, peer.ID)
	if !states[owner.ID].OwnOperationAt.Equal(second.LastOperationAt) || states[owner.ID].OwnOperationAt.Equal(first.LastOperationAt) {
		t.Fatal("new same-ID admission reused the previous row's presentation generation")
	}
	if !states[peer.ID].OperationInFlight || !states[peer.ID].OwnOperationAt.IsZero() {
		t.Fatal("same-email peer acquired the new owner's own-operation evidence")
	}
	if !firstState.OwnOperationAt.Equal(first.LastOperationAt) {
		t.Fatal("a later admission mutated the earlier detached presentation snapshot")
	}
}
