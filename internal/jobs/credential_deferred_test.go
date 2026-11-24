package jobs

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"tosub2-go-port/internal/accountlock"
	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/privatestate"
)

func TestActiveCredentialEditDefersWriteAndSurvivesVerifiedHook(t *testing.T) {
	const origin, email = "https://deferred-edit.example.test", "deferred-edit@example.test"
	vault := &deferredCredentialVault{MemoryStore: credentials.NewMemoryStore()}
	m, _ := sharedVaultWriterManager(t, vault, origin)
	ready, mirror, mirrored, cleanup := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	var mirrorOnce, cleanupOnce sync.Once
	finishMirror := func() { mirrorOnce.Do(func() { close(mirror) }) }
	finishCleanup := func() { cleanupOnce.Do(func() { close(cleanup) }) }
	t.Cleanup(func() { finishMirror(); finishCleanup() })
	next := make(chan *Job, 1)
	var runs atomic.Int32
	verified := credentials.Credentials{Password: "old-verified-password", TOTPSecret: "NEWLYVERIFIEDSEED", TOTPFactorID: "verified-factor", ProxyURL: "http://rotated.invalid:8080", MailAPIURL: "https://mail.invalid/verified", MailMethod: "POST", MailBody: "verified-body", PendingTOTPSecret: "PEERPENDINGSEED", PendingTOTPURI: "pending-uri", PendingTOTPFactorID: "pending-factor"}
	m.SetRunner(func(ctx context.Context, job *Job, _ func(JobEvent)) error {
		release, err := accountlock.Acquire(origin, job.Email)
		if err != nil {
			return err
		}
		defer release()
		if runs.Add(1) != 1 {
			next <- job
			return nil
		}
		// The old protocol has already saved/read back verified material, but
		// its manager mirror callback has not acquired the manager lock yet.
		if err := vault.MemoryStore.Save(job.Email, verified); err != nil {
			return err
		}
		close(ready)
		<-mirror
		m.UpdateStoredCredentials(job.ID, verified)
		close(mirrored)
		<-cleanup
		return ctx.Err()
	})
	job, _, err := m.SubmitUniqueEmail(Spec{Email: email, Input: map[string]any{"password": "old-verified-password", "totpSecret": "OLDSEED", "proxyUrl": "http://old.invalid:8080"}})
	if err != nil {
		t.Fatal(err)
	}
	m.Start()
	waitDeferredSignal(t, ready, "first runner")
	beforeWrites := vault.writes.Load()
	delta := map[string]any{"password": "  new operator password  ", "proxyUrl": "", "mailRequestBody": "operator-body"}
	updated, created, err := m.SubmitUniqueEmail(Spec{Email: email, Input: delta})
	if err != nil || created {
		t.Fatal("active duplicate edit lost its accepted restart contract", err)
	}
	stored, err := vault.Load(email)
	if err != nil || !credentials.EqualReadback(verified, stored) || vault.writes.Load() != beforeWrites {
		t.Error("active edit wrote credentials while the old protocol owned the account", err)
	}
	if updated.LastError == "" {
		t.Error("accepted edit did not explain that credential persistence waits for cleanup")
	}
	finishMirror()
	waitDeferredSignal(t, mirrored, "late verified-credential mirror")
	input, _ := m.InputCopy(job.ID)
	for key, value := range delta {
		if !reflect.DeepEqual(input[key], value) {
			t.Errorf("late old callback replaced a newly accepted draft field: %s", key)
		}
	}
	if input["totpSecret"] != verified.TOTPSecret || input["mailApiUrl"] != verified.MailAPIURL {
		t.Error("mirror protection discarded verified fields that the operator did not edit")
	}
	select {
	case <-next:
		t.Fatal("replacement runner overlapped old resource cleanup")
	default:
	}
	finishCleanup()
	var second *Job
	select {
	case second = <-next:
	case <-time.After(3 * time.Second):
		t.Fatal("accepted edited operation did not restart after cleanup")
	}
	for key, value := range delta {
		if !reflect.DeepEqual(second.Input[key], value) {
			t.Errorf("restart did not receive the accepted draft field: %s", key)
		}
	}
	want := verified
	want.Password, want.ProxyURL, want.MailBody = delta["password"].(string), "", "operator-body"
	stored, err = vault.Load(email)
	if err != nil || !credentials.EqualReadback(want, stored) || vault.writes.Load() != beforeWrites+1 {
		t.Error("cleanup did not merge the explicit delta once into the latest credential record", err)
	}
	completed := waitForStatus(t, m, job.ID, StatusSucceeded)
	if completed.Attempts != 2 || completed.LastError != "" {
		t.Error("deferred success lost the restart count or retained a stale pending-save warning")
	}
}

