package jobs

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"tosub2-go-port/internal/store"
)

func TestRunnerPanicKeepsEvidenceReleasesOwnershipAndRejectsLateEvents(t *testing.T) {
	m := NewManager(t.Context(), 2, nil, nil)
	t.Cleanup(func() { _ = m.Shutdown(context.Background()) })
	var count atomic.Int32
	var late func(JobEvent)
	contexts := make(chan context.Context, 2)
	m.SetRunner(func(ctx context.Context, job *Job, emit func(JobEvent)) error {
		contexts <- ctx
		if count.Add(1) == 1 {
			late = emit
			m.UpdateResult(job.ID, true, "saved grant evidence")
			panic("synthetic secret password and token must never be logged")
		}
		return nil
	})
	first, err := m.Submit(Spec{ID: "panic-first", Email: "same@example.test"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := m.Submit(Spec{ID: "panic-second", Email: "same@example.test"})
	if err != nil {
		t.Fatal(err)
	}
	m.Start()
	ctx := receiveAttemptContext(t, contexts)
	failed := waitForStatus(t, m, first.ID, StatusFailed)
	waitForStatus(t, m, second.ID, StatusSucceeded)
	if count.Load() != 2 || !failed.ResultSaved || failed.OperationStatus != "failed" || !strings.Contains(failed.OperationError, "TASK_RUNNER_PANIC") {
		t.Fatal("panic lost retained evidence or prevented independent work")
	}
	if strings.Contains(strings.Join(failed.Logs, " "), "synthetic secret") {
		t.Fatal("panic payload reached logs")
	}
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("panic retained attempt context")
	}
	late(JobEvent{Stage: "password", Status: "awaiting_input", Prompt: "late stale event"})
	after, _ := m.Get(first.ID)
	if after.Status != failed.Status || !after.UpdatedAt.Equal(failed.UpdatedAt) {
		t.Fatal("post-panic event changed completed attempt")
	}
	if snapshot := m.ActivitySnapshot(); !snapshot.Drained {
		t.Fatalf("panic retained scheduler ownership: %+v", snapshot)
	}
}

func TestInvokeRunnerClassifiesNilPanicWithoutProviderPayload(t *testing.T) {
	for _, payload := range []any{nil, "synthetic-private-value", errors.New("synthetic-private-value")} {
		err := invokeRunner(func(context.Context, *Job, func(JobEvent)) error { panic(payload) }, t.Context(), &Job{}, func(JobEvent) {})
		if !errors.Is(err, ErrRunnerPanic) || strings.Contains(err.Error(), "synthetic-private-value") {
			t.Fatal("panic escaped or exposed its value")
		}
	}
}

func TestExecutionStartRequiresDurableState(t *testing.T) {
	for _, failure := range []error{os.ErrPermission, store.ErrDurabilityUncertain} {
		t.Run(failure.Error(), func(t *testing.T) {
			st, err := store.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			m := NewManager(t.Context(), 1, st, nil)
			t.Cleanup(func() { m.metadataWriter = nil; _ = m.Shutdown(context.Background()) })
			var calls atomic.Int32
			m.SetRunner(func(context.Context, *Job, func(JobEvent)) error { calls.Add(1); return nil })
			job, err := m.Submit(Spec{Email: "blocked@example.test"})
			if err != nil {
				t.Fatal(err)
			}
			events, unsubscribe := m.Subscribe(8)
			defer unsubscribe()
			m.metadataWriter = func(ctx context.Context, name string, value any) error {
				if errors.Is(failure, store.ErrDurabilityUncertain) {
					if err := st.WriteJSON(ctx, name, value); err != nil {
						return err
					}
				}
				return failure
			}
			m.Start()
			stopped := waitForStatus(t, m, job.ID, StatusFailed)
			if calls.Load() != 0 || !stopped.RecoveryPaused || stopped.OperationStatus != "interrupted" || stopped.Attempts != 0 {
				t.Fatal("uncommitted start reached runner or lost explicit recovery pause")
			}
			select {
			case e := <-events:
				t.Fatalf("published uncommitted start: %+v", e)
			default:
			}
			if !m.ActivitySnapshot().Drained {
				t.Fatal("blocked start retained an active lease")
			}
		})
	}
}

func TestRunnerCompletionDoesNotPublishBeforeCommit(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(t.Context(), 1, st, nil)
	t.Cleanup(func() { m.mu.Lock(); m.metadataWriter = nil; m.mu.Unlock(); _ = m.Shutdown(context.Background()) })
	m.SetRunner(func(context.Context, *Job, func(JobEvent)) error {
		m.mu.Lock()
		m.metadataWriter = func(context.Context, string, any) error { return os.ErrPermission }
		m.mu.Unlock()
		return nil
	})
	job, err := m.Submit(Spec{Email: "completion@example.test"})
	if err != nil {
		t.Fatal(err)
	}
	events, unsubscribe := m.Subscribe(8)
	defer unsubscribe()
	m.Start()
	finished := waitForStatus(t, m, job.ID, StatusSucceeded)
	if !strings.Contains(finished.LastError, "TASK_RESULT_PERSISTENCE") {
		t.Fatal("local result commit failure was hidden")
	}
	for {
		select {
		case event := <-events:
			if event.Status == StatusSucceeded {
				t.Fatal("announced completion before durable commit")
			}
		default:
			return
		}
	}
}

func TestManagerStartAndShutdownShareAdmissionBarrier(t *testing.T) {
	for i := 0; i < 100; i++ {
		m := NewManager(context.Background(), 1, nil, nil)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); m.Start() }()
		go func() {
			defer wg.Done()
			if err := m.Shutdown(context.Background()); err != nil {
				t.Error(err)
			}
		}()
		wg.Wait()
		m.Start()
		if _, err := m.Submit(Spec{Email: "after-close@example.test"}); err == nil {
			t.Fatal("shutdown manager admitted new work")
		}
	}
}
