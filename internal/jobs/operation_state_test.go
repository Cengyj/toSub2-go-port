package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tosub2-go-port/internal/store"
)

func TestOperationOutcomeDistinguishesFailedActionFromUsableAuthorization(t *testing.T) {
	for _, operation := range []string{"setup-2fa", "add-password"} {
		t.Run(operation, func(t *testing.T) {
			st, err := store.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			m := NewManager(t.Context(), 1, st, nil)
			m.SetRunner(func(_ context.Context, j *Job, _ func(JobEvent)) error {
				failure := errors.New("fixture action failed")
				if totpOperation(operation) {
					if err := m.FinishTOTPWorkflow(j.ID, failure); err != nil {
						return err
					}
				}
				return failure
			})
			j, err := m.Submit(Spec{Email: "owner@example.invalid", Operation: operation})
			if err != nil {
				t.Fatal(err)
			}
			m.UpdateResult(j.ID, true, "old output")
			m.Start()
			current := waitForStatus(t, m, j.ID, StatusSucceeded)
			if current.OperationStatus != "failed" || current.OperationError != "fixture action failed" || !current.ResultSaved {
				t.Fatal("failed action became successful or discarded existing authorization")
			}
			if err := m.Shutdown(t.Context()); err != nil {
				t.Fatal(err)
			}
			restored := NewManager(t.Context(), 1, st, nil)
			if err := restored.Restore(t.Context()); err != nil {
				t.Fatal(err)
			}
			current, _ = restored.Get(j.ID)
			if current.OperationStatus != "failed" || current.OperationError != "fixture action failed" {
				t.Fatal("restart discarded latest action failure")
			}
		})
	}
}