func TestActiveCredentialEditCleanupFailureStopsRestart(t *testing.T) {
	for _, failure := range []string{"peer-busy", "save-failure", "readback-mismatch", "metadata-failure"} {
		t.Run(failure, func(t *testing.T) {
			const origin, email = "https://deferred-failure.example.test", "deferred-failure@example.test"
			vault := &deferredCredentialVault{MemoryStore: credentials.NewMemoryStore()}
			m, _ := sharedVaultWriterManager(t, vault, origin)
			ready, cleanup, released, peerReady := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
			var cleanupOnce, peerOnce sync.Once
			finishCleanup := func() { cleanupOnce.Do(func() { close(cleanup) }) }
			finishPeer := func() { peerOnce.Do(func() { close(peerReady) }) }
			t.Cleanup(func() { finishCleanup(); finishPeer() })
			var runs atomic.Int32
			m.SetRunner(func(ctx context.Context, job *Job, _ func(JobEvent)) error {
				release, err := accountlock.Acquire(origin, job.Email)
				if err != nil {
					return err
				}
				defer release()
				if runs.Add(1) != 1 {
					return nil
				}
				close(ready)
				<-ctx.Done()
				<-cleanup
				if err := release(); err != nil {
					return err
				}
				close(released)
				<-peerReady
				return ctx.Err()
			})
			job, _, err := m.SubmitUniqueEmail(Spec{Email: email, Input: map[string]any{"password": "old-password", "totpSecret": "RETAINEDSEED"}})
			if err != nil {
				t.Fatal(err)
			}
			m.Start()
			waitDeferredSignal(t, ready, "active runner")
			if _, _, err := m.SubmitUniqueEmail(Spec{Email: email, Input: map[string]any{"password": "new-operator-password"}}); err != nil {
				t.Fatal("active edit rejected before its deferred cleanup", err)
			}
			beforeWrites := vault.writes.Load()
			if failure == "save-failure" || failure == "readback-mismatch" {
				vault.mu.Lock()
				vault.failure = failure
				vault.mu.Unlock()
			}
			if failure == "metadata-failure" {
				m.mu.Lock()
				m.metadataWriter = func(context.Context, string, any) error { return os.ErrPermission }
				m.mu.Unlock()
				t.Cleanup(func() { m.mu.Lock(); m.metadataWriter = nil; m.mu.Unlock() })
			}
			finishCleanup()
			waitDeferredSignal(t, released, "old runner account release")
			if failure == "peer-busy" {
				stopPeer := holdAccountQueryProcessLock(t, origin, email)
				defer stopPeer()
			}
			finishPeer()
			finished := waitDeferredFinished(t, m, job.ID)
			if finished.Status != StatusFailed || !finished.RecoveryPaused || finished.LastError == "" || finished.Input["password"] != "new-operator-password" || runs.Load() != 1 || m.QueuePosition(job.ID) != 0 {
				t.Error("cleanup failure lost the draft, hid the failed save, or restarted remote work")
			}
			if strings.Contains(finished.LastError, "new-operator-password") {
				t.Error("credential failure exposed draft material")
			}
			stored, loadErr := vault.Load(email)
			if loadErr != nil || stored.TOTPSecret != "RETAINEDSEED" {
				t.Error("deferred save damaged an unedited credential", loadErr)
			}
			if failure == "peer-busy" || failure == "save-failure" || failure == "readback-mismatch" {
				if stored.Password != "old-password" {
					t.Error("unconfirmed deferred save replaced the original password")
				}
			} else if stored.Password != "new-operator-password" {
				t.Error("metadata failure discarded an already confirmed protected save")
			}
			wantWrites := beforeWrites + 1
			if failure == "peer-busy" {
				wantWrites = beforeWrites
			}
			if vault.writes.Load() != wantWrites {
				t.Error("deferred save was replayed or crossed another owner's lock")
			}
		})
	}
}

