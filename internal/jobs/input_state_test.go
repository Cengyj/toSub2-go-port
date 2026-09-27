package jobs

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestInputForStageAcceptsOnlyOneConcurrentSubmission(t *testing.T) {
	m := NewManager(context.Background(), 1, nil, nil)
	ready := make(chan struct{})
	m.SetRunner(func(ctx context.Context, _ *Job, emit func(JobEvent)) error {
		emit(JobEvent{Stage: "email_otp", Status: StatusWaitingInput})
		close(ready)
		<-ctx.Done()
		return ctx.Err()
	})
	m.Start()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := m.Shutdown(ctx); err != nil {
			t.Error(err)
		}
	})
	j, err := m.Submit(Spec{Email: "atomic@example.test"})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-ready:
	case <-time.After(time.Second):
		t.Fatal("runner did not wait for input")
	}
	start := make(chan struct{})
	accepted := make(chan string, 32)
	var workers sync.WaitGroup
	for i := 0; i < cap(accepted); i++ {
		workers.Add(1)
		go func(code string) {
			defer workers.Done()
			<-start
			if m.InputForStage(j.ID, "email_otp", map[string]any{"email_otp": code}) {
				accepted <- code
			}
		}(fmt.Sprintf("%06d", 100000+i))
	}
	close(start)
	workers.Wait()
	close(accepted)
	var values []string
	for value := range accepted {
		values = append(values, value)
	}
	if len(values) != 1 {
		t.Fatalf("accepted %d concurrent responses, want one", len(values))
	}
	current, _ := m.Get(j.ID)
	if current.Status != "working" || current.Prompt != "正在验证邮箱验证码" {
		t.Fatal("accepted response did not close prompt")
	}
	value, ok := m.ConsumeInput(j.ID, "email_otp")
	if !ok || value != values[0] {
		t.Fatal("rejected concurrent response overwrote accepted input")
	}
	m.Emit(JobEvent{JobID: j.ID, Stage: "email_otp", Status: StatusRunning})
	if m.InputForStage(j.ID, "email_otp", map[string]any{"email_otp": "555555"}) {
		t.Fatal("validation phase reopened input")
	}
	m.Emit(JobEvent{JobID: j.ID, Stage: "email_otp", Status: StatusWaitingInput})
	if !m.InputForStage(j.ID, "email_otp", map[string]any{"email_otp": "666666"}) {
		t.Fatal("next explicit prompt did not accept new response")
	}
}

func TestPublicStatusSeparatesWaitingInputFromValidation(t *testing.T) {
	for _, tc := range []struct{ stage, waiting string }{
		{"email_otp", "email_otp"}, {"password", "password"}, {"totp", "mfa_otp"}, {"mfa_otp", "mfa_otp"}, {"phone", "phone"}, {"sms_otp", "phone_otp"}, {"phone_otp", "phone_otp"}, {"totp_setup", "totp_setup_otp"}, {"totp_setup_otp", "totp_setup_otp"},
	} {
		for _, status := range []string{"awaiting_input", StatusWaitingInput} {
			if got := publicStatus(tc.stage, status); got != tc.waiting {
				t.Errorf("%s/%s=%s, want %s", tc.stage, status, got, tc.waiting)
			}
		}
		if got := publicStatus(tc.stage, StatusRunning); got != "working" {
			t.Errorf("running stage %s reopened %s", tc.stage, got)
		}
	}
	for stage, want := range map[string]string{"": "working", "oauth": "working", "start": "starting", "starting": "starting", "password_add": "password_add_starting", "totp_starting": "totp_starting", "refreshing": "refreshing", "finalizing": "finalizing"} {
		if got := publicStatus(stage, StatusRunning); got != want {
			t.Errorf("%s=%s, want %s", stage, got, want)
		}
		if !isTransientStatus(want) {
			t.Errorf("active display status %s cannot restore", want)
		}
	}
}

