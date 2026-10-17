package sub2api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

type deadlineProbeTransport struct {
	t    *testing.T
	want time.Duration
}

func (p deadlineProbeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	deadline, ok := req.Context().Deadline()
	remaining := time.Until(deadline)
	if !ok || remaining > p.want || remaining < p.want-time.Second {
		p.t.Errorf("Sub2API request budget = %s, want %s", remaining, p.want)
	}
	return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"data":[]}`))}, nil
}

func TestLegacySub2APIRequestDeadlineAndCallerCancellation(t *testing.T) {
	for _, tc := range []struct {
		name            string
		configuredLimit time.Duration
		parentLimit     time.Duration
		want            time.Duration
	}{
		{"default-per-attempt", 0, 0, 30 * time.Second},
		{"configured", 5 * time.Second, 0, 5 * time.Second},
		{"configured-long", 65 * time.Second, 0, 65 * time.Second},
		{"configured-longer", 3 * time.Minute, 0, 3 * time.Minute},
		{"earlier-parent", 0, 3 * time.Second, 3 * time.Second},
		{"configured-long-earlier-parent", 3 * time.Minute, 2 * time.Second, 2 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := New(Config{BaseURL: "https://fixture.invalid", APIKey: "synthetic-admin-key", Timeout: tc.configuredLimit})
			client.http.Transport = deadlineProbeTransport{t: t, want: tc.want}
			ctx := context.Background()
			if tc.parentLimit > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, tc.parentLimit)
				defer cancel()
			}
			if _, err := client.Groups(ctx); err != nil {
				t.Fatal(err)
			}
		})
	}
}

type deadlineWaitTransport struct{ calls int }

func (p *deadlineWaitTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	p.calls++
	<-req.Context().Done()
	return nil, req.Context().Err()
}

func TestSub2APIWholeRequestBudgetIsFinite(t *testing.T) {
	for _, tc := range []struct {
		name            string
		configuredLimit time.Duration
		parentLimit     time.Duration
		wantElapsed     time.Duration
		wantCalls       int
	}{
		{"default", 0, 0, 123 * time.Second, 4},
		{"configured-short", 5 * time.Second, 0, 23 * time.Second, 4},
		{"configured-long", 120 * time.Second, 0, 483 * time.Second, 4},
		{"earlier-parent", 120 * time.Second, 120 * time.Second, 120 * time.Second, 1},
		{"parent-during-retry", 120 * time.Second, 135 * time.Second, 135 * time.Second, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// This checks the actual retry loop with a synthetic clock and no
			// sockets. The separate 65-second console test retains its real wait.
			synctest.Test(t, func(t *testing.T) {
				client := New(Config{BaseURL: "https://fixture.invalid", APIKey: "synthetic-admin-key", Timeout: tc.configuredLimit})
				probe := &deadlineWaitTransport{}
				client.http.Transport = probe
				ctx := context.Background()
				if tc.parentLimit > 0 {
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, tc.parentLimit)
					defer cancel()
				}
				started := time.Now()
				_, err := client.Groups(ctx)
				if elapsed := time.Since(started); !errors.Is(err, context.DeadlineExceeded) || elapsed != tc.wantElapsed || probe.calls != tc.wantCalls {
					t.Fatalf("retry budget: elapsed=%s want=%s calls=%d want=%d error=%v", elapsed, tc.wantElapsed, probe.calls, tc.wantCalls, err)
				}
			})
		})
	}
}
