package protocol

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"tosub2-go-port/internal/browser"
	"tosub2-go-port/internal/challenge"
	"tosub2-go-port/internal/transport"
)

// The fixture has no HTTP listener or external network. Each target write
// returns an existing typed boundary or locally generated safe diagnostic.
func TestPhoneTerminalBoundaryPreservesErrorsWithoutManualRecovery(t *testing.T) {
	client, proxyConfigErr := transport.NewFingerprintClient("auto", "http://%", nil)
	if client != nil {
		client.Close()
	}
	var typedProxy *transport.TransportError
	if !errors.As(proxyConfigErr, &typedProxy) {
		t.Fatal("fixture did not produce a typed, local proxy configuration error")
	}
	cases := []struct {
		name string
		err  error
	}{
		{"session-sentinel", ErrSessionExpired},
		{"session-expired", &HTTPError{StatusCode: 401, Code: "session_expired"}},
		{"session-invalid", &HTTPError{StatusCode: 403, Code: "session_invalid"}},
		{"invalid-session", &HTTPError{StatusCode: 422, Code: "invalid_session"}},
		{"login-required", &HTTPError{StatusCode: 503, Code: "login_required"}},
		{"invalid-auth-step", &HTTPError{StatusCode: 400, Code: "invalid_auth_step"}},
		{"invalid-authorization-step", &HTTPError{StatusCode: 429, Code: "invalid_authorization_step"}},
		{"expired-text", &HTTPError{StatusCode: 503, sessionExpiredText: true}},
		{"browser-challenge", ErrBrowserChallenge},
		{"verification-pending", challenge.ErrVerificationPending},
		{"verification-pending-request-timeout", errors.Join(challenge.ErrVerificationPending, context.DeadlineExceeded)},
		{"verification-rejected", challenge.ErrVerificationRejected},
		{"browser-closed", browser.ErrClosed},
		{"browser-hidden-target", browser.ErrHiddenTargetUnsupported},
		{"browser-interaction-required", browser.ErrInteractiveBrowserRequired},
		{"browser-identity", errors.New("BROWSER_IDENTITY_MISMATCH: request User-Agent differs from Chromium")},
		{"browser-device", errors.New("BROWSER_DEVICE_MISMATCH: checkpoint cookie and browser device differ")},
		{"browser-partition", errors.New("BROWSER_COOKIE_PARTITION_REQUIRED: checkpoint contains a cookie without a restorable top-level site; start a fresh browser login")},
		{"proxy-config-typed", proxyConfigErr},
		{"proxy-auth-http", &HTTPError{StatusCode: http.StatusProxyAuthRequired}},
		{"proxy-risk-marker", errors.New("PROXY_RISK_CONTROL: fixture proxy session requires explicit repair")},
		{"proxy-retry-marker", errors.New("PROXY_CONNECTION_RETRY: connectivity attempts exhausted")},
		{"proxy-risk-joined-timeout", errors.Join(context.DeadlineExceeded, errors.New("PROXY_RISK_CONTROL: fixture proxy session requires explicit repair"))},
		{"tls-joined-timeout", errors.Join(context.DeadlineExceeded, errors.New("TLS certificate validation failed"))},
		{"browser-joined-timeout", errors.Join(context.DeadlineExceeded, errors.New("BROWSER_IDENTITY_MISMATCH: request User-Agent differs from Chromium"))},
		{"browser-joined-code-rejection", errors.Join(&HTTPError{StatusCode: 400, Code: "invalid_code"}, errors.New("BROWSER_DEVICE_MISMATCH: checkpoint cookie and browser device differ"))},
		{"profile-probe-typed", &transport.ProfileProbeError{}},
		{"profile-probe-request-timeout", errors.Join(&transport.ProfileProbeError{}, context.DeadlineExceeded)},
	}
	for _, item := range []struct{ name, message string }{
		{"proxy-configuration", "invalid or unsupported proxy configuration"},
		{"proxy-authentication", "proxy authentication rejected (HTTP 407)"},
		{"proxy-connect-refused", "proxy connection refused"},
		{"proxy-dns", "proxy DNS lookup failed"},
		{"proxy-unreachable", "proxy network is unreachable"},
		{"proxy-timeout", "proxy connection timed out"},
		{"proxy-connect-failed", "proxy connection failed"},
		{"tls-profile-unavailable", "requested TLS profile is not bundled"},
		{"tls-initialization", "TLS transport initialization failed"},
		{"tls-certificate", "TLS certificate validation failed"},
		{"proxy-certificate", "HTTPS proxy certificate validation failed"},
		{"tls-handshake", "TLS handshake or protocol failed"},
		{"proxy-tls-handshake", "HTTPS proxy TLS handshake failed"},
	} {
		cases = append(cases, struct {
			name string
			err  error
		}{item.name, errors.New(item.message)})
	}
	for _, operation := range []string{"send", "validate", "resend"} {
		for _, tc := range cases {
			t.Run(operation+"/"+tc.name, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				// Preserve the original chain through a contextual wrapper. None of
				// its terminal identities may be replaced by a generic phone error.
				injected := fmt.Errorf("phone boundary fixture: %w", tc.err)
				runPhoneTerminalBoundary(t, ctx, operation, func() error { return injected }, injected)
			})
		}
	}
}