func TestPreparedInputIsUnavailableUntilPreparationCompletes(t *testing.T) {
	for _, cancelDuringPreparation := range []bool{false, true} {
		t.Run(fmt.Sprintf("canceled=%t", cancelDuringPreparation), func(t *testing.T) {
			m := NewManager(context.Background(), 1, nil, nil)
			ready := make(chan struct{})
			m.SetRunner(func(ctx context.Context, _ *Job, emit func(JobEvent)) error {
				emit(JobEvent{Stage: "sms_otp", Status: StatusWaitingInput})
				close(ready)
				<-ctx.Done()
				return ctx.Err()
			})
			m.Start()
			t.Cleanup(func() { _ = m.Shutdown(context.Background()) })
			j, err := m.Submit(Spec{Email: "prepared@example.test"})
			if err != nil {
				t.Fatal(err)
			}
			select {
			case <-ready:
			case <-time.After(time.Second):
				t.Fatal("runner did not start")
			}
			preparing, complete := make(chan struct{}), make(chan struct{})
			accepted := make(chan bool, 1)
			go func() {
				accepted <- m.InputForStageWithPreparation(j.ID, "phone_otp", map[string]any{"phone_otp": "765432"}, func() {
					close(preparing)
					<-complete
				})
			}()
			<-preparing
			current, _ := m.Get(j.ID)
			if current.Status != "working" {
				t.Error("preparation did not close prompt")
			}
			if _, ok := m.ConsumeInput(j.ID, "phone_otp"); ok {
				t.Error("input published before SMS preparation")
			}
			if m.InputForStage(j.ID, "phone_otp", map[string]any{"phone_otp": "999999"}) {
				t.Error("second submission entered reserved prompt")
			}
			if cancelDuringPreparation {
				m.Cancel(j.ID)
			}
			close(complete)
			if got := <-accepted; got == cancelDuringPreparation {
				t.Fatalf("accepted=%v after cancellation=%v", got, cancelDuringPreparation)
			}
			value, ok := m.ConsumeInput(j.ID, "phone_otp")
			if cancelDuringPreparation {
				if ok {
					t.Fatal("canceled preparation left consumable input")
				}
			} else if !ok || value != "765432" {
				t.Fatal("completed preparation did not publish original input")
			}
		})
	}
}

func TestPreparedInputCannotOverwriteNewPrompt(t *testing.T) {
	m := NewManager(context.Background(), 1, nil, nil)
	ready := make(chan struct{})
	m.SetRunner(func(ctx context.Context, _ *Job, emit func(JobEvent)) error {
		emit(JobEvent{Stage: "sms_otp", Status: StatusWaitingInput})
		close(ready)
		<-ctx.Done()
		return ctx.Err()
	})
	m.Start()
	t.Cleanup(func() { _ = m.Shutdown(context.Background()) })
	j, err := m.Submit(Spec{Email: "newprompt@example.test"})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-ready:
	case <-time.After(time.Second):
		t.Fatal("runner did not start")
	}
	preparing, complete := make(chan struct{}), make(chan struct{})
	accepted := make(chan bool, 1)
	go func() {
		accepted <- m.InputForStageWithPreparation(j.ID, "phone_otp", map[string]any{"phone_otp": "111111"}, func() { close(preparing); <-complete })
	}()
	<-preparing
	m.Emit(JobEvent{JobID: j.ID, Stage: "sms_otp", Status: StatusWaitingInput})
	if !m.InputForStage(j.ID, "phone_otp", map[string]any{"phone_otp": "222222"}) {
		t.Error("new prompt did not accept new response")
	}
	close(complete)
	if <-accepted {
		t.Fatal("old reservation survived new prompt")
	}
	if value, ok := m.ConsumeInput(j.ID, "phone_otp"); !ok || value != "222222" {
		t.Fatal("old preparation replaced new prompt response")
	}
}
