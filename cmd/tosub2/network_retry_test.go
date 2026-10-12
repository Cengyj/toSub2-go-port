package main

import (
	"context"
	"io"
	"testing"

	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/transport"
)

func TestNetworkRetryReporterDoesNotReopenInputOrReplacePrompt(t *testing.T) {
	m := jobs.NewManager(t.Context(), 1, nil, nil)
	j, err := m.Submit(jobs.Spec{Email: "fixture@example.invalid"})
	if err != nil {
		t.Fatal(err)
	}
	m.Emit(jobs.JobEvent{JobID: j.ID, Stage: "email_otp", Status: jobs.StatusWaitingInput, Prompt: "请输入验证码"})
	ctx, cancel := context.WithCancel(t.Context())
	report := networkRetryReporter(ctx, j.ID, "", m.Emit)
	retry := transport.ReadRetryEvent{Retry: 1, MaxRetries: 3, Err: io.EOF}
	report(retry)
	current, _ := m.Get(j.ID)
	if current.Stage != "email_otp" || current.Status != "email_otp" || current.Prompt != "请输入验证码" || current.OperationStatus != "waiting" {
		t.Fatal("retry log changed the manual input state")
	}
	last := current.LogEntries[len(current.LogEntries)-1]
	if last.Stage != "network_retry" || last.Message != retry.Message() {
		t.Fatal("retry was not appended to operation logs")
	}
	m.Emit(jobs.JobEvent{JobID: j.ID, Stage: "authorize", Status: jobs.StatusRunning, Prompt: "正在授权"})
	report(transport.ReadRetryEvent{Retry: 2, MaxRetries: 3, Err: io.EOF})
	current, _ = m.Get(j.ID)
	if current.Stage != "authorize" || current.Prompt != "正在授权" || current.OperationStatus != "running" {
		t.Fatal("late polling retry reopened the earlier input")
	}
	count := len(current.LogEntries)
	cancel()
	report(retry)
	current, _ = m.Get(j.ID)
	if len(current.LogEntries) != count {
		t.Fatal("canceled polling still emitted retries")
	}
}
