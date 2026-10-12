package jobs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/store"
)

func TestImportedAccountWaitsAcrossRestartAndValidatesCredentialsBeforeStarting(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	vault := credentials.NewMemoryStore()
	m := NewManager(context.Background(), 1, st, nil)
	m.SetCredentialStore(vault)
	runs := make(chan *Job, 3)
	m.SetRunner(func(_ context.Context, job *Job, _ func(JobEvent)) error { runs <- job; return nil })
	m.Start()
	j, created, err := m.SubmitUniqueEmail(Spec{Email: "imported@example.test", ImportOnly: true, Input: map[string]any{"password": "Fixture-password9!", "totpSecret": "JBSWY3DPEHPK3PXP", "proxyUrl": "http://proxy.example:8080"}})
	if err != nil || !created || j.Status != StatusImported || j.Attempts != 0 || !j.QueuedAt.IsZero() || j.LastOperationType != "account_import" {
		t.Fatal("new import was scheduled", err)
	}
	// A later ordinary submission exercises the live scheduler and still starts
	// immediately, proving imported records neither run nor occupy queue slots.
	single, err := m.Submit(Spec{Email: "single@example.test"})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case run := <-runs:
		if run.ID != single.ID {
			t.Fatal("import ran before explicit selection")
		}
	case <-time.After(time.Second):
		t.Fatal("single creation no longer starts")
	}
	waitForStatus(t, m, single.ID, StatusSucceeded)
	if err := m.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(st.Root, "job-meta.json"))
	if err != nil || strings.Contains(string(data), "Fixture-password9!") || strings.Contains(string(data), "JBSWY3DPEHPK3PXP") {
		t.Fatal("import metadata exposed credentials", err)
	}
	m = NewManager(context.Background(), 1, st, nil)
	m.SetCredentialStore(credentials.NewMemoryStore())
	if err := m.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, _ := m.Get(j.ID)
	if got.Status != StatusImported || got.Attempts != 0 || !got.QueuedAt.IsZero() || len(m.pending) != 0 {
		t.Fatal("restart scheduled an imported account")
	}
	before, _ := os.ReadFile(filepath.Join(st.Root, "job-meta.json"))
	if _, err := m.RetryWithValues(j.ID, map[string]any{"proxyUrl": ""}); err == nil {
		t.Fatal("missing recorded secrets silently changed login identity")
	}
	after, _ := os.ReadFile(filepath.Join(st.Root, "job-meta.json"))
	if string(before) != string(after) {
		t.Fatal("rejected imported action changed metadata")
	}
	m.SetCredentialStore(vault)
	got, err = m.RetryWithValues(j.ID, map[string]any{"proxyUrl": ""})
	if err != nil || got.Status != StatusQueued || got.LastOperationType != "initial_authorization" || got.Input["proxyUrl"] != "http://proxy.example:8080" || got.Input["password"] != "Fixture-password9!" {
		t.Fatal("explicit start did not recover the complete original identity", err)
	}
	m.SetRunner(func(_ context.Context, job *Job, _ func(JobEvent)) error { runs <- job; return nil })
	m.Start()
	defer m.Shutdown(context.Background())
	select {
	case run := <-runs:
		if run.ID != j.ID {
			t.Fatal("wrong imported record started")
		}
	case <-time.After(time.Second):
		t.Fatal("explicit start did not run")
	}
	if got := waitForStatus(t, m, j.ID, StatusSucceeded); got.Attempts != 1 {
		t.Fatal("import counted as a login attempt")
	}
}

