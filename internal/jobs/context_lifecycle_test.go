package jobs

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestCompletedAttemptReleasesContextWithoutChangingOutcome(t *testing.T) {
	for _, test := range []struct {
		name    string
		err     error
		status  string
		outcome string
	}{
		{name: "success", status: StatusSucceeded, outcome: "succeeded"},
		{name: "failure", err: errors.New("synthetic operation failed"), status: StatusFailed, outcome: "failed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			parent, cancelParent := context.WithCancel(t.Context())
			defer cancelParent()
			m := NewManager(parent, 1, nil, nil)
			t.Cleanup(func() { _ = m.Shutdown(context.Background()) })
			attempts := make(chan context.Context, 2)
			release := make(chan struct{})
			m.SetRunner(func(ctx context.Context, _ *Job, _ func(JobEvent)) error {
				attempts <- ctx
				select {
				case <-release:
					return test.err
				case <-ctx.Done():
					return ctx.Err()
				}
			})
			first, err := m.Submit(Spec{Email: "lifecycle@example.test"})
			if err != nil {
				t.Fatal(err)
			}
			m.Start()
			firstContext := receiveAttemptContext(t, attempts)
			close(release)
			finished := waitForStatus(t, m, first.ID, test.status)
			if finished.OperationStatus != test.outcome {
				t.Fatalf("operation outcome = %q, want %q", finished.OperationStatus, test.outcome)
			}
			select {
			case <-firstContext.Done():
			case <-time.After(time.Second):
				t.Fatal("completed attempt retained its context until service shutdown")
			}
			if parent.Err() != nil || m.rootCtx.Err() != nil {
				t.Fatal("attempt cleanup canceled its parent service context")
			}

			// A subsequent attempt for the same account owns a fresh context.
			// Releasing an old attempt must not cancel or alter its successor.
			second, err := m.QueueOperation(first.ID, "relogin", nil)
			if err != nil {
				t.Fatal(err)
			}
			secondContext := receiveAttemptContext(t, attempts)
			if firstContext == secondContext {
				t.Fatal("follow-up operation reused a completed attempt context")
			}
			finished = waitForStatus(t, m, second.ID, test.status)
			if finished.OperationStatus != test.outcome || finished.Attempts != 2 {
				t.Fatalf("follow-up outcome = %q, attempts = %d", finished.OperationStatus, finished.Attempts)
			}
			select {
			case <-secondContext.Done():
			case <-time.After(time.Second):
				t.Fatal("follow-up attempt retained its context")
			}
		})
	}
}

func receiveAttemptContext(t *testing.T, attempts <-chan context.Context) context.Context {
	t.Helper()
	select {
	case ctx := <-attempts:
		return ctx
	case <-time.After(time.Second):
		t.Fatal("runner did not start")
		return nil
	}
}
