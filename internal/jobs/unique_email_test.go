package jobs

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"

	"tosub2-go-port/internal/store"
)

func TestSubmitUniqueEmailConcurrentBatchAndSingle(t *testing.T) {
	st, _ := store.New(t.TempDir())
	manager := NewManager(context.Background(), 20, st, slog.Default())
	var wg sync.WaitGroup
	var created atomic.Int32
	for n := 0; n < 50; n++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			email := "a@example.com"
			if n%2 == 0 {
				email = " A@EXAMPLE.COM "
			}
			_, isNew, err := manager.SubmitUniqueEmail(Spec{Email: email, Input: map[string]any{"password": "test-secret"}})
			if err != nil {
				t.Error(err)
			}
			if isNew {
				created.Add(1)
			}
		}(n)
	}
	wg.Wait()
	if created.Load() != 1 || len(manager.List()) != 1 || len(manager.pending) != 1 {
		t.Fatalf("created=%d jobs=%d pending=%d", created.Load(), len(manager.List()), len(manager.pending))
	}
}
