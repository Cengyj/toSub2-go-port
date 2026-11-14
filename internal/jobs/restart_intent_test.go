package jobs

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/store"
)

func TestRestartRetainsAccountsWithoutExecutingOldIntents(t *testing.T) {
	for _, status := range []string{StatusQueued, "starting", "interrupted"} {
		for _, operation := range []string{"full", "relogin", "refresh", "add-password", "setup-2fa"} {
			t.Run(status+"/"+operation, func(t *testing.T) {
				st, err := store.New(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				const id, email = "old-intent", "restart@example.test"
				job := &Job{ID: id, Email: email, Status: status, Operation: operation, Stage: "start", OperationStatus: "queued", HasPassword: true, CreatedAt: time.Now().UTC()}
				if err := st.WriteJSON(t.Context(), "job-meta.json", map[string]any{"version": 1, "jobs": []any{jobMetadata(job)}}); err != nil {
					t.Fatal(err)
				}
				vault := credentials.NewMemoryStore()
				if err := vault.Save(email, credentials.Credentials{Password: "Fixture-Password9!"}); err != nil {
					t.Fatal(err)
				}
				manager := NewManager(t.Context(), 1, st, nil)
				t.Cleanup(func() { _ = manager.Shutdown(context.Background()) })
				manager.SetCredentialStore(vault)
				var calls atomic.Int32
				manager.SetRunner(func(context.Context, *Job, func(JobEvent)) error { calls.Add(1); return nil })
				if err := manager.Restore(t.Context()); err != nil {
					t.Fatal(err)
				}
				manager.Start()
				// A newly submitted unrelated operation gives the scheduler an
				// observable barrier after all restored records were considered.
				barrier, err := manager.Submit(Spec{Email: "barrier@example.test"})
				if err != nil {
					t.Fatal(err)
				}
				waitForStatus(t, manager, barrier.ID, StatusSucceeded)
				got, ok := manager.Get(id)
				if !ok || got.Email != email || got.Status == StatusQueued || calls.Load() != 1 {
					t.Fatalf("restart executed an old operation: calls=%d account=%+v", calls.Load(), got != nil)
				}
				if _, err := manager.QueueOperation(id, "relogin", nil); err != nil {
					t.Fatal("explicit new login was blocked", err)
				}
				waitForStatus(t, manager, id, StatusSucceeded)
				if calls.Load() != 2 {
					t.Fatal("explicit new intent did not execute exactly once")
				}
			})
		}
	}
}
