package protocol

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"

	"tosub2-go-port/internal/transport"
)

func initialStatusRetryFixture(t *testing.T, operation string) (*resetFixture, func(context.Context) (map[string]any, error)) {
	t.Helper()
	x := newResetFixture(t, false)
	x.flow.Options.Input = func(context.Context, string, string) (string, error) {
		t.Fatal("status retry requested authentication input")
		return "", nil
	}
	run := func(ctx context.Context) (map[string]any, error) { return x.flow.ResetTOTP(ctx, x.path) }
	return x, run
}

func TestTOTPInitialStatusReadRecoversTransientHTTPInOriginalSession(t *testing.T) {
	for _, operation := range []string{"reset"} {
		t.Run(operation, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				x, run := initialStatusRetryFixture(t, operation)
				reads, initialDone := 0, false
				x.override = func(step string, r transport.Request) (*transport.Response, error, bool) {
					if step == "disable" || step == "enroll" {
						initialDone = true
					}
					if !strings.HasPrefix(step, "info") || initialDone {
						return nil, nil, false
					}
					reads++
					if r.Method != http.MethodGet || r.Headers.Get("Cache-Control") != "no-cache, no-store" {
						t.Fatal("status retry did not use an uncached GET")
					}
					if reads == 1 {
						x.flow.Client.Jar.Set(transport.Cookie{Name: "status-retry-cookie", Value: "rotated", Domain: "chat.example", Path: "/", Secure: true})
						return response(500, `{}`), nil, true
					}
					return nil, nil, false
				}
				out, err := run(t.Context())
				if err != nil || reads != 2 {
					t.Fatalf("initial status retry failed: reads=%d err=%v", reads, err)
				}
				if out["credential_persisted"] != true || countResetSteps(x.calls, "enroll") != 1 || countResetSteps(x.calls, "activate") != 1 {
					t.Fatal("bounded retry did not finish exactly once")
				}
				wantDisable := 0
				if operation == "reset" {
					wantDisable = 1
				}
				if countResetSteps(x.calls, "disable") != wantDisable || countResetSteps(x.calls, "logout") != 0 {
					t.Fatal("status retry repeated a removal or dispatched logout")
				}
			})
		})
	}
}

func TestTOTPInitialStatusReadHTTPBudgetAndImmediateStops(t *testing.T) {
	for _, operation := range []string{"reset"} {
		for _, scenario := range []struct {
			name   string
			status int
			body   string
			reads  int
		}{
			{"persistent-500", 500, `{}`, totpStatusChecks},
			{"session-refused", 401, `{}`, 1},
			{"access-refused", 403, `{}`, 1},
			{"malformed-json", 200, `invalid`, 1},
		} {
			t.Run(operation+"/"+scenario.name, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					x, run := initialStatusRetryFixture(t, operation)
					reads := 0
					x.override = func(step string, _ transport.Request) (*transport.Response, error, bool) {
						if strings.HasPrefix(step, "info") {
							reads++
							return response(scenario.status, scenario.body), nil, true
						}
						return nil, nil, false
					}
					if _, err := run(t.Context()); err == nil || reads != scenario.reads {
						t.Fatalf("unexpected initial status budget: reads=%d err=%v", reads, err)
					}
					for _, step := range []string{"disable", "enroll", "activate", "logout"} {
						if countResetSteps(x.calls, step) != 0 {
							t.Fatalf("%s ran after status read failed", step)
						}
					}
				})
			})
		}
	}
}

func TestTOTPInitialStatusReadStopsBeforeRetryOnCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		x := newResetFixture(t, false)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		reads := 0
		x.override = func(step string, _ transport.Request) (*transport.Response, error, bool) {
			if !strings.HasPrefix(step, "info") {
				t.Fatal("status retry dispatched a non-status request")
			}
			reads++
			return response(500, "{}"), nil, true
		}
		x.flow.Options.Event = func(stage, _, _ string) {
			if stage == "totp_reset_status_pending" {
				cancel()
			}
		}
		_, err := x.flow.readTOTPResetStateWithRetry(ctx, resetFixtureToken, "current-account")
		if reads != 1 || !errors.Is(err, context.Canceled) {
			t.Fatalf("initial status read continued after cancellation: reads=%d err=%v", reads, err)
		}
	})
}
