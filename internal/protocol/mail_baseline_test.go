package protocol

import (
	"context"
	"errors"
	"net/url"
	"os"
	"testing"
	"time"

	"tosub2-go-port/internal/transport"
)

func TestMailBaselinePrecedesSendingAndResendButNotPendingResume(t *testing.T) {
	for _, resume := range []bool{false, true} {
		t.Run(map[bool]string{false: "fresh", true: "resume"}[resume], func(t *testing.T) {
			h := newRecoveryHarness(t)
			if resume {
				h.checkpoint(t, "web_email_otp", map[string]any{"pageURL": "https://auth.example/email-verification", "loginMethod": "email_otp"})
			}
			baselines, requested, inputs, resends := 0, 0, 0, 0
			h.flow.Options.BeforeEmailOTP = func(context.Context) error { baselines++; return nil }
			h.flow.Options.EmailOTPRequested = func(time.Time) { requested++ }
			h.flow.Options.Input = func(context.Context, string, string) (string, error) {
				inputs++
				want := 1
				if resume {
					want = 0
				}
				if inputs == 1 {
					if baselines != want {
						t.Error("pending OTP was snapshotted after it had already been sent")
					}
					return "r", nil
				}
				return "123456", nil
			}
			h.intercept = func(_ context.Context, r transport.Request) (*transport.Response, error, bool) {
				u, _ := url.Parse(r.URL)
				if u.Path == "/api/auth/signin/openai" && baselines != 1 {
					t.Error("initial OTP was sent before mailbox baseline")
				}
				if u.Path == "/api/accounts/email-otp/resend" {
					resends++
					want := 2
					if resume {
						want = 1
					}
					if baselines != want {
						t.Error("resend was sent before mailbox baseline")
					}
					return fixtureJSON(map[string]any{}), nil, true
				}
				return nil, nil, false
			}
			if _, err := h.flow.Run(context.Background()); err != nil {
				t.Fatal(err)
			}
			wantRequested := 2
			if resume {
				wantRequested = 1
			}
			if inputs != 2 || resends != 1 || requested != wantRequested {
				t.Fatalf("inputs=%d resends=%d requested=%d", inputs, resends, requested)
			}
		})
	}
}

func TestMailBaselineCancellationStopsSend(t *testing.T) {
	h := newRecoveryHarness(t)
	h.flow.Options.BeforeEmailOTP = func(context.Context) error { return context.Canceled }
	h.intercept = func(_ context.Context, r transport.Request) (*transport.Response, error, bool) {
		u, _ := url.Parse(r.URL)
		if u.Path == "/api/auth/signin/openai" {
			t.Error("canceled baseline still sent an OTP")
		}
		return nil, nil, false
	}
	if _, err := h.flow.Run(context.Background()); !errors.Is(err, context.Canceled) {
		t.Fatal("baseline cancellation lost", err)
	}
}

func TestWebOnlyNeverReturnsRestoredCookiesAsAuthenticated(t *testing.T) {
	for _, stage := range []string{"email_verified", "oauth_continue", "web_email_otp"} {
		t.Run(stage, func(t *testing.T) {
			h := newRecoveryHarness(t)
			h.checkpoint(t, stage, map[string]any{"pageURL": "https://auth.example/email-verification"})
			h.flow.Options.WebOnly = true
			h.flow.Options.OutputMode = "session"
			h.flow.Options.SessionOutputPath = t.TempDir() + "/session.json"
			h.intercept = func(context.Context, transport.Request) (*transport.Response, error, bool) {
				return nil, context.Canceled, true
			}
			if _, err := h.flow.Run(context.Background()); !errors.Is(err, context.Canceled) {
				t.Fatal("restored web-only checkpoint reported success without fresh web login", err)
			}
			if _, err := os.Stat(h.flow.Options.SessionOutputPath); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("failed web authentication produced session export")
			}
		})
	}
}
