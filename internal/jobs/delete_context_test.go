package jobs

import (
	"context"
	"errors"
	"testing"

	"tosub2-go-port/internal/store"
)

func TestDeleteAccountsCanceledRequestPreservesQueuedWork(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(t.Context(), 1, st, nil)
	job, err := m.Submit(Spec{ID: "cancel-delete", Email: "cancel-delete@example.test"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	deleted, err := m.DeleteAccounts(ctx, []string{job.ID})
	if deleted != 0 || !errors.Is(err, context.Canceled) {
		t.Fatalf("delete result = %d, %v; want no deletion and cancellation", deleted, err)
	}
	current, ok := m.Get(job.ID)
	if !ok || current.Status != StatusQueued || m.QueuePosition(job.ID) != 1 {
		t.Fatalf("already canceled request changed queued account: exists=%v status=%v position=%d", ok, current.Status, m.QueuePosition(job.ID))
	}
	restored := NewManager(t.Context(), 1, st, nil)
	if err := restored.Restore(t.Context()); err != nil {
		t.Fatal(err)
	}
	current, ok = restored.Get(job.ID)
	if !ok || current.Status != StatusQueued || restored.QueuePosition(job.ID) != 1 {
		t.Fatalf("already canceled request changed durable queue: exists=%v status=%v", ok, current.Status)
	}
}

func TestDeleteAccountsCanceledRequestDoesNotCancelRunner(t *testing.T) {
	m := NewManager(t.Context(), 1, nil, nil)
	t.Cleanup(func() { _ = m.Shutdown(context.Background()) })
	attempts := make(chan context.Context, 1)
	m.SetRunner(func(ctx context.Context, _ *Job, _ func(JobEvent)) error {
		attempts <- ctx
		<-ctx.Done()
		return ctx.Err()
	})
	job, err := m.Submit(Spec{Email: "running-delete@example.test"})
	if err != nil {
		t.Fatal(err)
	}
	m.Start()
	running := receiveAttemptContext(t, attempts)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := m.DeleteAccounts(ctx, []string{job.ID}); !errors.Is(err, context.Canceled) {
		t.Fatalf("delete error = %v, want canceled", err)
	}
	if running.Err() != nil {
		t.Fatal("already canceled deletion request canceled an active account operation")
	}
}

func TestDeleteAccountsRejectsNewDeletionAfterShutdown(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(t.Context(), 1, st, nil)
	job, err := m.Submit(Spec{ID: "shutdown-delete", Email: "shutdown-delete@example.test"})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if removed, err := m.DeleteAccounts(t.Context(), []string{job.ID}); removed != 0 || err == nil {
		t.Fatalf("shutdown admitted deletion: removed=%d err=%v", removed, err)
	}
	if current, ok := m.Get(job.ID); !ok || current.Status != StatusQueued || m.QueuePosition(job.ID) != 1 {
		t.Fatal("shutdown deletion changed retained queued work")
	}
}
