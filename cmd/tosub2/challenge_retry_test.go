package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"tosub2-go-port/internal/challenge"
)

func TestSameBrowserChallengeRetryDelayAndLimit(t *testing.T) {
	blocked := errors.New("temporary browser navigation failure")
	calls, notifications := 0, 0
	var starts []time.Time
	err := retryBrowserChallenge(context.Background(), 3, 15*time.Millisecond, func() error {
		calls++
		starts = append(starts, time.Now())
		return blocked
	}, func() { notifications++ })
	if !errors.Is(err, blocked) || calls != 4 || notifications != 3 {
		t.Fatalf("retry limit: calls=%d notifications=%d error=%v", calls, notifications, err)
	}
	for i := 1; i < len(starts); i++ {
		if starts[i].Sub(starts[i-1]) < 15*time.Millisecond {
			t.Fatal("retry bypassed configured delay")
		}
	}
	calls = 0
	if err = retryBrowserChallenge(context.Background(), 3, 0, func() error { calls++; return nil }, nil); err != nil || calls != 1 {
		t.Fatalf("successful browser challenge was repeated: %d %v", calls, err)
	}
}

func TestSameBrowserChallengeRetryCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	started := time.Now()
	err := retryBrowserChallenge(ctx, 3, time.Hour, func() error { calls++; return errors.New("pending") }, cancel)
	if !errors.Is(err, context.Canceled) || calls != 1 || time.Since(started) > time.Second {
		t.Fatalf("retry delay ignored cancellation: calls=%d err=%v", calls, err)
	}
}

func TestVerificationWaitAndChildCancellationDoNotRetryNavigation(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"pending", challenge.ErrVerificationPending},
		{"rejected", challenge.ErrVerificationRejected},
		{"wrapped_pending", fmt.Errorf("verification incomplete: %w", challenge.ErrVerificationPending)},
		{"wrapped_rejected", fmt.Errorf("verification not accepted: %w", challenge.ErrVerificationRejected)},
		{"child_timeout", context.DeadlineExceeded},
		{"child_canceled", context.Canceled},
		{"wrapped_timeout", fmt.Errorf("challenge token not observed: challenge pending: %w", context.DeadlineExceeded)},
		{"wrapped_canceled", fmt.Errorf("browser operation stopped: %w", context.Canceled)},
		{"pending_and_timeout", errors.Join(challenge.ErrVerificationPending, context.DeadlineExceeded)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls, notifications := 0, 0
			err := retryBrowserChallenge(context.Background(), 20, 0, func() error {
				calls++
				return tc.err
			}, func() { notifications++ })
			if err != tc.err || calls != 1 || notifications != 0 {
				t.Fatalf("non-retryable verification reloaded: calls=%d notifications=%d error=%v", calls, notifications, err)
			}
		})
	}
}

func TestBrowserChallengeParentCancellationWinsBeforeOrDuringSolve(t *testing.T) {
	for _, at := range []string{"before_solve", "during_failed_solve", "during_successful_solve"} {
		t.Run(at, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if at == "before_solve" {
				cancel()
			}
			calls, notifications := 0, 0
			err := retryBrowserChallenge(ctx, 3, 0, func() error {
				calls++
				cancel()
				if at == "during_successful_solve" {
					return nil
				}
				return errors.New("transient browser transport failure")
			}, func() { notifications++ })
			wantCalls := 1
			if at == "before_solve" {
				wantCalls = 0
			}
			if !errors.Is(err, context.Canceled) || calls != wantCalls || notifications != 0 {
				t.Fatalf("parent cancellation was ignored: calls=%d notifications=%d error=%v", calls, notifications, err)
			}
		})
	}
}

// The fixture supplies a pending document locally; no browser process or
// network request is made. Each Navigate represents a page replacement that
// would discard in-progress user verification in a real Chromium document.
type retryPendingVerificationSession struct{ navigations int }

func (s *retryPendingVerificationSession) Navigate(context.Context, string) error {
	s.navigations++
	return nil
}
func (*retryPendingVerificationSession) Cookies() ([]map[string]any, error) { return nil, nil }
func (*retryPendingVerificationSession) SetCookies([]map[string]any) error  { return nil }
func (*retryPendingVerificationSession) Evaluate(_ context.Context, script string) (any, error) {
	if strings.Contains(script, "document.title") {
		return true, nil
	}
	return map[string]any{"token": "", "provider": "chromium", "reason": "challenge pending"}, nil
}
func (*retryPendingVerificationSession) EvaluateFrame(context.Context, string, string) (any, error) {
	return "", nil
}
func (*retryPendingVerificationSession) HTML(context.Context) (string, error) { return "", nil }
func (*retryPendingVerificationSession) Close() error                         { return nil }

func TestPendingBrowserSolverWaitDoesNotNavigateAgain(t *testing.T) {
	session := &retryPendingVerificationSession{}
	notifications := 0
	ctx := context.Background()
	err := retryBrowserChallenge(ctx, 3, 0, func() error {
		_, err := (challenge.BrowserSolver{Session: session, Timeout: 5 * time.Millisecond}).SolveContext(ctx, "https://verification.example.test/")
		return err
	}, func() { notifications++ })
	if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, challenge.ErrVerificationPending) || session.navigations != 1 || notifications != 0 {
		t.Fatalf("pending real solver reloaded verification: navigations=%d notifications=%d error=%v", session.navigations, notifications, err)
	}
}
