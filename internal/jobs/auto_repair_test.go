package jobs

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"

	"tosub2-go-port/internal/store"
)

func TestAutoRepairIsAtomicAndDeduplicatesIDs(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	manager := NewManager(context.Background(), 1, st, slog.Default())
	job, err := manager.Submit(Spec{ID: "repair", Email: "a@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	manager.Cancel(job.ID)
	var successes atomic.Int32
	var wg sync.WaitGroup
	for n := 0; n < 10; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := manager.QueueAutoRepair(job.ID, []int{1, 1, 2}, "backend"); err == nil {
				successes.Add(1)
			}
		}()
	}
	wg.Wait()
	if successes.Load() != 1 {
		t.Fatal(successes.Load())
	}
	got, _ := manager.Get(job.ID)
	if got.Status != StatusQueued || got.Operation != "relogin" || len(got.AutoRepairPendingAccountIDs) != 2 || got.AutoRepairBackend != "backend" || got.AutoRepairLastAttemptAt == nil {
		t.Fatalf("%+v", got)
	}
}
