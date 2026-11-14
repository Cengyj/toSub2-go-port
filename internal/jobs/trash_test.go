package jobs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/store"
)

func trashManager(t *testing.T) (*Manager, *store.Store, credentials.Store) {
	t.Helper()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	vault := credentials.NewMemoryStore()
	m := NewManager(t.Context(), 1, st, nil)
	m.SetCredentialStore(vault)
	return m, st, vault
}

func TestTrashRetainsEmailHistoryArtifactsCredentialsAndRestartIsolation(t *testing.T) {
	m, st, vault := trashManager(t)
	email := "archive@example.test"
	for _, id := range []string{"older", "newer"} {
		if _, err := m.Submit(Spec{ID: id, Email: email, ImportOnly: true}); err != nil {
			t.Fatal(err)
		}
		discoveryFixture(t, st.Root, id, email, StatusQueued, true, true)
	}
	if err := vault.Save(email, credentials.Credentials{Password: "synthetic-secret"}); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	m.jobs["older"].Logs = []string{"retained operation history"}
	m.mu.Unlock()
	if count, err := m.TrashAccounts(t.Context(), []string{"newer"}); err != nil || count != 1 {
		t.Fatalf("trash = %d, %v", count, err)
	}
	if len(m.List()) != 0 || len(m.ListTrash()) != 1 {
		t.Fatal("email history was not isolated as one trash entry")
	}
	entry := m.ListTrash()[0]
	if len(entry.Jobs) != 2 || entry.State != TrashStateTrashed {
		t.Fatalf("incomplete trash entry: %+v", entry)
	}
	if !reflect.DeepEqual(m.RetainedAccountIDs(), []string{"newer", "older"}) {
		t.Fatal("trash IDs were not retained for local caches")
	}
	for _, id := range []string{"older", "newer"} {
		if _, err := os.Stat(filepath.Join(st.Root, id)); err != nil {
			t.Fatal("private artifacts removed", err)
		}
	}
	if got, err := vault.Load(email); err != nil || got.Password != "synthetic-secret" {
		t.Fatal("credential removed by soft delete")
	}
	if _, err := m.Submit(Spec{ID: "blocked", Email: " Archive@Example.Test ", ImportOnly: true}); !errors.Is(err, ErrAccountInTrash) {
		t.Fatalf("same-email import = %v", err)
	}
	if _, _, err := m.SubmitUniqueEmail(Spec{Email: email, ImportOnly: true}); !errors.Is(err, ErrAccountInTrash) {
		t.Fatalf("unique import = %v", err)
	}
	if _, err := m.Submit(Spec{ID: "older", Email: "other@example.test", ImportOnly: true}); !errors.Is(err, ErrAccountInTrash) {
		t.Fatalf("reused private ID = %v", err)
	}
	if err := m.SyncOutputs(t.Context(), true); err != nil || len(m.List()) != 0 {
		t.Fatalf("discovery resurrected trash: %v", err)
	}
	restarted := NewManager(t.Context(), 1, st, nil)
	restarted.SetCredentialStore(vault)
	if err := restarted.Restore(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(restarted.List()) != 0 || len(restarted.ListTrash()) != 1 {
		t.Fatal("startup rediscovered trashed artifacts")
	}
	if count, err := restarted.RestoreTrash(t.Context(), []string{entry.ID}); err != nil || count != 1 {
		t.Fatalf("restore = %d, %v", count, err)
	}
	if len(restarted.List()) != 2 || len(restarted.ListTrash()) != 0 || len(restarted.pending) != 0 {
		t.Fatal("restore requeued historical work")
	}
	got, _ := restarted.Get("older")
	if !reflect.DeepEqual(got.Logs, []string{"retained operation history"}) {
		t.Fatal("history lost across restore")
	}
}

func TestTrashCanceledRequestAndPauseFailurePreserveQueue(t *testing.T) {
	m, _, _ := trashManager(t)
	job, err := m.Submit(Spec{ID: "queued", Email: "queued@example.test"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := m.TrashAccounts(ctx, []string{job.ID}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation = %v", err)
	}
	m.SetTrashPauseHook(func(context.Context, []*Job) error { return errors.New("synthetic pause failure") })
	if _, err := m.TrashAccounts(t.Context(), []string{job.ID}); err == nil {
		t.Fatal("pause failure ignored")
	}
	current, exists := m.Get(job.ID)
	if !exists || current.Status != StatusQueued || m.QueuePosition(job.ID) != 1 || len(m.ListTrash()) != 0 {
		t.Fatal("unaccepted trash changed queued work")
	}
}
