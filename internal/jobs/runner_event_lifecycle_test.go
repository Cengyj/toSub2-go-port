package jobs

import (
	"context"
	"testing"
	"time"
)

func TestRunnerEventsCannotOutliveTheirAttempt(t *testing.T) {
	m := NewManager(t.Context(), 1, nil, nil)
	t.Cleanup(func() { _ = m.Shutdown(context.Background()) })
	type attempt struct {
		ctx  context.Context
		emit func(JobEvent)
		end  chan struct{}
	}
	attempts := make(chan attempt, 2)
	m.SetRunner(func(ctx context.Context, _ *Job, emit func(JobEvent)) error {
		a := attempt{ctx: ctx, emit: emit, end: make(chan struct{})}
		emit(JobEvent{Stage: "oauth", Status: StatusRunning})
		attempts <- a
		select {
		case <-a.end:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	j, err := m.Submit(Spec{Email: "late-runner@example.test"})
	if err != nil {
		t.Fatal(err)
	}
	m.Start()
	first := <-attempts
	close(first.end)
	waitForStatus(t, m, j.ID, StatusSucceeded)
	late := JobEvent{Stage: "email_otp", Status: StatusWaitingInput, Prompt: "stale prompt", Data: map[string]any{"mailStatus": "polling"}}
	first.emit(late)
	if got, _ := m.Get(j.ID); got.Status != StatusSucceeded || got.Stage != "complete" || got.MailStatus == "polling" {
		t.Fatalf("completed attempt accepted a late event: status=%s stage=%s", got.Status, got.Stage)
	}
	if _, err := m.QueueOperation(j.ID, "relogin", nil); err != nil {
		t.Fatal(err)
	}
	second := <-attempts
	first.emit(late)
	if got, _ := m.Get(j.ID); got.Status != "working" || got.Stage != "oauth" || got.Prompt == "stale prompt" {
		t.Fatalf("old callback changed its successor: status=%s stage=%s", got.Status, got.Stage)
	}
	m.Cancel(j.ID)
	waitForStatus(t, m, j.ID, StatusCanceled)
	second.emit(late)
	if got, _ := m.Get(j.ID); got.Status != StatusCanceled || got.OperationStatus != "canceled" {
		t.Fatal("canceled attempt reopened its input")
	}
}

func TestCanceledRunnerCannotPublishWhileStillCleaningUp(t *testing.T) {
	m := NewManager(t.Context(), 1, nil, nil)
	t.Cleanup(func() { _ = m.Shutdown(context.Background()) })
	ready, published := make(chan struct{}), make(chan struct{})
	finish := make(chan struct{})
	defer close(finish)
	m.SetRunner(func(ctx context.Context, _ *Job, emit func(JobEvent)) error {
		emit(JobEvent{Stage: "oauth", Status: StatusRunning})
		close(ready)
		<-ctx.Done()
		emit(JobEvent{Stage: "email_otp", Status: StatusWaitingInput, Data: map[string]any{"mailStatus": "polling"}})
		close(published)
		<-finish
		return ctx.Err()
	})
	j, err := m.Submit(Spec{Email: "canceled-runner@example.test"})
	if err != nil {
		t.Fatal(err)
	}
	m.Start()
	select {
	case <-ready:
	case <-time.After(time.Second):
		t.Fatal("runner did not start")
	}
	m.Cancel(j.ID)
	<-published
	if got, _ := m.Get(j.ID); got.Stage != "oauth" || got.MailStatus == "polling" {
		t.Fatal("canceled runner reopened its prompt before cleanup completed")
	}
}