func TestProtocolCompletionWaitsForRunnerFinalization(t *testing.T) {
	m := NewManager(t.Context(), 1, nil, nil)
	ready, finish := make(chan struct{}), make(chan struct{})
	m.SetRunner(func(ctx context.Context, _ *Job, emit func(JobEvent)) error {
		emit(JobEvent{Stage: "email_otp", Status: StatusWaitingInput, Prompt: "请输入邮箱验证码"})
		emit(JobEvent{Stage: "completed", Status: "succeeded"})
		close(ready)
		select {
		case <-finish:
			return errors.New("secure result save failed")
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	defer m.Shutdown(context.Background())
	j, _ := m.Submit(Spec{Email: "owner@example.invalid"})
	events, unsubscribe := m.Subscribe(20)
	defer unsubscribe()
	m.Start()
	select {
	case <-ready:
	case <-time.After(time.Second):
		t.Fatal("runner did not reach finalization")
	}
	current, _ := m.Get(j.ID)
	if current.Status == StatusSucceeded || current.OperationStatus != "running" || current.Stage != "finalizing" || current.Prompt != "正在保存授权结果" {
		t.Fatalf("protocol completion escaped before finalization: %#v", current)
	}
	for len(events) > 0 {
		if e := <-events; e.Status == "succeeded" || e.Status == StatusSucceeded {
			t.Fatal("SSE announced success before finalization")
		}
	}
	close(finish)
	current = waitForStatus(t, m, j.ID, StatusFailed)
	if current.OperationStatus != "failed" || current.OperationError != "secure result save failed" || strings.Contains(current.Prompt, "请输入") {
		t.Fatal("finalization failure retained stale input prompt")
	}
}

func TestNextOperationClearsOldAttemptStateAndRetainsLogHistory(t *testing.T) {
	m := NewManager(t.Context(), 1, nil, nil)
	j, _ := m.Submit(Spec{Email: "owner@example.invalid"})
	m.Cancel(j.ID)
	m.mu.Lock()
	current := m.jobs[j.ID]
	current.Stage, current.Prompt = "email_otp", "old input prompt"
	current.Error, current.LastError = "old error", "old credential warning"
	current.MailStatus, current.MailAPIError = "timeout", "old mail error"
	current.SMSStatus, current.SMSError = "error", "old SMS error"
	current.PasswordAddError, current.TOTPSetupError = "old password error", "old 2FA error"
	m.mu.Unlock()
	queued, err := m.QueueOperation(j.ID, "relogin", nil)
	if err != nil {
		t.Fatal(err)
	}
	if queued.Stage != "queued" || queued.Prompt == "old input prompt" || queued.OperationStatus != "queued" || queued.OperationError != "" || queued.Error != "" || queued.LastError != "" || queued.MailStatus != "" || queued.MailAPIError != "" || queued.SMSError != "" || queued.SMSStatus != "" || queued.PasswordAddError != "" || queued.TOTPSetupError != "" {
		t.Fatal("next action retained previous attempt display state")
	}
	if len(queued.LogEntries) < 3 || queued.LogEntries[len(queued.LogEntries)-1].OperationAt.Equal(queued.LogEntries[0].OperationAt) {
		t.Fatal("log history no longer identifies separate operations")
	}
}

func TestSuccessfulActionEndsWithCompletionMessageInsteadOfActivePrompt(t *testing.T) {
	m := NewManager(t.Context(), 1, nil, nil)
	m.SetRunner(func(_ context.Context, j *Job, emit func(JobEvent)) error {
		m.UpdateResult(j.ID, true, "fixture saved output")
		emit(JobEvent{Stage: "completed", Status: "succeeded"})
		return nil
	})
	defer m.Shutdown(context.Background())
	j, _ := m.Submit(Spec{Email: "owner@example.invalid"})
	m.Start()
	current := waitForStatus(t, m, j.ID, StatusSucceeded)
	if current.OperationStatus != "succeeded" || current.Prompt != "授权已完成，结果已保存" {
		t.Fatal("successful action retained its saving/input prompt")
	}
	last := current.LogEntries[len(current.LogEntries)-1]
	if last.OperationStatus != "succeeded" || last.Message != current.Prompt {
		t.Fatal("terminal log does not agree with successful action state")
	}
}

func TestCredentialWarningDoesNotBecomeOperationFailureAfterRestore(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(t.Context(), 1, st, nil)
	m.SetRunner(func(_ context.Context, j *Job, _ func(JobEvent)) error {
		m.mu.Lock()
		m.jobs[j.ID].LastError = "fixture credential store warning"
		m.mu.Unlock()
		return nil
	})
	j, _ := m.Submit(Spec{Email: "owner@example.invalid"})
	m.Start()
	current := waitForStatus(t, m, j.ID, StatusSucceeded)
	if current.OperationStatus != "succeeded" || current.OperationError != "" || current.LastError == "" {
		t.Fatal("successful action hid credential warning or became failed")
	}
	if err := m.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	restored := NewManager(t.Context(), 1, st, nil)
	if err := restored.Restore(t.Context()); err != nil {
		t.Fatal(err)
	}
	current, _ = restored.Get(j.ID)
	if current.OperationStatus != "succeeded" || current.OperationError != "" || current.LastError != "fixture credential store warning" {
		t.Fatal("credential warning changed persisted action outcome")
	}
}

func TestCanceledTOTPWorkflowRemainsCanceledAfterRestore(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(t.Context(), 1, st, nil)
	j, err := m.Submit(Spec{Email: "owner@example.invalid", Operation: "setup-2fa"})
	if err != nil {
		t.Fatal(err)
	}
	if !m.Cancel(j.ID) {
		t.Fatal("queued workflow cancellation was rejected")
	}
	restored := NewManager(t.Context(), 1, st, nil)
	if err := restored.Restore(t.Context()); err != nil {
		t.Fatal(err)
	}
	current, _ := restored.Get(j.ID)
	if current.OperationStatus != "canceled" || current.OperationError != "" || current.TOTPWorkflowStatus != "interrupted" {
		t.Fatal("user cancellation became a service interruption after restart")
	}
}

func TestOperationLogsRedactKnownSecretsBeforeSubscriberAndPersistence(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(t.Context(), 1, st, nil)
	secret, key := "fixture-unlabelled-password", "fixture-unlabelled-admin-key"
	j, _ := m.Submit(Spec{Email: "owner@example.invalid", Input: map[string]any{"password": secret, "sub2apiConfig": map[string]any{"adminApiKey": key}}})
	// Submit deliberately discards unrelated workflow config. A public event
	// boundary must also protect a running workflow's nested private config.
	m.mu.Lock()
	m.jobs[j.ID].Input["sub2apiConfig"] = map[string]any{"adminApiKey": key}
	m.mu.Unlock()
	events, unsubscribe := m.Subscribe(20)
	defer unsubscribe()
	e := JobEvent{JobID: j.ID, Stage: "login", Status: StatusRunning, Prompt: secret + " " + key, Data: map[string]any{"error": secret + " " + key}}
	m.Emit(e)
	m.Emit(e)
	current, _ := m.Get(j.ID)
	if len(current.LogEntries) != 2 {
		t.Fatal("consecutive duplicate events were not collapsed")
	}
	encoded, _ := json.Marshal(<-events)
	metadata, _ := os.ReadFile(filepath.Join(st.Root, "job-meta.json"))
	public, _ := json.Marshal(SanitizeJobForPublic(current))
	for _, value := range []string{secret, key} {
		if strings.Contains(string(encoded), value) || strings.Contains(string(metadata), value) || strings.Contains(string(public), value) {
			t.Fatal("known secret escaped event/log/metadata boundary")
		}
	}
}
