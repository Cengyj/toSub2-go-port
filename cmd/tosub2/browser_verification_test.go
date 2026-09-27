package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"tosub2-go-port/internal/challenge"
	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/store"
)

func TestBrowserReplayRequiresBoundReadiness(t *testing.T) {
	for _, result := range []challenge.Result{{}, {Solved: true}, {RetryReady: true}, {SessionBound: true}} {
		if validateBrowserReplayReadiness(result, nil) == nil {
			t.Fatal("unconfirmed or unbound browser result allowed replay")
		}
	}
	for _, result := range []challenge.Result{{Solved: true, SessionBound: true}, {RetryReady: true, SessionBound: true}} {
		if err := validateBrowserReplayReadiness(result, nil); err != nil {
			t.Fatal(err)
		}
	}
	failure := errors.Join(challenge.ErrVerificationPending, context.DeadlineExceeded)
	if !errors.Is(validateBrowserReplayReadiness(challenge.Result{Solved: true, SessionBound: true}, failure), failure) {
		t.Fatal("browser error overwritten by result")
	}
}

func TestVerificationFailureClearsRetryPromptAndPersists(t *testing.T) {
	for _, tc := range []struct {
		name    string
		failure error
		stage   string
	}{
		{"pending", errors.Join(challenge.ErrVerificationPending, context.DeadlineExceeded), "browser_verification_incomplete"},
		{"rejected", challenge.ErrVerificationRejected, "browser_verification_rejected"},
		{"sdk", errors.New("SENTINEL_SDK_FAILED: loader_failed"), "browser_verification_failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, err := store.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			m := jobs.NewManager(context.Background(), 1, st, nil)
			m.SetRunner(func(_ context.Context, j *jobs.Job, emit func(jobs.JobEvent)) error {
				emit(jobs.JobEvent{JobID: j.ID, Stage: "browser_challenge_retry", Status: jobs.StatusRunning, Prompt: "使用原代理和原浏览器会话重试安全验证"})
				emit(browserVerificationFailureEvent(j.ID, tc.failure))
				return tc.failure
			})
			m.Start()
			t.Cleanup(func() { _ = m.Shutdown(context.Background()) })
			j, err := m.Submit(jobs.Spec{Email: "fixture@example.test"})
			if err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(3 * time.Second)
			for {
				got, ok := m.Get(j.ID)
				if ok && got.Status == jobs.StatusFailed {
					if got.Stage != tc.stage || got.Prompt == "使用原代理和原浏览器会话重试安全验证" || got.Error == "canceled" || got.SecurityCheckRequired {
						t.Fatalf("incorrect final verification state: stage=%s prompt=%s", got.Stage, got.Prompt)
					}
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("job did not settle")
				}
				time.Sleep(time.Millisecond)
			}
			if err := m.Shutdown(context.Background()); err != nil {
				t.Fatal(err)
			}
			restored := jobs.NewManager(context.Background(), 1, st, nil)
			if err := restored.Restore(context.Background()); err != nil {
				t.Fatal(err)
			}
			got, ok := restored.Get(j.ID)
			if !ok || got.Status != jobs.StatusFailed || !strings.Contains(got.Prompt, "已结束") && !strings.Contains(got.Prompt, "已停止") {
				t.Fatal("terminal browser prompt lost on restore")
			}
		})
	}
}
