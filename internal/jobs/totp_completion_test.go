package jobs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"tosub2-go-port/internal/accountlock"
	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/privatestate"
	"tosub2-go-port/internal/protocol"
)

func localCompletionFixture(t *testing.T, source string) (*Manager, *memoryVault, *Job, *Job, string) {
	t.Helper()
	m, v, j, p, path := pendingResetFixture(t)
	out := completionFixtureResult(j.Email, "fixture-op", completionFixtureSeed, source, false)
	if err := protocol.WritePrivateJSON(path, out); err != nil {
		t.Fatal(err)
	}
	c, _ := v.Load(j.Email)
	c.PendingTOTPSecret = completionFixtureSeed
	if err := v.Save(j.Email, c); err != nil {
		t.Fatal(err)
	}
	return m, v, j, p, path
}

func finishLocalCompletionFixture(t *testing.T, m *Manager, v *memoryVault, j *Job, path string) error {
	t.Helper()
	out, err := protocol.ReadTOTPResetCompletion(path, j.Email)
	if err != nil {
		return err
	}
	c, err := v.Load(j.Email)
	if err != nil {
		return err
	}
	c.TOTPSecret, c.PendingTOTPSecret, c.PendingTOTPURI, c.TOTPResetPending = resultString(out, "secret"), "", "", false
	if err := v.Save(j.Email, c); err != nil {
		return err
	}
	if err := m.CommitTOTPReset(t.Context(), j.ID, c); err != nil {
		return err
	}
	if err := protocol.MarkTOTPResetCredentialPersisted(path, j.Email, c.TOTPResetOperationID); err != nil {
		return err
	}
	out, err = protocol.ReadTOTPResetCompletion(path, j.Email)
	if err != nil {
		return err
	}
	if err := m.UpdateTOTPResetResult(j.ID, out, nil); err != nil {
		return err
	}
	return m.MarkTOTPResetCommitted(t.Context(), j.ID, c.TOTPResetOperationID)
}

func TestTOTPCompleteAcceptsBothActualConfirmationSources(t *testing.T) {
	for _, source := range []string{"business_receipt", "state_read"} {
		t.Run(source, func(t *testing.T) {
			m, v, j, p, path := localCompletionFixture(t, source)
			before, _ := v.Load(j.Email)
			journalBefore, _ := os.ReadFile(path)
			queued, err := m.QueueOperation(j.ID, "complete-2fa", map[string]any{"totpIntentId": "completion-intent-123"})
			if err != nil {
				t.Fatal(err)
			}
			after, _ := v.Load(j.Email)
			journalAfter, _ := os.ReadFile(path)
			if !reflect.DeepEqual(before, after) || string(journalBefore) != string(journalAfter) {
				t.Fatal("admission changed private material")
			}
			if queued.Operation != "complete-2fa" || queued.ReloginAfterTOTP || queued.UploadAfterTOTP || queued.LogoutAllDevicesAfterTOTP {
				t.Fatal("completion inherited remote follow-ups")
			}
			if err := m.StartTOTPWorkflowStep(t.Context(), j.ID, "totp"); err != nil {
				t.Fatal(err)
			}
			if err := finishLocalCompletionFixture(t, m, v, j, path); err != nil {
				t.Fatal(err)
			}
			if err := m.FinishTOTPWorkflow(j.ID, nil); err != nil {
				t.Fatal(err)
			}
			for _, id := range []string{j.ID, p.ID} {
				got, _ := m.Get(id)
				if got.TOTPRecoveryPending || got.TOTPCredentialInvalidated || !got.HasTOTPKey {
					t.Fatal("same-operation quarantine not released")
				}
			}
			out, err := protocol.ReadTOTPResetCompletion(path, j.Email)
			if err != nil {
				t.Fatal(err)
			}
			if out["activation_confirmation_source"] != source || (source == "business_receipt" && out["activation_confirmed_by_read"] == true) {
				t.Fatal("completion fabricated confirmation source")
			}
			m.mu.Lock()
			m.removePendingLocked(j.ID)
			m.jobs[j.ID].Status = StatusSucceeded
			m.persistLocked()
			m.mu.Unlock()
			restored := NewManager(t.Context(), 1, m.store, nil)
			restored.SetCredentialStore(v)
			if err := restored.Restore(t.Context()); err != nil {
				t.Fatal(err)
			}
			got, _ := restored.Get(j.ID)
			if !got.TOTPResetCommitted || got.TOTPRecoveryPending || got.TOTPWorkflowStatus != "succeeded" {
				t.Fatal("durable completion lost on restart")
			}
			replay, err := restored.QueueOperation(j.ID, "complete-2fa", map[string]any{"totpIntentId": "completion-intent-123"})
			if err != nil || replay.ID != j.ID || len(restored.pending) != 0 {
				t.Fatal("completed intent replay queued work", err)
			}
		})
	}
}

