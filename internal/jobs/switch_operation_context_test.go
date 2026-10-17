package jobs

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestCanceledSwitchOperationDoesNotChangeAccount(t *testing.T) {
	for _, active := range []bool{false, true} {
		name := "idle"
		if active {
			name = "active-phone-wait"
		}
		t.Run(name, func(t *testing.T) {
			m := NewManager(t.Context(), 1, nil, nil)
			defer m.cancel()
			j := &Job{ID: "switch-fixture", Email: "switch@example.test", Status: "phone", Operation: "full", Input: map[string]any{"password": "synthetic-password"}}
			if !active {
				j.Status = "resume_available"
			}
			m.jobs[j.ID] = j
			before := cloneJob(j)
			canceledRunner := false
			if active {
				m.active[j.ID] = func() { canceledRunner = true }
			}
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			queued, err := m.SwitchOperation(ctx, j.ID, "setup-2fa", map[string]any{"authorizationMethod": "bps"})
			if !errors.Is(err, context.Canceled) || queued != nil {
				t.Errorf("canceled request admitted an operation: queued=%t error=%v", queued != nil, err)
			}
			if canceledRunner || len(m.pending) != 0 || !reflect.DeepEqual(cloneJob(j), before) {
				t.Fatal("canceled request interrupted the current run or changed its account state")
			}
		})
	}
}

func TestSwitchOperationCanceledDuringRunnerReleaseDoesNotQueue(t *testing.T) {
	m := NewManager(t.Context(), 1, nil, nil)
	defer m.cancel()
	job := &Job{ID: "release-fixture", Email: "release@example.test", Status: "phone", Operation: "full", AuthorizationMethod: AuthorizationMethodChatGPT, Input: map[string]any{"password": "synthetic-password"}}
	m.jobs[job.ID] = job
	before := cloneJob(job)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	m.active[job.ID] = func() {
		// Model the request closing while the canceled runner releases its
		// resources. There is no remaining active run for the wait loop.
		cancel()
		m.mu.Lock()
		delete(m.active, job.ID)
		m.mu.Unlock()
	}
	queued, err := m.SwitchOperation(ctx, job.ID, "setup-2fa", map[string]any{"authorizationMethod": AuthorizationMethodBPS})
	if !errors.Is(err, context.Canceled) || queued != nil {
		t.Fatalf("cancellation during release admitted a new operation: %v", err)
	}
	if len(m.pending) != 0 || len(m.wake) != 0 || !reflect.DeepEqual(cloneJob(job), before) {
		t.Fatal("canceled switch changed identity or scheduled a follow-up operation")
	}
}
