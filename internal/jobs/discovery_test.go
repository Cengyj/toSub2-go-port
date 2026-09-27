package jobs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/store"
)

func discoveryFixture(t *testing.T, root, id, email, status string, output, checkpoint bool) {
	t.Helper()
	st, err := store.New(filepath.Join(root, id))
	if err != nil {
		t.Fatal(err)
	}
	write := func(name string, v any) {
		t.Helper()
		if err := st.WriteJSON(context.Background(), name, v); err != nil {
			t.Fatal(err)
		}
	}
	write("job-meta.json", map[string]any{"email": email, "status": status})
	if output {
		write("sub2api-import-oauth.json", map[string]any{"type": "sub2api-data", "accounts": []any{map[string]any{"credentials": map[string]any{"email": email}}}})
	}
	if checkpoint {
		write("login-checkpoint.json", map[string]any{"version": 1, "email": email, "stage": "email_verified"})
	}
}

func TestSyncOutputsDiscoversNewDirectoriesWithoutReloadingLiveJobs(t *testing.T) {
	st, _ := store.New(t.TempDir())
	m := NewManager(context.Background(), 1, st, nil)
	m.SetCredentialStore(credentials.NewMemoryStore())
	current, err := m.Submit(Spec{ID: "current", Email: "current@example.test", Input: map[string]any{"password": "Fresh-Password9!"}})
	if err != nil {
		t.Fatal(err)
	}
	discoveryFixture(t, st.Root, "current", current.Email, "failed", true, false)
	discoveryFixture(t, st.Root, "finished", "finished@example.test", "", true, false)
	discoveryFixture(t, st.Root, "checkpoint", "checkpoint@example.test", "phone_otp", false, true)
	discoveryFixture(t, st.Root, "queued", "queued@example.test", "queued", false, false)
	if err := os.Mkdir(filepath.Join(st.Root, "empty"), 0700); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	discoveryFixture(t, outside, "foreign", "foreign@example.test", "", true, false)
	if err := os.Symlink(filepath.Join(outside, "foreign"), filepath.Join(st.Root, "linked")); err != nil {
		t.Fatal(err)
	}
	if err := m.SyncOutputs(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if got := len(m.List()); got != 4 {
		t.Fatalf("jobs=%d; want existing plus three new records", got)
	}
	if got, _ := m.Get(current.ID); got.Status != StatusQueued || got.ResultSaved {
		t.Fatal("discovery replaced live job with old disk state")
	}
	if got, _ := m.Get("finished"); got.Status != StatusSucceeded || !got.ResultSaved {
		t.Fatal("completed CLI output not restored")
	}
	if got, _ := m.Get("checkpoint"); got.Status != "resume_available" {
		t.Fatal("checkpoint import lost resume state")
	}
	if got, _ := m.Get("queued"); got.Status != StatusQueued || m.QueuePosition("queued") < 1 {
		t.Fatal("queued import was not scheduled")
	}
	discoveryFixture(t, st.Root, "later", "later@example.test", "", true, false)
	if err := m.SyncOutputs(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if _, found := m.Get("later"); found {
		t.Fatal("two second output scan throttle was lost")
	}
	if err := m.SyncOutputs(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if _, found := m.Get("later"); !found {
		t.Fatal("forced monitor discovery did not bypass throttle")
	}
}

func TestSyncOutputsCredentialCancellationDoesNotBlockManager(t *testing.T) {
	st, _ := store.New(t.TempDir())
	m := NewManager(context.Background(), 1, st, nil)
	prior, _ := m.Submit(Spec{ID: "prior", Email: "same@example.test"})
	m.Cancel(prior.ID)
	discoveryFixture(t, st.Root, "incoming", prior.Email, "", true, false)
	vault := &cancelRestoreVault{blockLoadAt: 1, entered: make(chan struct{})}
	m.SetCredentialStore(vault)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- m.SyncOutputs(ctx, true) }()
	select {
	case <-vault.entered:
	case <-time.After(time.Second):
		t.Fatal("discovery did not reach private load")
	}
	listDone := make(chan struct{})
	go func() { _ = m.List(); close(listDone) }()
	select {
	case <-listDone:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("credential read blocked global manager lock")
	}
	if _, err := m.Update(prior.ID, map[string]any{"password": "Mutated-Password9!"}); err == nil {
		t.Fatal("private recovery did not reserve matching email")
	}
	if _, err := m.DeleteAccounts(context.Background(), []string{prior.ID}); err == nil {
		t.Fatal("delete overlapped credential import")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("discovery ignored cancellation")
	}
	if _, exists := m.Get("incoming"); exists {
		t.Fatal("canceled discovery published partial job")
	}
	m.SetCredentialStore(credentials.NewMemoryStore())
	if _, err := m.Update(prior.ID, map[string]any{"password": "Allowed-Password9!"}); err != nil {
		t.Fatal("cancel did not release account reservation", err)
	}
	if err := m.SyncOutputs(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if _, exists := m.Get("incoming"); !exists {
		t.Fatal("later discovery did not recover after cancellation")
	}
}

type rejectingGrantVault struct{ calls atomic.Int32 }

func (v *rejectingGrantVault) Load(string) (credentials.Credentials, error) {
	v.calls.Add(1)
	return credentials.Credentials{}, errors.New("locked vault")
}
func (v *rejectingGrantVault) Save(string, credentials.Credentials) error {
	v.calls.Add(1)
	return errors.New("locked vault")
}
func (v *rejectingGrantVault) Delete(string) error { v.calls.Add(1); return errors.New("locked vault") }
func TestReceivedGrantRestoreBypassesCredentialsAndKeepsOldDownload(t *testing.T) {
	for _, output := range []bool{false, true} {
		for _, live := range []bool{false, true} {
			name := map[bool]string{false: "no_output", true: "old_output"}[output] + map[bool]string{false: "_startup", true: "_live"}[live]
			t.Run(name, func(t *testing.T) {
				st, _ := store.New(t.TempDir())
				discoveryFixture(t, st.Root, "grant", "grant@example.test", "failed", output, false)
				dir, _ := store.New(filepath.Join(st.Root, "grant"))
				if err := dir.WriteJSON(context.Background(), "login-checkpoint.json", map[string]any{"version": 1, "email": "grant@example.test", "stage": "tokens_received", "oauth": map[string]any{"tokenResponse": map[string]any{"access_token": "synthetic-access", "refresh_token": "synthetic-refresh"}}}); err != nil {
					t.Fatal(err)
				}
				vault := &rejectingGrantVault{}
				m := NewManager(context.Background(), 1, st, nil)
				m.SetCredentialStore(vault)
				var err error
				if live {
					err = m.SyncOutputs(context.Background(), true)
				} else {
					err = m.Restore(context.Background())
				}
				if err != nil {
					t.Fatal(err)
				}
				got, _ := m.Get("grant")
				if got.Status != "resume_available" || got.Operation != "resume" || got.ResultSaved != output || got.Error != "" {
					t.Fatal("received grant lost finalization entry or original download", got)
				}
				if vault.calls.Load() != 0 {
					t.Fatal("offline finalization restoration consulted locked credentials")
				}
				retried, err := m.Retry("grant")
				if err != nil || retried.Operation != "resume" {
					t.Fatal("received grant retry requested fresh login", err)
				}
			})
		}
	}
}
