package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tosub2-go-port/internal/challenge"
	"tosub2-go-port/internal/store"
)

// This session is a local fixture with a perpetually pending verification
// document. It exercises the production solver deadline without any network,
// browser process, account or attempted provider verification.
type pendingVerificationSession struct{}

func (*pendingVerificationSession) Navigate(context.Context, string) error { return nil }
func (*pendingVerificationSession) Cookies() ([]map[string]any, error)     { return nil, nil }
func (*pendingVerificationSession) SetCookies([]map[string]any) error      { return nil }
func (*pendingVerificationSession) Evaluate(_ context.Context, script string) (any, error) {
	if strings.Contains(script, "document.title") {
		return true, nil
	}
	return map[string]any{"token": "", "provider": "chromium", "reason": "challenge pending"}, nil
}
func (*pendingVerificationSession) EvaluateFrame(context.Context, string, string) (any, error) {
	return "", nil
}
func (*pendingVerificationSession) HTML(context.Context) (string, error) { return "", nil }
func (*pendingVerificationSession) Close() error                         { return nil }

func TestVerificationTimeoutFailsWithoutCancelingJobAndSurvivesRestore(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(context.Background(), 1, st, nil)
	m.SetRunner(func(ctx context.Context, _ *Job, emit func(JobEvent)) error {
		emit(JobEvent{Stage: "browser_interaction_required", Status: StatusRunning, Prompt: "网站验证尚未完成，请在真实浏览器窗口完成验证"})
		_, err := (challenge.BrowserSolver{Session: &pendingVerificationSession{}, Timeout: 10 * time.Millisecond}).Sentinel(ctx, "https://auth.example.test/login", "https://sentinel.example.test/loader.js", "authorize_continue")
		if ctx.Err() != nil || !errors.Is(err, context.DeadlineExceeded) {
			return errors.New("fixture did not produce a child verification deadline")
		}
		return err
	})
	m.Start()
	t.Cleanup(func() { _ = m.Shutdown(context.Background()) })
	j, err := m.Submit(Spec{ID: "verification-timeout", Email: "timeout@example.test"})
	if err != nil {
		t.Fatal(err)
	}
	current := waitForStatus(t, m, j.ID, StatusFailed)
	for _, required := range []string{"SENTINEL_PAGE_CHALLENGE", "context deadline exceeded"} {
		if !strings.Contains(current.Error, required) {
			t.Fatalf("verification diagnosis lost %q: %q", required, current.Error)
		}
	}
	if current.Stage != "browser_interaction_required" || current.Error == "canceled" {
		t.Fatal("verification timeout was labeled user cancellation")
	}
	if err := m.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(st.Root, "job-meta.json"))
	if err != nil {
		t.Fatal(err)
	}
	var metadata map[string]any
	if err := json.Unmarshal(raw, &metadata); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "SENTINEL_PAGE_CHALLENGE") || !strings.Contains(string(raw), "context deadline exceeded") {
		t.Fatal("persisted metadata erased verification failure")
	}
	restored := NewManager(context.Background(), 1, st, nil)
	if err := restored.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, ok := restored.Get(j.ID)
	if !ok || got.Status != StatusFailed || !strings.Contains(got.Error, "SENTINEL_PAGE_CHALLENGE") {
		t.Fatal("restart lost the failed challenge diagnosis")
	}
}

func TestChildContextFailuresAreNotUserCancellation(t *testing.T) {
	for _, operation := range []string{"login", "setup-2fa", "add-password"} {
		for _, failure := range []string{"deadline", "canceled"} {
			t.Run(operation+"/"+failure, func(t *testing.T) {
				m := NewManager(context.Background(), 1, nil, nil)
				m.SetRunner(func(ctx context.Context, _ *Job, _ func(JobEvent)) error {
					var child context.Context
					var cancel context.CancelFunc
					if failure == "deadline" {
						child, cancel = context.WithTimeout(ctx, time.Millisecond)
					} else {
						child, cancel = context.WithCancel(ctx)
					}
					defer cancel()
					if failure == "canceled" {
						cancel()
					}
					<-child.Done()
					if ctx.Err() != nil {
						return errors.New("job context unexpectedly canceled")
					}
					return fmt.Errorf("BROWSER_STEP_FAILED: password=fixture-child-secret: %w", child.Err())
				})
				j, err := m.Submit(Spec{Email: "child@example.test", Operation: operation})
				if err != nil {
					t.Fatal(err)
				}
				if operation != "login" {
					m.UpdateResult(j.ID, true, "")
				}
				m.Start()
				t.Cleanup(func() { _ = m.Shutdown(context.Background()) })
				want := StatusFailed
				if operation != "login" {
					want = StatusSucceeded
				}
				got := waitForStatus(t, m, j.ID, want)
				if !strings.Contains(got.Error, "BROWSER_STEP_FAILED") || got.Error == "canceled" || strings.Contains(got.Error, "fixture-child-secret") {
					t.Fatal("child operation failure was discarded or not sanitized")
				}
				if strings.Contains(got.PasswordAddError, "用户取消") || strings.Contains(got.TOTPSetupError, "用户取消") {
					t.Fatal("timed out account operation blamed user cancellation")
				}
				if operation != "login" && !got.ResultSaved {
					t.Fatal("suboperation failure discarded prior authorization")
				}
			})
		}
	}
}

func TestJobCancellationAndServiceInterruptionRemainDistinct(t *testing.T) {
	for _, source := range []string{"user_cancel", "cancel_all", "service_shutdown", "parent_canceled"} {
		t.Run(source, func(t *testing.T) {
			parent, stopParent := context.WithCancel(context.Background())
			defer stopParent()
			m := NewManager(parent, 1, nil, nil)
			ready := make(chan struct{})
			m.SetRunner(func(ctx context.Context, _ *Job, emit func(JobEvent)) error {
				emit(JobEvent{Stage: "browser_interaction_required", Status: StatusRunning})
				close(ready)
				<-ctx.Done()
				return fmt.Errorf("SENTINEL_PAGE_CHALLENGE: verification stopped: %w", ctx.Err())
			})
			j, err := m.Submit(Spec{Email: "cancellation@example.test"})
			if err != nil {
				t.Fatal(err)
			}
			m.Start()
			t.Cleanup(func() { _ = m.Shutdown(context.Background()) })
			select {
			case <-ready:
			case <-time.After(time.Second):
				t.Fatal("runner did not start")
			}
			switch source {
			case "user_cancel":
				if !m.Cancel(j.ID) {
					t.Fatal("cancel rejected")
				}
			case "cancel_all":
				if m.CancelAll() != 1 {
					t.Fatal("cancel-all count mismatch")
				}
			case "service_shutdown":
				if err := m.Shutdown(context.Background()); err != nil {
					t.Fatal(err)
				}
			case "parent_canceled":
				stopParent()
			}
			want := StatusCanceled
			if source == "service_shutdown" || source == "parent_canceled" {
				want = "interrupted"
			}
			got := waitForStatus(t, m, j.ID, want)
			if want == StatusCanceled && got.Error != "canceled" {
				t.Fatal("user cancellation contract changed")
			}
			if want == "interrupted" && !strings.Contains(got.Error, "服务关闭") {
				t.Fatal("service cancellation lost interruption reason")
			}
		})
	}
}
