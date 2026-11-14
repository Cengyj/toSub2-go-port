package jobs

import (
	"context"
	"testing"

	"tosub2-go-port/internal/store"
)

func TestRemovedRemoteRecoveryCannotEnterQueue(t *testing.T) {
	for _, operation := range []string{"recover-2fa", "inspect-2fa", "recover_2fa", "inspect_2fa", "reauthenticate", " REAUTHENTICATE "} {
		t.Run(operation, func(t *testing.T) {
			m := NewManager(t.Context(), 1, nil, nil)
			defer m.Shutdown(context.Background())
			if _, err := m.Submit(Spec{ID: "removed-new", Email: "owner@example.test", Operation: operation}); err == nil {
				t.Fatal("retired remote operation was admitted")
			}
			job, err := m.Submit(Spec{ID: "existing", Email: "owner@example.test", ImportOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := m.QueueOperation(job.ID, operation, nil); err == nil {
				t.Fatal("retired remote operation was queued")
			}
			if _, err := m.Requeue(job.ID, operation); err == nil {
				t.Fatal("retired remote operation was requeued")
			}
			m.mu.Lock()
			m.jobs[job.ID].Operation, m.jobs[job.ID].Status = operation, StatusCanceled
			m.mu.Unlock()
			if m.Input(job.ID, map[string]any{"email_otp": "new-code"}) {
				t.Fatal("compatibility input restarted a retired remote operation")
			}
			if len(m.pending) != 0 || len(m.jobs) != 1 {
				t.Fatal("rejected operation changed queue or created a task")
			}
		})
	}
}

func TestRemovedReauthenticationDoesNotRestartWithManager(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.WriteJSON(t.Context(), "job-meta.json", map[string]any{"jobs": []any{map[string]any{
		"id": "removed-reauth", "email": "owner@example.test", "operation": "reauthenticate", "status": StatusQueued,
	}}}); err != nil {
		t.Fatal(err)
	}
	m := NewManager(t.Context(), 1, st, nil)
	defer m.Shutdown(context.Background())
	if err := m.Restore(t.Context()); err != nil {
		t.Fatal(err)
	}
	job, ok := m.Get("removed-reauth")
	if !ok || job.Status != StatusFailed || job.Stage != "manual_review_required" || len(m.pending) != 0 {
		t.Fatal("startup admitted retired reauthentication")
	}
}