func TestActiveCredentialEditRejectsNewRecoveryEvidenceAtCleanup(t *testing.T) {
	for _, evidence := range []string{"shared-reset-quarantine", "local-setup-journal"} {
		t.Run(evidence, func(t *testing.T) {
			const origin, email = "https://deferred-recovery.example.test", "deferred-recovery@example.test"
			vault := &deferredCredentialVault{MemoryStore: credentials.NewMemoryStore()}
			m, st := sharedVaultWriterManager(t, vault, origin)
			ready, complete, published := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var completeOnce sync.Once
			finish := func() { completeOnce.Do(func() { close(complete) }) }
			t.Cleanup(finish)
			var runs atomic.Int32
			pending := credentials.Credentials{Password: "old-password", TOTPSecret: "CONFIRMEDOLDSEED", TOTPFactorID: "old-factor", PendingTOTPSecret: "UNCONFIRMEDNEWSEED", PendingTOTPURI: "pending-uri", PendingTOTPFactorID: "new-factor"}
			if evidence == "shared-reset-quarantine" {
				pending.TOTPResetPending, pending.TOTPResetOperationID = true, "new-reset-operation"
			}
			var journalPath string
			var journalBytes []byte
			m.SetRunner(func(ctx context.Context, job *Job, _ func(JobEvent)) error {
				release, err := accountlock.Acquire(origin, job.Email)
				if err != nil {
					return err
				}
				defer release()
				if runs.Add(1) != 1 {
					return errors.New("unexpected second authentication")
				}
				close(ready)
				<-ctx.Done()
				<-complete
				if err := vault.MemoryStore.Save(email, pending); err != nil {
					return err
				}
				if evidence == "local-setup-journal" {
					registerCompletionFixture(t, st.Root)
					journalPath = filepath.Join(st.Root, job.ID, "totp-setup-result.json")
					if err := privatestate.WriteJSON(t.Context(), journalPath, "totp-reset", email, "new-setup-operation", map[string]any{
						"version": 1, "email": email, "operation_id": "new-setup-operation", "secret": pending.PendingTOTPSecret,
						"otpauth_uri": pending.PendingTOTPURI, "new_totp_factor_id": pending.PendingTOTPFactorID,
						"session_id": "new-enrollment-session", "setup_evidence_schema": 1,
						"enrollment_dispatch": "may_have_been_sent", "activation_dispatch": "not_sent", "activation_succeeded": false,
					}); err != nil {
						return err
					}
					journalBytes, err = os.ReadFile(journalPath)
					if err != nil {
						return err
					}
				}
				// Quarantine is allowed to invalidate current-key presentation;
				// the separately accepted draft still has to survive cleanup.
				m.UpdateStoredCredentials(job.ID, pending)
				close(published)
				return ctx.Err()
			})
			job, _, err := m.SubmitUniqueEmail(Spec{Email: email, Input: map[string]any{"password": "old-password", "totpSecret": "CONFIRMEDOLDSEED"}})
			if err != nil {
				t.Fatal(err)
			}
			m.Start()
			waitDeferredSignal(t, ready, "active runner before recovery publication")
			if _, _, err := m.SubmitUniqueEmail(Spec{Email: email, Input: map[string]any{"password": "new-draft-password", "totpSecret": "NEWOPERATORSEED"}}); err != nil {
				t.Fatal(err)
			}
			writes := vault.writes.Load()
			finish()
			waitDeferredSignal(t, published, "new recovery evidence")
			finished := waitDeferredFinished(t, m, job.ID)
			if finished.Status != StatusFailed || !finished.RecoveryPaused || finished.Input["password"] != "new-draft-password" || finished.Input["totpSecret"] != "NEWOPERATORSEED" || finished.LastError == "" || runs.Load() != 1 || m.QueuePosition(job.ID) != 0 {
				t.Error("new security evidence did not preserve the edit while stopping automatic work")
			}
			if evidence == "shared-reset-quarantine" && (finished.HasTOTPKey || !finished.TOTPRecoveryPending) {
				t.Error("retained draft was presented as an available active TOTP key")
			}
			retained, loadErr := vault.Load(email)
			if loadErr != nil || !credentials.EqualReadback(pending, retained) || vault.writes.Load() != writes {
				t.Error("cleanup rewrote newly published recovery material", loadErr)
			}
			if journalPath != "" {
				after, readErr := os.ReadFile(journalPath)
				if readErr != nil || !bytes.Equal(journalBytes, after) {
					t.Error("cleanup changed the protected pending setup journal", readErr)
				}
			}
		})
	}
}

