package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestSecurityOperationCancellationRetainsAuthorization(t *testing.T) {
	for _, operation := range []string{"setup-2fa", "add-password"} {
		for _, output := range []bool{false, true} {
			for _, running := range []bool{false, true} {
				for _, all := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/output=%v/running=%v/all=%v", operation, output, running, all), func(t *testing.T) {
						m := NewManager(context.Background(), 1, nil, nil)
						entered := make(chan struct{})
						m.SetRunner(func(ctx context.Context, _ *Job, emit func(JobEvent)) error {
							emit(JobEvent{Stage: operation, Status: StatusRunning})
							close(entered)
							<-ctx.Done()
							return ctx.Err()
						})
						j, _ := m.Submit(Spec{Email: "owner@example.test", Operation: operation})
						m.UpdateResult(j.ID, output, "")
						if running {
							m.Start()
							select {
							case <-entered:
							case <-time.After(time.Second):
								t.Fatal("runner did not start")
							}
						}
						if all {
							if m.CancelAll() != 1 {
								t.Fatal("cancel-all count mismatch")
							}
						} else if !m.Cancel(j.ID) {
							t.Fatal("cancel rejected")
						}
						want := "resume_available"
						if output {
							want = StatusSucceeded
						}
						deadline := time.Now().Add(time.Second)
						for {
							current, _ := m.Get(j.ID)
							if current.Status == want {
								if current.ResultSaved != output || (operation == "setup-2fa" && current.TOTPSetupError == "") || (operation == "add-password" && current.PasswordAddError == "") {
									t.Fatal("cancellation lost authorization or operation-specific result")
								}
								break
							}
							if time.Now().After(deadline) {
								t.Fatalf("cancel status=%s, want=%s", current.Status, want)
							}
							time.Sleep(time.Millisecond)
						}
						_ = m.Shutdown(context.Background())
					})
				}
			}
		}
	}
}

func TestStructuredPhoneMetadataAndManualNumberUpdates(t *testing.T) {
	m := NewManager(context.Background(), 1, nil, nil)
	j, _ := m.Submit(Spec{Email: "owner@example.test"})
	m.Input(j.ID, map[string]any{"phone": "+12025550123"})
	current, _ := m.Get(j.ID)
	if current.CurrentPhone != "+12025550123" {
		t.Fatal("manual number not reflected in job metadata")
	}
	m.Emit(JobEvent{JobID: j.ID, Stage: "sms_otp_rejected", Status: StatusRunning, Data: map[string]any{"currentPhone": "+12025550123", "phoneError": "password=fixture-sensitive-value", "securityCheckRequired": true, "password": "fixture-sensitive-value"}})
	current, _ = m.Get(j.ID)
	data, _ := json.Marshal(SanitizeJobForPublic(current))
	if strings.Contains(string(data), "fixture-sensitive-value") || current.PhoneError == "" || !current.SecurityCheckRequired {
		t.Fatal("structured phone metadata missing or private data exposed")
	}
	m.Input(j.ID, map[string]any{"phone_otp": "p"})
	current, _ = m.Get(j.ID)
	if current.CurrentPhone != "" {
		t.Fatal("change-phone kept obsolete current number")
	}
	m.Emit(JobEvent{JobID: j.ID, Stage: "phone_verified", Status: StatusRunning, Data: map[string]any{"currentPhone": "+12025550124"}})
	current, _ = m.Get(j.ID)
	if current.CurrentPhone != "+12025550124" || current.PhoneError != "" || current.SecurityCheckRequired {
		t.Fatal("successful verification retained phone security error")
	}
}

func TestManualRecoveryClearsMonitorTargetsAndSelectsCorrectOperation(t *testing.T) {
	for _, status := range []string{StatusFailed, StatusCanceled, "resume_available", "reauth_required"} {
		t.Run(status, func(t *testing.T) {
			m := NewManager(context.Background(), 1, nil, nil)
			j, _ := m.Submit(Spec{Email: "owner@example.test", Operation: "add-password"})
			m.Cancel(j.ID)
			m.Emit(JobEvent{JobID: j.ID, Status: status})
			m.UpdateAutoRepair(j.ID, []int{17}, "remote", "account_disabled", true)
			next, err := m.RetryWithValues(j.ID, map[string]any{"proxyUrl": "http://127.0.0.1:8888"})
			if err != nil {
				t.Fatal(err)
			}
			want := "full"
			if status == "resume_available" {
				want = "resume"
			}
			if next.Operation != want || next.AutoRepairBlocked || next.AutoRepairBlockedReason != "" || len(next.AutoRepairPendingAccountIDs) != 0 || next.AutoRepairBackend != "" || !next.ProxyConfigured {
				t.Fatalf("manual retry did not reset recovery state: operation=%s blocked=%v targets=%v", next.Operation, next.AutoRepairBlocked, next.AutoRepairPendingAccountIDs)
			}
		})
	}
	m := NewManager(context.Background(), 1, nil, nil)
	j, _ := m.Submit(Spec{Email: "relogin@example.test"})
	m.Cancel(j.ID)
	m.UpdateAutoRepair(j.ID, []int{17}, "remote", "account_disabled", true)
	next, err := m.QueueOperation(j.ID, "relogin", nil)
	if err != nil || next.AutoRepairBlocked || next.AutoRepairBlockedReason != "" || len(next.AutoRepairPendingAccountIDs) != 0 {
		t.Fatal("manual relogin retained monitor block", err)
	}
}

func TestRejectedRetryDoesNotModifyCredentials(t *testing.T) {
	m := NewManager(context.Background(), 1, nil, nil)
	j, _ := m.Submit(Spec{Email: "owner@example.test", Input: map[string]any{"password": "old-synthetic-password"}})
	m.Cancel(j.ID)
	m.Emit(JobEvent{JobID: j.ID, Status: StatusSucceeded})
	if _, err := m.RetryWithValues(j.ID, map[string]any{"password": "new-synthetic-password"}); err == nil {
		t.Fatal("completed task accepted retry")
	}
	values, _ := m.InputCopy(j.ID)
	if values["password"] != "old-synthetic-password" {
		t.Fatal("rejected retry changed credentials")
	}
}

func TestCredentialReadCancellationDoesNotBlockManager(t *testing.T) {
	m := NewManager(context.Background(), 1, nil, nil)
	j, _ := m.Submit(Spec{Email: "owner@example.test"})
	vault := &cancelRestoreVault{blockLoadAt: 1, entered: make(chan struct{})}
	m.SetCredentialStore(vault)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := m.LoadCredentialsContext(ctx, j.ID); done <- err }()
	select {
	case <-vault.entered:
	case <-time.After(time.Second):
		t.Fatal("credential read did not start")
	}
	read := make(chan bool, 1)
	go func() { _, exists := m.Get(j.ID); read <- exists }()
	select {
	case exists := <-read:
		if !exists {
			t.Fatal("job missing")
		}
	case <-time.After(time.Second):
		t.Fatal("OS credential read held the scheduler lock")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("request cancellation did not reach credential store", err)
		}
	case <-time.After(time.Second):
		t.Fatal("credential read ignored cancellation")
	}
}
