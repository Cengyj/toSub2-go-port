package jobs

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"tosub2-go-port/internal/credentials"
)

type capturedCredentialVault struct {
	credentials.Store
	calls   atomic.Int32
	read    chan struct{}
	release chan struct{}
}

func (v *capturedCredentialVault) Load(email string) (credentials.Credentials, error) {
	value, err := v.Store.Load(email)
	if v.calls.Add(1) == 1 {
		close(v.read)
		<-v.release
	}
	return value, err
}

func TestCredentialReadRejectsCompletedInPlaceTOTPReplacement(t *testing.T) {
	for _, readPeer := range []bool{false, true} {
		t.Run(map[bool]string{false: "same-job", true: "same-email-history"}[readPeer], func(t *testing.T) {
			m, vault, owner, peer := resetManagerFixture(t)
			delayed := &capturedCredentialVault{Store: vault, read: make(chan struct{}), release: make(chan struct{})}
			m.SetCredentialStore(delayed)
			readID := owner.ID
			if readPeer {
				readID = peer.ID
			}
			type answer struct {
				value credentials.Credentials
				err   error
			}
			done := make(chan answer, 1)
			go func() {
				value, err := m.LoadCredentialsContext(t.Context(), readID)
				done <- answer{value, err}
			}()
			select {
			case <-delayed.read:
			case <-time.After(time.Second):
				t.Fatal("credential read did not reach controlled delay")
			}
			if _, err := m.QueueOperation(owner.ID, "reset-2fa", nil); err != nil {
				t.Fatal(err)
			}
			if err := beginTOTPResetFixture(t, m, t.Context(), owner.ID, "later-operation"); err != nil {
				t.Fatal(err)
			}
			current, _ := vault.Load(owner.Email)
			current.TOTPSecret, current.TOTPResetPending = completionFixtureSeed, false
			if err := vault.Save(owner.Email, current); err != nil {
				t.Fatal(err)
			}
			if err := commitTOTPResetFixture(t, m, t.Context(), owner.ID, current); err != nil {
				t.Fatal(err)
			}
			if err := markCommittedResetFixture(t, m, owner.ID, current.TOTPResetOperationID); err != nil {
				t.Fatal(err)
			}
			m.Cancel(owner.ID)
			m.Emit(JobEvent{JobID: owner.ID, Status: StatusSucceeded})
			if m.TOTPExportPending(readID) {
				t.Fatal("fixture still has pending recovery; stale read would be rejected independently")
			}
			close(delayed.release)
			got := <-done
			if !errors.Is(got.err, ErrCredentialReadSuperseded) || !credentials.EqualReadback(got.value, credentials.Credentials{}) {
				t.Fatal("completed reset allowed an earlier seed to escape")
			}
			latest, err := m.LoadCredentialsContext(t.Context(), readID)
			if err != nil || latest.TOTPSecret != completionFixtureSeed {
				t.Fatal("fresh read could not obtain the committed seed", err)
			}
		})
	}
}

func TestCredentialReadRejectsVaultReplacement(t *testing.T) {
	m, vault, owner, _ := resetManagerFixture(t)
	delayed := &capturedCredentialVault{Store: vault, read: make(chan struct{}), release: make(chan struct{})}
	m.SetCredentialStore(delayed)
	done := make(chan error, 1)
	go func() { _, err := m.LoadCredentials(owner.ID); done <- err }()
	<-delayed.read
	m.SetCredentialStore(credentials.NewMemoryStore())
	close(delayed.release)
	if err := <-done; !errors.Is(err, ErrCredentialReadSuperseded) {
		t.Fatal("old vault read survived store replacement", err)
	}
}