func TestTOTPCompleteRejectsOptionsAndInvalidIdentity(t *testing.T) {
	for _, key := range []string{"password", "totpSecret", "totpReauthenticate", "resetTotp", "reloginAfterTotp", "logoutAllDevicesAfterTotp", "uploadAfterTotp", "authorizationMethod", "proxyUrl"} {
		t.Run(key, func(t *testing.T) {
			m, v, j, _, path := localCompletionFixture(t, "business_receipt")
			before, _ := v.Load(j.Email)
			journal, _ := os.ReadFile(path)
			if _, err := m.QueueOperation(j.ID, "complete-2fa", map[string]any{key: true}); err == nil {
				t.Fatal("forbidden option accepted")
			}
			after, _ := v.Load(j.Email)
			retained, _ := os.ReadFile(path)
			if !reflect.DeepEqual(before, after) || string(journal) != string(retained) || len(m.pending) != 0 {
				t.Fatal("rejected request changed state")
			}
		})
	}
	for _, kind := range []string{"unknown", "wrong-operation", "missing-operation", "wrong-seed", "missing-seed", "wrong-uri", "foreign-email", "forged-receipt", "contradictory-evidence", "memory-only"} {
		t.Run(kind, func(t *testing.T) {
			m, v, j, _, path := localCompletionFixture(t, "business_receipt")
			out, _ := protocol.ReadTOTPResetCompletion(path, j.Email)
			c, _ := v.Load(j.Email)
			switch kind {
			case "unknown":
				out["activation_succeeded"] = false
			case "wrong-operation":
				c.TOTPResetOperationID = "different-operation"
			case "missing-operation":
				m.jobs[j.ID].TOTPResetOperationID = ""
			case "wrong-seed":
				c.PendingTOTPSecret = "KRSXG5DSNFXGOIDB"
			case "missing-seed":
				c.PendingTOTPSecret = ""
			case "wrong-uri":
				c.PendingTOTPURI = "otpauth://totp/other"
			case "foreign-email":
				out["email"] = "foreign@example.test"
			case "forged-receipt":
				out["activation_receipt"].(map[string]any)["attempt_id"] = "foreign-attempt"
			case "contradictory-evidence":
				out["activation_confirmed_by_read"] = true
			case "memory-only":
				m.SetCredentialStore(credentials.NewMemoryStore())
			}
			if err := v.Save(j.Email, c); err != nil {
				t.Fatal(err)
			}
			if err := protocol.WritePrivateJSON(path, out); err != nil {
				t.Fatal(err)
			}
			before, _ := v.Load(j.Email)
			if err := m.CanCompleteTOTPReset(j.ID); err == nil {
				t.Fatal("invalid completion offered")
			}
			if _, err := m.QueueOperation(j.ID, "complete-2fa", nil); err == nil {
				t.Fatal("invalid completion queued")
			}
			after, _ := v.Load(j.Email)
			if !reflect.DeepEqual(before, after) || len(m.pending) != 0 {
				t.Fatal("invalid completion changed state")
			}
		})
	}
}