func TestImportDoesNotRestartOrModifyQueuedAndRunningAccounts(t *testing.T) {
	m := NewManager(context.Background(), 1, nil, nil)
	started := make(chan struct{})
	m.SetRunner(func(ctx context.Context, _ *Job, _ func(JobEvent)) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	})
	defer m.Shutdown(context.Background())
	j, _, err := m.SubmitUniqueEmail(Spec{Email: "running@example.test", Input: map[string]any{"password": "Original9!"}})
	if err != nil {
		t.Fatal(err)
	}
	checkImport := func() {
		t.Helper()
		before, _ := m.Get(j.ID)
		for _, input := range []map[string]any{nil, {"password": "Original9!"}, {"proxyUrl": ""}} {
			repeated, created, err := m.SubmitUniqueEmail(Spec{Email: "RUNNING@example.test", ImportOnly: true, Input: input})
			if err != nil || created || repeated.ID != j.ID || !repeated.UpdatedAt.Equal(before.UpdatedAt) || repeated.Attempts != before.Attempts {
				t.Fatal("identical import changed active task", err)
			}
		}
		if _, _, err := m.SubmitUniqueEmail(Spec{Email: j.Email, ImportOnly: true, Input: map[string]any{"password": "Changed9!"}}); err == nil {
			t.Fatal("active credentials changed through batch import")
		}
		after, _ := m.Get(j.ID)
		if after.Input["password"] != "Original9!" || after.Status != before.Status || m.restart[j.ID] {
			t.Fatal("batch import restarted the existing protocol")
		}
	}
	checkImport()
	m.Start()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("single task did not start")
	}
	checkImport()
}

func TestImportedSecuritySelectionReturnsToImportedWithoutAuthorization(t *testing.T) {
	for _, operation := range []string{"setup-2fa", "add-password"} {
		for _, outcome := range []string{"success", "failure", "cancel"} {
			t.Run(operation+"/"+outcome, func(t *testing.T) {
				m := NewManager(context.Background(), 1, nil, nil)
				started := make(chan struct{})
				m.SetRunner(func(ctx context.Context, job *Job, _ func(JobEvent)) error {
					if !isSuboperation(job.Operation) {
						return nil
					}
					close(started)
					if outcome == "cancel" {
						<-ctx.Done()
						return ctx.Err()
					}
					if outcome == "failure" {
						return errors.New("fixture security failure")
					}
					return nil
				})
				m.Start()
				defer m.Shutdown(context.Background())
				j, _ := m.Submit(Spec{Email: "security@example.test", ImportOnly: true})
				queued, err := m.SwitchOperation(context.Background(), j.ID, operation, nil)
				if err != nil || queued.SecurityActionReturnStatus != StatusImported {
					t.Fatal(err)
				}
				select {
				case <-started:
				case <-time.After(time.Second):
					t.Fatal("selected security operation did not start")
				}
				if outcome == "cancel" {
					m.Cancel(j.ID)
				}
				got := waitForStatus(t, m, j.ID, StatusImported)
				if got.Attempts != 0 || got.ResultSaved || !got.QueuedAt.IsZero() || len(m.pending) != 0 {
					t.Fatal("security operation started authorization")
				}
				if outcome == "failure" && got.Error == "" {
					t.Fatal("security operation failure disappeared")
				}
				queued, err = m.RetryWithValues(j.ID, nil)
				if err != nil || queued.LastOperationType != "initial_authorization" || queued.SecurityActionReturnStatus != "" {
					t.Fatal("first authorization lost initial status", err)
				}
				m.Cancel(j.ID)
			})
		}
	}
}

func TestInterruptedImportedSecuritySelectionDoesNotBecomeQueuedAuthorization(t *testing.T) {
	for _, operation := range []string{"setup-2fa", "add-password"} {
		st, _ := store.New(t.TempDir())
		m := NewManager(context.Background(), 1, st, nil)
		j, _ := m.Submit(Spec{Email: "interrupted@example.test", ImportOnly: true})
		if _, err := m.QueueOperation(j.ID, operation, nil); err != nil {
			t.Fatal(err)
		}
		m.Shutdown(context.Background())
		restored := NewManager(context.Background(), 1, st, nil)
		if err := restored.Restore(context.Background()); err != nil {
			t.Fatal(err)
		}
		got, _ := restored.Get(j.ID)
		if got.Status != StatusFailed || got.Attempts != 0 || len(restored.pending) != 0 {
			t.Fatal("interrupted security operation automatically restarted")
		}
	}
}
