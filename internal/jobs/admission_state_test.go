package jobs

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestMaintenanceStopsNewAdmissionAndDrainsAcceptedWork(t *testing.T) {
	m := NewManager(t.Context(), 1, nil, nil)
	t.Cleanup(func() { _ = m.Shutdown(context.Background()) })
	prompts := make(chan context.Context, 2)
	release := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	var calls atomic.Int32
	m.SetRunner(func(ctx context.Context, _ *Job, emit func(JobEvent)) error {
		if calls.Add(1) == 1 {
			emit(JobEvent{Stage: "email_otp", Status: "awaiting_input"})
			prompts <- ctx
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	})
	first, err := m.Submit(Spec{Email: "drain-first@example.test"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := m.Submit(Spec{Email: "drain-second@example.test"})
	if err != nil {
		t.Fatal(err)
	}
	idle, err := m.Submit(Spec{Email: "drain-idle@example.test", ImportOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	m.Start()
	receiveAttemptContext(t, prompts)
	m.SetAdmissionPaused(true)
	state := m.ActivitySnapshot()
	if !state.AdmissionPaused || state.Running != 1 || state.Queued != 1 || state.Drained {
		t.Fatalf("incorrect drain snapshot: %+v", state)
	}
	checks := []func() error{
		func() error { _, err := m.Submit(Spec{Email: "rejected@example.test"}); return err },
		func() error { _, _, err := m.SubmitUniqueEmail(Spec{Email: idle.Email, ImportOnly: true}); return err },
		func() error { _, err := m.Retry(idle.ID); return err },
		func() error { _, err := m.Requeue(idle.ID, "relogin"); return err },
		func() error { _, err := m.QueueOperation(idle.ID, "relogin", nil); return err },
		func() error { _, err := m.QueueAutoRepair(idle.ID, []int{1}, "synthetic"); return err },
		func() error { _, err := m.SwitchOperation(t.Context(), first.ID, "setup-2fa", nil); return err },
	}
	for i, check := range checks {
		if err := check(); !errors.Is(err, ErrAdmissionPaused) {
			t.Fatalf("entry %d bypassed maintenance: %v", i, err)
		}
	}
	if m.Input(idle.ID, map[string]any{"email_otp": "123456"}) {
		t.Fatal("legacy input started a new operation during maintenance")
	}
	current, _ := m.Get(first.ID)
	if !m.InputForStageExpected(first.ID, InputExpectation{OperationAt: current.LastOperationAt, Stage: "email_otp"}, map[string]any{"email_otp": "123456"}) {
		t.Fatal("maintenance prevented completing accepted work")
	}
	close(release)
	waitForStatus(t, m, first.ID, StatusSucceeded)
	waitForStatus(t, m, second.ID, StatusSucceeded)
	if state = m.ActivitySnapshot(); !state.Drained || calls.Load() != 2 {
		t.Fatalf("accepted queue failed to drain: %+v calls=%d", state, calls.Load())
	}
	lease, err := m.ReserveAccountQuery(t.Context(), idle.ID)
	if err != nil {
		t.Fatal("maintenance interrupted an application-admitted query", err)
	}
	if m.ActivitySnapshot().Drained {
		t.Fatal("query ownership was omitted from maintenance drain")
	}
	lease.Release()
	if !m.ActivitySnapshot().Drained {
		t.Fatal("released query retained maintenance ownership")
	}
	m.SetAdmissionPaused(false)
	if _, err := m.Submit(Spec{Email: "after-maintenance@example.test", ImportOnly: true}); err != nil {
		t.Fatal(err)
	}
}

func TestExistingPasswordOutcomeDoesNotInventAdditionTime(t *testing.T) {
	m := NewManager(t.Context(), 1, nil, nil)
	defer m.cancel()
	previous := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
	job, err := m.Submit(Spec{Email: "existing@example.test", ImportOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	m.jobs[job.ID].PasswordAddedAt = &previous
	m.jobs[job.ID].Operation = "add-password"
	m.mu.Unlock()
	if err := m.MarkPasswordExisting(job.ID); err != nil {
		t.Fatal(err)
	}
	current, _ := m.Get(job.ID)
	if !current.HasPassword || !current.PasswordAddedAt.Equal(previous) || current.PasswordAddOutcome != PasswordOutcomeExisting || operationCompletionPrompt(current) != "账号已有密码，已跳过添加" {
		t.Fatal("skip was reported as a new password mutation")
	}
}