func TestTOTPCompleteReplayStillRejectsForbiddenOptions(t *testing.T) {
	m, _, j, _, _ := localCompletionFixture(t, "business_receipt")
	if _, err := m.QueueOperation(j.ID, "complete-2fa", map[string]any{"totpIntentId": "completion-intent-replay"}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.QueueOperation(j.ID, "complete-2fa", map[string]any{"totpIntentId": "completion-intent-replay", "password": "synthetic"}); err == nil {
		t.Fatal("deduplication bypassed strict local-completion body")
	}
	if len(m.pending) != 1 {
		t.Fatal("replay duplicated queue")
	}
}

func TestTOTPFinalMetadataFailureRetainsQuarantine(t *testing.T) {
	m, v, j, p, path := localCompletionFixture(t, "state_read")
	c, _ := v.Load(j.Email)
	c.TOTPSecret, c.PendingTOTPSecret, c.TOTPResetPending = completionFixtureSeed, "", false
	if err := v.Save(j.Email, c); err != nil {
		t.Fatal(err)
	}
	if err := m.CommitTOTPReset(t.Context(), j.ID, c); err != nil {
		t.Fatal(err)
	}
	if err := protocol.MarkTOTPResetCredentialPersisted(path, j.Email, c.TOTPResetOperationID); err != nil {
		t.Fatal(err)
	}
	m.metadataWriter = func(context.Context, string, any) error { return errors.New("synthetic metadata fault") }
	if err := m.MarkTOTPResetCommitted(t.Context(), j.ID, c.TOTPResetOperationID); err == nil {
		t.Fatal("metadata failure hidden")
	}
	for _, id := range []string{j.ID, p.ID} {
		got, _ := m.Get(id)
		if !got.TOTPRecoveryPending || !got.TOTPCredentialInvalidated || got.TOTPResetCommitted {
			t.Fatal("failed metadata commit released quarantine")
		}
	}
	if _, err := protocol.ReadTOTPResetCompletion(path, j.Email); err != nil {
		t.Fatal("failure removed completion proof", err)
	}
	m.metadataWriter = nil
	if err := m.MarkTOTPResetCommitted(t.Context(), j.ID, c.TOTPResetOperationID); err != nil {
		t.Fatal(err)
	}
}

func TestTOTPFinalCommitNeverClearsUnrelatedOperation(t *testing.T) {
	m, v, j, p, path := localCompletionFixture(t, "business_receipt")
	m.jobs[p.ID].TOTPResetOperationID = "unrelated-unknown-operation"
	if err := finishLocalCompletionFixture(t, m, v, j, path); err != nil {
		t.Fatal(err)
	}
	peer, _ := m.Get(p.ID)
	if !peer.TOTPRecoveryPending || !peer.TOTPCredentialInvalidated || peer.TOTPResetOperationID != "unrelated-unknown-operation" {
		t.Fatal("completion cleared unrelated unknown operation")
	}
	if err := m.CheckTOTPExport(j.ID); !errors.Is(err, ErrTOTPRecoveryPending) {
		t.Fatal("unrelated quarantine no longer protects account", err)
	}
}

func TestTOTPStartupOnlyProjectsConfirmedEvidenceAndHonorsAccountLease(t *testing.T) {
	m, v, j, _, path := localCompletionFixture(t, "business_receipt")
	before, _ := v.Load(j.Email)
	journal, _ := os.ReadFile(path)
	release, err := accountlock.Acquire("https://chatgpt.com", j.Email)
	if err != nil {
		t.Fatal(err)
	}
	candidate, _ := m.Get(j.ID)
	_, err = recoverPrivateResult(t.Context(), m.store.Root, candidate, v, nil)
	release()
	if !errors.Is(err, accountlock.ErrBusy) {
		t.Fatal("startup ignored normal account lease", err)
	}
	restored := NewManager(t.Context(), 1, m.store, nil)
	restored.SetCredentialStore(v)
	if err := restored.Restore(t.Context()); err != nil {
		t.Fatal(err)
	}
	got, _ := restored.Get(j.ID)
	after, _ := v.Load(j.Email)
	retained, _ := os.ReadFile(path)
	if got.TOTPResetCommitted || !got.TOTPRecoveryPending || len(restored.pending) != 0 || !reflect.DeepEqual(before, after) || string(journal) != string(retained) {
		t.Fatal("startup completed or replayed an interrupted operation")
	}
	if err := restored.CanCompleteTOTPReset(j.ID); err != nil {
		t.Fatal("valid local completion lost after restart", err)
	}
}

func TestSetupUnknownEnrollmentWithoutSecretCannotReplay(t *testing.T) {
	// An independent first-setup record may exist before a seed was returned.
	m, v, j, _ := resetManagerFixture(t)
	if _, err := m.QueueOperation(j.ID, "setup-2fa", nil); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(m.store.Root, j.ID, "totp-setup-result.json")
	out := map[string]any{"version": 1, "email": j.Email, "operation_id": "unknown-first-enrollment", "activation_succeeded": false, "already_enabled": false}
	if err := protocol.WritePrivateJSON(path, out); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	m.removePendingLocked(j.ID)
	m.jobs[j.ID].Status = StatusFailed
	m.persistLocked()
	m.mu.Unlock()
	if m.Input(j.ID, map[string]any{"totpSecret": "SYNTHETIC_INPUT"}) {
		t.Fatal("generic input requeued unknown enrollment")
	}
	for _, op := range []string{"setup-2fa", "reset-2fa", "relogin", "complete-2fa", "recover-2fa", "inspect-2fa"} {
		if _, err := m.QueueOperation(j.ID, op, nil); err == nil {
			t.Fatal("unknown enrollment replayed", op)
		}
	}
	restored := NewManager(t.Context(), 1, m.store, nil)
	restored.SetCredentialStore(v)
	if err := restored.Restore(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(restored.pending) != 0 {
		t.Fatal("startup requeued unknown setup")
	}
	if _, err := restored.QueueOperation(j.ID, "setup-2fa", nil); err == nil {
		t.Fatal("restart permitted duplicate enrollment")
	}
	if _, err := restored.Retry(j.ID); err == nil {
		t.Fatal("retry permitted duplicate enrollment")
	}
	if restored.Input(j.ID, map[string]any{"password": "synthetic"}) {
		t.Fatal("generic input replayed unknown enrollment after startup")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("unknown material discarded")
	}
}

func TestTOTPLegacyConfirmedCompletionPreservesOriginalJournal(t *testing.T) {
	m, v, j, _, path := localCompletionFixture(t, "business_receipt")
	out, err := protocol.ReadTOTPResetCompletion(path, j.Email)
	if err != nil {
		t.Fatal(err)
	}
	delete(out, "operation_scope")
	out["recovery_schema"] = 1
	out["recovery_checkpoint"] = map[string]any{"legacy_session_provenance": true}
	out["credential_persisted"] = false
	if err := protocol.WritePrivateJSON(path, out); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.QueueOperation(j.ID, "complete-2fa", nil); err != nil {
		t.Fatal(err)
	}
	c, _ := v.Load(j.Email)
	c.TOTPSecret, c.PendingTOTPSecret, c.TOTPResetPending = completionFixtureSeed, "", false
	if err := v.Save(j.Email, c); err != nil {
		t.Fatal(err)
	}
	if err := m.CommitTOTPReset(t.Context(), j.ID, c); err != nil {
		t.Fatal(err)
	}
	if err := m.UpdateTOTPResetResult(j.ID, out, nil); err != nil {
		t.Fatal(err)
	}
	if err := m.MarkTOTPResetCommitted(t.Context(), j.ID, c.TOTPResetOperationID); err != nil {
		t.Fatal("legacy proof requires a fabricated journal persisted flag", err)
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(original) {
		t.Fatal("local completion rewrote immutable legacy evidence")
	}
	m.mu.Lock()
	m.removePendingLocked(j.ID)
	m.jobs[j.ID].Status = StatusSucceeded
	m.jobs[j.ID].TOTPWorkflowStatus = "succeeded"
	m.persistLocked()
	m.mu.Unlock()
	if _, err := m.QueueOperation(j.ID, "reset-2fa", map[string]any{"totpIntentId": "new-intent-legacy"}); err == nil {
		t.Fatal("legacy provenance journal was moved for a new reset")
	}
	after, _ = os.ReadFile(path)
	if string(after) != string(original) {
		t.Fatal("fresh reset admission changed legacy provenance")
	}
	restored := NewManager(t.Context(), 1, m.store, nil)
	restored.SetCredentialStore(v)
	if err := restored.Restore(t.Context()); err != nil {
		t.Fatal(err)
	}
	got, _ := restored.Get(j.ID)
	if !got.TOTPResetCommitted || got.TOTPRecoveryPending {
		t.Fatal("immutable completed legacy proof did not restore")
	}
}

func TestExplicitNewResetArchivesOnlyCompletedCurrentSessionProof(t *testing.T) {
	for _, scenario := range []string{"accepted", "missing-intent", "same-intent", "metadata-failure", "different-vault", "account-busy", "matching-archive", "conflicting-archive"} {
		t.Run(scenario, func(t *testing.T) {
			m, v, j, _, path := localCompletionFixture(t, "business_receipt")
			m.jobs[j.ID].TOTPIntentID = "prior-reset-intent"
			if err := finishLocalCompletionFixture(t, m, v, j, path); err != nil {
				t.Fatal(err)
			}
			m.mu.Lock()
			m.removePendingLocked(j.ID)
			m.jobs[j.ID].Status = StatusSucceeded
			m.jobs[j.ID].TOTPWorkflowStatus = "succeeded"
			m.persistLocked()
			m.mu.Unlock()
			original, _ := os.ReadFile(path)
			out, _ := protocol.ReadTOTPResetCompletion(path, j.Email)
			values := map[string]any{"totpIntentId": "explicit-new-reset-intent"}
			switch scenario {
			case "matching-archive", "conflicting-archive":
				digest := sha256.Sum256([]byte(resultString(out, "operation_id")))
				archivePath := filepath.Join(filepath.Dir(path), "totp-completed-"+hex.EncodeToString(digest[:])+".json")
				archived := cloneValue(out).(map[string]any)
				if scenario == "conflicting-archive" {
					archived["fixture_conflict"] = true
				}
				if err := privatestate.WriteJSON(t.Context(), archivePath, "totp-reset", j.Email, resultString(out, "operation_id"), archived); err != nil {
					t.Fatal(err)
				}
			case "missing-intent":
				values = nil
			case "same-intent":
				values["totpIntentId"] = "prior-reset-intent"
			case "metadata-failure":
				m.metadataWriter = func(context.Context, string, any) error { return errors.New("synthetic admission metadata failure") }
			case "different-vault":
				c, _ := v.Load(j.Email)
				c.TOTPResetOperationID = "foreign-later-operation"
				if err := v.Save(j.Email, c); err != nil {
					t.Fatal(err)
				}
			case "account-busy":
				release, err := accountlock.Acquire("https://chatgpt.com", j.Email)
				if err != nil {
					t.Fatal(err)
				}
				defer release()
			}
			queued, err := m.QueueOperation(j.ID, "reset-2fa", values)
			if scenario != "accepted" && scenario != "matching-archive" {
				if err == nil {
					t.Fatal("unsafe or uncommitted fresh reset admitted")
				}
				retained, readErr := os.ReadFile(path)
				if readErr != nil || string(retained) != string(original) {
					t.Fatal("rejected admission changed original evidence", readErr)
				}
				if len(m.pending) != 0 {
					t.Fatal("rejected reset entered queue")
				}
				return
			}
			if err != nil || queued.TOTPResetCommitted || queued.TOTPResetOperationID != "" || len(m.pending) != 1 {
				t.Fatal("new reset did not begin fresh", err)
			}
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("old completed proof still blocks new protocol start")
			}
			archived, err := filepath.Glob(filepath.Join(filepath.Dir(path), "totp-completed-*.json"))
			if err != nil || len(archived) != 1 {
				t.Fatal("expected one retained completion archive", err)
			}
			proof, err := protocol.ReadTOTPResetCompletion(archived[0], j.Email)
			if err != nil || !reflect.DeepEqual(proof, out) {
				t.Fatal("archived proof is not readable with its destination binding", err)
			}
			if err := protocol.ValidateTOTPResetStart(path, j.Email); err != nil {
				t.Fatal("explicit new reset remains blocked", err)
			}
		})
	}
}

func TestHistoricalCompletionDoesNotBindToLaterVaultOperation(t *testing.T) {
	m, v, j, _, path := localCompletionFixture(t, "business_receipt")
	if err := finishLocalCompletionFixture(t, m, v, j, path); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	m.removePendingLocked(j.ID)
	m.jobs[j.ID].Status = StatusSucceeded
	m.jobs[j.ID].TOTPWorkflowStatus = "succeeded"
	m.persistLocked()
	m.mu.Unlock()
	current, _ := v.Load(j.Email)
	current.TOTPResetOperationID = "later-confirmed-operation"
	current.TOTPSecret = "KRSXG5DSNFXGOIDB"
	if err := v.Save(j.Email, current); err != nil {
		t.Fatal(err)
	}
	if err := m.CheckTOTPExport(j.ID); err != nil {
		t.Fatal("historical completion was invalidated by later vault operation", err)
	}
	restored := NewManager(t.Context(), 1, m.store, nil)
	restored.SetCredentialStore(v)
	if err := restored.Restore(t.Context()); err != nil {
		t.Fatal(err)
	}
	got, _ := restored.Get(j.ID)
	if !got.TOTPResetCommitted || got.TOTPRecoveryPending || got.TOTPResetOperationID != "fixture-op" {
		t.Fatal("historical operation was rebound or turned unknown")
	}
	after, _ := v.Load(j.Email)
	if !reflect.DeepEqual(after, current) {
		t.Fatal("restoring history changed later vault")
	}
}

func TestRemovedTOTPTasksNeverResumeOnStartup(t *testing.T) {
	for _, operation := range []string{"recover-2fa", "inspect-2fa"} {
		t.Run(operation, func(t *testing.T) {
			m, v, j, _ := resetManagerFixture(t)
			m.jobs[j.ID].Operation = operation
			m.jobs[j.ID].Status = StatusQueued
			m.persistLocked()
			restored := NewManager(t.Context(), 1, m.store, nil)
			restored.SetCredentialStore(v)
			if err := restored.Restore(t.Context()); err != nil {
				t.Fatal(err)
			}
			got, _ := restored.Get(j.ID)
			if got.Status != StatusFailed || len(restored.pending) != 0 {
				t.Fatal("removed operation resumed after startup")
			}
			if _, err := restored.Requeue(j.ID, operation); err == nil {
				t.Fatal("removed operation requeued")
			}
		})
	}
}

func TestLaterPeerResetPreservesHistoricalProofAndRefreshesCurrentInput(t *testing.T) {
	m, v, old, peer, path := localCompletionFixture(t, "business_receipt")
	if err := finishLocalCompletionFixture(t, m, v, old, path); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	m.removePendingLocked(old.ID)
	m.jobs[old.ID].Status = StatusSucceeded
	m.jobs[old.ID].TOTPWorkflowStatus = "succeeded"
	m.persistLocked()
	m.mu.Unlock()
	original, _ := os.ReadFile(path)
	if _, err := m.QueueOperation(peer.ID, "reset-2fa", map[string]any{"totpIntentId": "explicit-peer-reset"}); err != nil {
		t.Fatal(err)
	}
	if err := beginTOTPResetFixture(t, m, t.Context(), peer.ID, "later-peer-operation"); err != nil {
		t.Fatal(err)
	}
	nextPath := filepath.Join(m.store.Root, peer.ID, "totp-setup-result.json")
	next := completionFixtureResult(peer.Email, "later-peer-operation", "KRSXG5DSNFXGOIDB", "business_receipt", false)
	if err := protocol.WritePrivateJSON(nextPath, next); err != nil {
		t.Fatal(err)
	}
	c, _ := v.Load(peer.Email)
	c.PendingTOTPSecret = "KRSXG5DSNFXGOIDB"
	if err := v.Save(peer.Email, c); err != nil {
		t.Fatal(err)
	}
	if err := finishLocalCompletionFixture(t, m, v, peer, nextPath); err != nil {
		t.Fatal(err)
	}
	historical, _ := m.Get(old.ID)
	retained, _ := os.ReadFile(path)
	if historical.TOTPResetOperationID != "fixture-op" || !historical.TOTPResetCommitted || historical.TOTPRecoveryPending || historical.Input["totpSecret"] != "KRSXG5DSNFXGOIDB" || string(retained) != string(original) {
		t.Fatal("later reset changed historical operation or left a stale current login seed")
	}
}

func TestNewResetArchiveUsesConfiguredOrdinaryAccountLease(t *testing.T) {
	for _, failMetadata := range []bool{false, true} {
		t.Run(map[bool]string{false: "accepted", true: "metadata-failure"}[failMetadata], func(t *testing.T) {
			m, v, j, _, path := localCompletionFixture(t, "business_receipt")
			if err := finishLocalCompletionFixture(t, m, v, j, path); err != nil {
				t.Fatal(err)
			}
			m.jobs[j.ID].Status = StatusSucceeded
			m.jobs[j.ID].TOTPWorkflowStatus = "succeeded"
			m.persistLocked()
			productionRelease, err := accountlock.Acquire("https://chatgpt.com", j.Email)
			if err != nil {
				t.Fatal(err)
			}
			defer productionRelease()
			acquired, released := 0, 0
			m.SetAccountOperationLease(func(email string) (func() error, error) {
				acquired++
				if email != j.Email {
					t.Fatal("lease used foreign account")
				}
				release, err := accountlock.Acquire("https://fixture-custom-origin.example", email)
				if err != nil {
					return nil, err
				}
				return func() error { released++; return release() }, nil
			})
			if failMetadata {
				m.metadataWriter = func(context.Context, string, any) error { return errors.New("synthetic metadata fault") }
			}
			_, err = m.QueueOperation(j.ID, "reset-2fa", map[string]any{"totpIntentId": "configured-lease-new-intent"})
			if failMetadata && err == nil || !failMetadata && err != nil {
				t.Fatal("unexpected archive result", err)
			}
			if acquired != 1 || released != 1 {
				t.Fatal("ordinary configured lease not acquired and released exactly once", acquired, released)
			}
		})
	}
}