func TestPhoneTerminalBoundaryTaskCancellationDuringWrite(t *testing.T) {
	for _, operation := range []string{"send", "validate", "resend"} {
		for _, cause := range []string{"cancel", "deadline"} {
			t.Run(operation+"/"+cause, func(t *testing.T) {
				var ctx context.Context
				var cancel context.CancelFunc
				want := context.Canceled
				if cause == "deadline" {
					ctx, cancel = context.WithTimeout(context.Background(), 100*time.Millisecond)
					want = context.DeadlineExceeded
				} else {
					ctx, cancel = context.WithCancel(context.Background())
				}
				defer cancel()
				runPhoneTerminalBoundary(t, ctx, operation, func() error {
					if cause == "cancel" {
						cancel()
					}
					<-ctx.Done()
					return ctx.Err()
				}, want)
			})
		}
	}
}

func runPhoneTerminalBoundary(t *testing.T, ctx context.Context, operation string, failure func() error, want error) {
	t.Helper()
	const base = "https://phone-boundary.example.test"
	var sends, validations, failedWrites, prompts, deferredEvents, pausedCheckpoints, challengeEvents int
	client := NewClient(doerFunc(func(_ context.Context, request transport.Request) (*transport.Response, error) {
		u, err := url.Parse(request.URL)
		if err != nil {
			return nil, err
		}
		body := "{}"
		switch u.Path {
		case "/add-phone":
			if request.Method != http.MethodGet {
				t.Fatal("fixture received an unexpected phone navigation method")
			}
		case "/api/accounts/add-phone/send":
			sends++
			if operation == "send" || operation == "resend" && sends == 2 {
				failedWrites++
				return nil, failure()
			}
			body = "{\"continue_url\":\"" + base + "/phone-verification\"}"
		case "/api/accounts/phone-otp/validate":
			validations++
			failedWrites++
			return nil, failure()
		default:
			t.Fatalf("fixture received unexpected protocol path %s", u.Path)
		}
		return &transport.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: []byte(body), URL: u}, nil
	}), nil, "auto")
	client.ConnectionRetries = 3
	flow := NewFlow(client, FlowOptions{
		AuthBase: base, Phone: "+12025550123", Email: "fixture@example.test",
		Input: func(_ context.Context, stage, _ string) (string, error) {
			prompts++
			if prompts > 1 || stage != "sms_otp" || operation == "send" {
				return "", errors.New("terminal phone error incorrectly requested manual recovery")
			}
			if operation == "resend" {
				return "r", nil
			}
			return "654321", nil
		},
		EventData: func(stage, _, _ string, data map[string]any) {
			if strings.HasSuffix(stage, "_deferred") || data["phoneRetryRequired"] == true {
				deferredEvents++
			}
			if data["securityCheckRequired"] == true {
				challengeEvents++
			}
		},
		SaveCheckpoint: func(checkpoint Checkpoint) error {
			if checkpoint.OAuth["phoneRetryRequired"] == true {
				pausedCheckpoints++
			}
			return nil
		},
	})
	_, got := flow.bindPhone(ctx, map[string]any{"addPhoneUrl": base + "/add-phone"}, false)
	if !errors.Is(got, want) {
		t.Fatalf("terminal phone error identity was lost: got=%v want=%v", got, want)
	}
	var wantHTTP, gotHTTP *HTTPError
	if errors.As(want, &wantHTTP) && (!errors.As(got, &gotHTTP) || gotHTTP != wantHTTP) {
		t.Fatal("terminal provider status/type was replaced")
	}
	wantSends, wantValidations, wantPrompts := 1, 0, 0
	if operation == "validate" {
		wantValidations, wantPrompts = 1, 1
	} else if operation == "resend" {
		wantSends, wantPrompts = 2, 1
	}
	if sends != wantSends || validations != wantValidations || failedWrites != 1 || prompts != wantPrompts || deferredEvents != 0 || pausedCheckpoints != 0 {
		t.Fatalf("terminal boundary replayed or paused: sends=%d validations=%d failures=%d prompts=%d deferred=%d pausedCheckpoints=%d", sends, validations, failedWrites, prompts, deferredEvents, pausedCheckpoints)
	}
	wantChallengeEvents := 0
	if errors.Is(want, ErrBrowserChallenge) {
		wantChallengeEvents = 1
	}
	if challengeEvents != wantChallengeEvents {
		t.Fatal("terminal classification changed the existing browser challenge flag")
	}
}

func TestPhoneTerminalBoundaryAlreadyEndedTaskDoesNoWork(t *testing.T) {
	for _, cause := range []string{"cancel", "deadline"} {
		t.Run(cause, func(t *testing.T) {
			var ctx context.Context
			var cancel context.CancelFunc
			want := context.Canceled
			if cause == "deadline" {
				ctx, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
				want = context.DeadlineExceeded
			} else {
				ctx, cancel = context.WithCancel(context.Background())
				cancel()
			}
			defer cancel()
			calls := 0
			flow := NewFlow(NewClient(doerFunc(func(context.Context, transport.Request) (*transport.Response, error) {
				calls++
				return nil, errors.New("ended task reached a transport")
			}), nil, "auto"), FlowOptions{AuthBase: "https://phone-boundary.example.test", Phone: "+12025550123"})
			_, err := flow.bindPhone(ctx, map[string]any{"addPhoneUrl": "https://phone-boundary.example.test/add-phone"}, false)
			if !errors.Is(err, want) || calls != 0 {
				t.Fatal("already ended task started phone work or lost its context error")
			}
		})
	}
}