func TestActiveCredentialInputCleanupDoesNotPromotePasswordCandidate(t *testing.T) {
	const origin, email = "https://deferred-input.example.test", "deferred-input@example.test"
	vault := &deferredCredentialVault{MemoryStore: credentials.NewMemoryStore()}
	m, _ := sharedVaultWriterManager(t, vault, origin)
	ready, complete := make(chan struct{}), make(chan struct{})
	var once sync.Once
	finish := func() { once.Do(func() { close(complete) }) }
	t.Cleanup(finish)
	m.SetRunner(func(ctx context.Context, job *Job, emit func(JobEvent)) error {
		release, err := accountlock.Acquire(origin, job.Email)
		if err != nil {
			return err
		}
		defer release()
		emit(JobEvent{Stage: "password", Status: "awaiting_input"})
		close(ready)
		<-complete
		if password, ok := m.ConsumeInput(job.ID, "password"); !ok || password != "  rejected manual candidate  " {
			t.Error("active input did not retain the exact manual password candidate")
		}
		return errors.New("synthetic password rejection")
	})
	job, err := m.Submit(Spec{Email: email, Input: map[string]any{"password": "verified-password", "mailRequestBody": "old-body"}})
	if err != nil {
		t.Fatal(err)
	}
	m.Start()
	waitDeferredSignal(t, ready, "password prompt")
	writes := vault.writes.Load()
	if !m.InputForStage(job.ID, "password", map[string]any{"password": "  rejected manual candidate  ", "mailRequestBody": "operator-body", "email_otp": "123456"}) {
		t.Fatal("legacy authentication input was rejected")
	}
	if vault.writes.Load() != writes {
		t.Error("active input immediately wrote shared credentials")
	}
	finish()
	finished := waitForStatus(t, m, job.ID, StatusFailed)
	stored, err := vault.Load(email)
	if err != nil || stored.Password != "verified-password" || stored.MailBody != "operator-body" || vault.writes.Load() != writes+1 || finished.Attempts != 1 || finished.LastError != "" || !strings.Contains(finished.OperationError, "synthetic password rejection") {
		t.Error("cleanup promoted a rejected password, lost the independent edit, or restarted authentication", err)
	}
}

