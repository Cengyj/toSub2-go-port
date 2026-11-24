package jobs

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"tosub2-go-port/internal/credentials"
)

func TestExpectedInputCannotReachAnotherOperationOrCanceledAttempt(t *testing.T) {
	m := NewManager(t.Context(), 1, nil, nil)
	t.Cleanup(func() { _ = m.Shutdown(context.Background()) })
	prompts := make(chan context.Context, 1)
	release := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	m.SetRunner(func(ctx context.Context, _ *Job, emit func(JobEvent)) error {
		emit(JobEvent{Stage: "email_otp", Status: "awaiting_input"})
		prompts <- ctx
		<-release
		return nil
	})
	job, err := m.Submit(Spec{Email: "input@example.test"})
	if err != nil {
		t.Fatal(err)
	}
	m.Start()
	receiveAttemptContext(t, prompts)
	current, _ := m.Get(job.ID)
	expected := InputExpectation{OperationAt: current.LastOperationAt, Stage: "email_otp"}
	stale := expected
	stale.OperationAt = stale.OperationAt.Add(-time.Nanosecond)
	if m.InputForStageExpected(job.ID, stale, map[string]any{"email_otp": "123456"}) {
		t.Fatal("input from another operation was accepted")
	}
	wrong := expected
	wrong.Stage = "password"
	if m.InputForStageExpected(job.ID, wrong, map[string]any{"password": "candidate"}) {
		t.Fatal("input from another stage was accepted")
	}
	before, _ := m.InputCopy(job.ID)
	if ok, err := m.CancelWithError(job.ID); !ok || err != nil {
		t.Fatal("cancel not accepted", err)
	}
	if m.InputForStageExpected(job.ID, expected, map[string]any{"email_otp": "123456"}) || m.InputForStage(job.ID, "email_otp", map[string]any{"email_otp": "654321"}) || m.Input(job.ID, map[string]any{"email_otp": "987654"}) {
		t.Fatal("canceled runner accepted delayed input before cleanup")
	}
	after, _ := m.InputCopy(job.ID)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("rejected input changed process input")
	}
	close(release)
	waitForStatus(t, m, job.ID, StatusCanceled)
}

func TestInteractivePasswordRemainsCandidateUntilVerified(t *testing.T) {
	m := NewManager(t.Context(), 1, nil, nil)
	t.Cleanup(func() { _ = m.Shutdown(context.Background()) })
	vault := credentials.NewMemoryStore()
	m.SetCredentialStore(vault)
	const email = "password-candidate@example.test"
	if err := vault.Save(email, credentials.Credentials{Password: "verified-old-password"}); err != nil {
		t.Fatal(err)
	}
	prompt := make(chan context.Context, 1)
	release := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	m.SetRunner(func(ctx context.Context, _ *Job, emit func(JobEvent)) error {
		emit(JobEvent{Stage: "password", Status: "awaiting_input"})
		prompt <- ctx
		<-release
		return errors.New("candidate rejected")
	})
	job, err := m.Submit(Spec{Email: email})
	if err != nil {
		t.Fatal(err)
	}
	m.Start()
	receiveAttemptContext(t, prompt)
	current, _ := m.Get(job.ID)
	expected := InputExpectation{OperationAt: current.LastOperationAt, Stage: "password"}
	const candidate = "  candidate with exact spaces  "
	if !m.InputForStageExpected(job.ID, expected, map[string]any{"password": candidate}) {
		t.Fatal("candidate was not accepted for verification")
	}
	if m.InputForStageExpected(job.ID, expected, map[string]any{"password": "duplicate"}) {
		t.Fatal("duplicate prompt response was accepted")
	}
	input, _ := m.InputCopy(job.ID)
	if input["password"] != candidate {
		t.Fatal("password whitespace changed")
	}
	saved, err := vault.Load(email)
	if err != nil || saved.Password != "verified-old-password" {
		t.Fatal("unverified password replaced protected credential", err)
	}
	close(release)
	waitForStatus(t, m, job.ID, StatusFailed)
	saved, _ = vault.Load(email)
	if saved.Password != "verified-old-password" {
		t.Fatal("rejected password was saved")
	}
}

func TestExpectedInputPreparationCannotCrossOperationWithSameAttemptCount(t *testing.T) {
	m := NewManager(t.Context(), 1, nil, nil)
	defer m.cancel()
	at := time.Now().UTC()
	job := &Job{ID: "input-preparation", Email: "prepare@example.test", Status: "phone", Stage: "phone", LastOperationAt: at, Input: map[string]any{}}
	m.jobs[job.ID] = job
	called := 0
	accepted := m.InputForStageWithPreparationExpected(job.ID, InputExpectation{OperationAt: at, Stage: "phone"}, map[string]any{"phone": "+15555550123"}, func() {
		called++
		m.mu.Lock()
		job.LastOperationAt = at.Add(time.Second)
		m.mu.Unlock()
	})
	if accepted || called != 1 || job.Input["phone"] != nil || len(m.pending) != 0 {
		t.Fatal("prepared response crossed operation identity")
	}
}
