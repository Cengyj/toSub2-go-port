package console

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"tosub2-go-port/internal/sub2api"
)

func TestMonitorCheckPreservesCallerDeadlineAcrossMultipleRemoteRequests(t *testing.T) {
	for _, parentDeadline := range []bool{false, true} {
		name := "no-overall-deadline"
		if parentDeadline {
			name = "caller-deadline"
		}
		t.Run(name, func(t *testing.T) {
			s, _ := newMonitorTestServer(t)
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"items": []any{}}})
			}))
			defer remote.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if parentDeadline {
				var deadlineCancel context.CancelFunc
				ctx, deadlineCancel = context.WithTimeout(ctx, 5*time.Minute)
				defer deadlineCancel()
			}
			wantDeadline, wantBounded := ctx.Deadline()
			called := false
			err := s.monitor.Configure(sub2api.New(sub2api.Config{BaseURL: remote.URL, APIKey: "synthetic-admin-key"}), sub2api.MonitorConfig{Enabled: true, BaseURL: remote.URL}, sub2api.MonitorHooks{
				RetryPending: func(checkCtx context.Context) (int, error) {
					called = true
					deadline, bounded := checkCtx.Deadline()
					if bounded != wantBounded || (bounded && !deadline.Equal(wantDeadline)) {
						t.Errorf("console shortened the legacy multi-request monitor budget: bounded=%v deadline=%v wantBounded=%v wantDeadline=%v", bounded, deadline, wantBounded, wantDeadline)
					}
					return 0, nil
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.monitorCheck(ctx, "manual"); err != nil || !called {
				t.Fatal("monitor did not reach pending recovery", err)
			}
		})
	}
}

func TestMonitorCheckStillCancelsWithCaller(t *testing.T) {
	s, _ := newMonitorTestServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	called := false
	err := s.monitor.Configure(sub2api.New(sub2api.Config{BaseURL: "http://127.0.0.1:1", APIKey: "synthetic-admin-key"}), sub2api.MonitorConfig{Enabled: true, BaseURL: "http://127.0.0.1:1"}, sub2api.MonitorHooks{
		RetryPending: func(checkCtx context.Context) (int, error) {
			called = true
			cancel()
			select {
			case <-checkCtx.Done():
				return 0, checkCtx.Err()
			case <-time.After(time.Second):
				t.Error("monitor lost caller cancellation")
				return 0, errors.New("caller cancellation was lost")
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.monitorCheck(ctx, "manual"); !errors.Is(err, context.Canceled) || !called {
		t.Fatal("monitor cancellation did not propagate", err)
	}
}