func TestActiveCredentialEditRefreshesUneditedFieldsAfterPeerCompletion(t *testing.T) {
	const origin, email = "https://deferred-peer-complete.example.test", "deferred-peer-complete@example.test"
	vault := &deferredCredentialVault{MemoryStore: credentials.NewMemoryStore()}
	m, _ := sharedVaultWriterManager(t, vault, origin)
	ready, released, peerDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var peerOnce sync.Once
	finishPeer := func() { peerOnce.Do(func() { close(peerDone) }) }
	t.Cleanup(finishPeer)
	next := make(chan *Job, 1)
	var runs atomic.Int32
	m.SetRunner(func(ctx context.Context, job *Job, _ func(JobEvent)) error {
		release, err := accountlock.Acquire(origin, job.Email)
		if err != nil {
			return err
		}
		defer release()
		if runs.Add(1) > 1 {
			next <- job
			return nil
		}
		close(ready)
		<-ctx.Done()
		if err := release(); err != nil {
			return err
		}
		close(released)
		<-peerDone
		return ctx.Err()
	})
	job, _, err := m.SubmitUniqueEmail(Spec{Email: email, Input: map[string]any{"password": "old-password", "totpSecret": "OLDSEED", "mailRequestBody": "old-body"}})
	if err != nil {
		t.Fatal(err)
	}
	m.Start()
	waitDeferredSignal(t, ready, "old authentication owner")
	if _, _, err := m.SubmitUniqueEmail(Spec{Email: email, Input: map[string]any{"mailRequestBody": "operator-body"}}); err != nil {
		t.Fatal(err)
	}
	waitDeferredSignal(t, released, "old authentication lock release")
	// Model another completed writer in the OS-lock handoff gap. Its latest
	// credential values were never mirrored into this manager's process Input.
	releasePeer, err := accountlock.Acquire(origin, email)
	if err != nil {
		t.Fatal(err)
	}
	peer := credentials.Credentials{Password: "peer-confirmed-password", TOTPSecret: "PEERCONFIRMEDSEED", TOTPFactorID: "peer-confirmed-factor", TOTPResetOperationID: "peer-completed-reset", ProxyURL: "http://peer-proxy.invalid:8080", MailBody: "peer-body"}
	err = vault.MemoryStore.Save(email, peer)
	releaseErr := releasePeer()
	if err != nil || releaseErr != nil {
		t.Fatal("peer fixture could not complete its owned commit", err, releaseErr)
	}
	finishPeer()
	var restarted *Job
	select {
	case restarted = <-next:
	case <-time.After(3 * time.Second):
		t.Fatal("edited operation did not restart after peer completion")
	}
	if restarted.Input["password"] != peer.Password || restarted.Input["totpSecret"] != peer.TOTPSecret || restarted.Input["proxyUrl"] != peer.ProxyURL || restarted.Input["mailRequestBody"] != "operator-body" {
		t.Error("replacement runner received stale unedited credentials after a peer commit")
	}
	completed := waitForStatus(t, m, job.ID, StatusSucceeded)
	want := peer
	want.MailBody = "operator-body"
	stored, err := vault.Load(email)
	if err != nil || !credentials.EqualReadback(want, stored) || completed.LastError != "" || completed.Attempts != 2 {
		t.Error("cleanup failed to keep the peer result and the independent operator edit", err)
	}
}

type deferredCredentialVault struct {
	*credentials.MemoryStore
	mu      sync.Mutex
	failure string
	writes  atomic.Int32
}

func (v *deferredCredentialVault) SaveContext(ctx context.Context, email string, value credentials.Credentials) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	v.writes.Add(1)
	v.mu.Lock()
	failure := v.failure
	v.mu.Unlock()
	if failure == "save-failure" {
		return errors.New("synthetic unavailable secure storage")
	}
	if failure == "readback-mismatch" {
		return nil
	}
	return v.MemoryStore.Save(email, value)
}

func (v *deferredCredentialVault) LoadContext(ctx context.Context, email string) (credentials.Credentials, error) {
	if err := ctx.Err(); err != nil {
		return credentials.Credentials{}, err
	}
	return v.MemoryStore.Load(email)
}

func (v *deferredCredentialVault) DeleteContext(ctx context.Context, email string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return v.MemoryStore.Delete(email)
}

func waitDeferredSignal(t *testing.T, ready <-chan struct{}, label string) {
	t.Helper()
	select {
	case <-ready:
	case <-time.After(3 * time.Second):
		t.Fatalf("did not reach %s", label)
	}
}

func waitDeferredFinished(t *testing.T, m *Manager, id string) *Job {
	t.Helper()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	for {
		job, ok := m.Get(id)
		if ok && job.FinishedAt != nil {
			return job
		}
		select {
		case <-deadline.C:
			t.Fatal("deferred credential operation did not finish")
		case <-time.After(time.Millisecond):
		}
	}
}
