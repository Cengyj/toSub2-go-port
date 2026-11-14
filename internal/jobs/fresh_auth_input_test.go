package jobs

import (
	"context"
	"errors"
	"testing"
)

func TestNewOperationDiscardsPriorOneTimeInputsAndKeepsCredentials(t *testing.T) {
	for _, action := range []string{"retry", "queue", "requeue", "input", "auto-repair"} {
		t.Run(action, func(t *testing.T) {
			m := NewManager(context.Background(), 1, nil, nil)
			defer m.Shutdown(context.Background())
			job, err := m.Submit(Spec{ID: "fresh-input", Email: "owner@example.test", Input: map[string]any{"password": "keep-password", "totpSecret": "JBSWY3DPEHPK3PXP", "email_otp": "old-email", "mfa_otp": "old-mfa", "phone_otp": "old-phone", "code": "old-code", "value": "old-value"}})
			if err != nil {
				t.Fatal(err)
			}
			m.Cancel(job.ID)
			values := map[string]any{"email_otp": "explicit-new-input"}
			switch action {
			case "retry":
				_, err = m.RetryWithValues(job.ID, values)
			case "queue":
				_, err = m.QueueOperation(job.ID, "relogin", values)
			case "requeue":
				_, err = m.Requeue(job.ID, "relogin")
			case "input":
				if !m.Input(job.ID, values) {
					t.Fatal("explicit new input was rejected")
				}
			case "auto-repair":
				_, err = m.QueueAutoRepair(job.ID, []int{1}, "synthetic-backend")
			}
			if err != nil {
				t.Fatal(err)
			}
			current, _ := m.InputCopy(job.ID)
			if current["password"] != "keep-password" || current["totpSecret"] != "JBSWY3DPEHPK3PXP" {
				t.Fatal("account credential removed")
			}
			for _, key := range []string{"mfa_otp", "phone_otp", "code", "value"} {
				if _, ok := current[key]; ok {
					t.Fatalf("old input survived: %s", key)
				}
			}
			if action != "requeue" && action != "auto-repair" && current["email_otp"] != "explicit-new-input" {
				t.Fatal("explicit new input lost")
			}
			if (action == "requeue" || action == "auto-repair") && current["email_otp"] != nil {
				t.Fatal("requeue retained old code")
			}
		})
	}
}

func TestCompatibilityInputRestartsWithoutReplayingPriorCodes(t *testing.T) {
	m := NewManager(context.Background(), 1, nil, nil)
	m.SetRunner(func(ctx context.Context, job *Job, emit func(JobEvent)) error {
		if _, ok := m.ConsumeInput(job.ID, "email_otp", "emailOtp", "code", "value"); ok {
			return errors.New("ended operation's email code was replayed")
		}
		if code, ok := m.ConsumeInput(job.ID, "totp", "mfa_otp"); !ok || code != "new-mfa" {
			return errors.New("explicit new MFA input was not delivered")
		}
		if password, ok := m.ConsumeInput(job.ID, "password"); !ok || password != "keep-password" {
			return errors.New("normal account password was lost")
		}
		return nil
	})
	defer m.Shutdown(context.Background())
	job, err := m.Submit(Spec{ID: "compat-input", Email: "owner@example.test", Input: map[string]any{
		"password": "keep-password", "emailOtp": "old-email", "totp": "old-mfa", "code": "old-code", "value": "old-value",
	}})
	if err != nil {
		t.Fatal(err)
	}
	m.Cancel(job.ID)
	if !m.Input(job.ID, map[string]any{"mfa_otp": "new-mfa"}) {
		t.Fatal("explicit new input was rejected")
	}
	// A second edit before the restarted operation is dispatched still belongs
	// to that queued operation and must not erase its explicitly supplied code.
	if !m.Input(job.ID, map[string]any{"phone": "+12025550123"}) {
		t.Fatal("queued operation input was rejected")
	}
	m.Start()
	completed := waitForStatus(t, m, job.ID, StatusCompleted)
	if completed.Attempts != 1 || completed.Error != "" {
		t.Fatal("compatibility input did not complete exactly one fresh operation")
	}
}
