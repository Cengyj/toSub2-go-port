package protocol

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"tosub2-go-port/internal/transport"
)

func TestMFAForbiddenIncorrectCodeKeepsCheckpointForManualCorrection(t *testing.T) {
	issueCalls, verifyCalls, manualCalls, savedSeeds, rejectedEvents := 0, 0, 0, 0, 0
	d := doerFunc(func(_ context.Context, r transport.Request) (*transport.Response, error) {
		switch {
		case strings.HasSuffix(r.URL, "/mfa/issue_challenge"):
			issueCalls++
			return response(http.StatusOK, `{}`), nil
		case strings.HasSuffix(r.URL, "/mfa/verify"):
			verifyCalls++
			if verifyCalls == 1 {
				return response(http.StatusForbidden, `{"error":{"code":"incorrect_code","message":"fixture-private-response-echo"}}`), nil
			}
			var submitted map[string]any
			if err := json.Unmarshal(r.Body, &submitted); err != nil || submitted["code"] != "123456" {
				t.Fatal("corrected manual code was not submitted")
			}
			return response(http.StatusOK, `{"continue_url":"https://chat.example/complete"}`), nil
		case r.URL == "https://chat.example/complete":
			return response(http.StatusOK, "ok"), nil
		default:
			t.Fatal("resuming MFA replayed a consumed login step")
			return nil, errors.New("unexpected request")
		}
	})
	f := NewFlow(NewClient(d, nil, ""), FlowOptions{
		ChatGPTBase: "https://chat.example", AuthBase: "https://auth.example", Email: "fixture@example.test",
		TOTPSecret: "JBSWY3DPEHPK3PXP", Sentinel: fakeSentinel,
		TOTPVerified: func(context.Context, string) error { savedSeeds++; return nil },
		Event: func(stage, _, message string) {
			if stage == "mfa_code_rejected" {
				rejectedEvents++
				if !strings.Contains(message, "configured 2FA key") || strings.Contains(message, "fixture-private") {
					t.Fatal("missing safe explanation for rejected automatic code")
				}
			}
		},
	})
	f.Options.Input = func(_ context.Context, stage, prompt string) (string, error) {
		manualCalls++
		if manualCalls > 1 || stage != "totp" || !strings.Contains(prompt, "手动输入") {
			t.Fatal("did not ask for a corrected authenticator code")
		}
		if f.checkpoint.Stage != "web_mfa" || f.checkpoint.Web["mfaVerified"] == true {
			t.Fatal("rejected code advanced or discarded MFA checkpoint")
		}
		return "123456", nil
	}
	challenge := authPayload{"continue_url": "https://auth.example/mfa-challenge/factor", "page": map[string]any{"type": "mfa_challenge"}, "oai-client-auth-session": map[string]any{"mfa_factors": []any{map[string]any{"factor_type": "totp", "id": "factor"}}}}
	cp := Checkpoint{Version: 1, Stage: "web_mfa", Email: f.Options.Email, Cookies: []transport.Cookie{}, Web: map[string]any{"deviceId": "fixture-device"}, OAuth: map[string]any{"pageURL": "https://auth.example/log-in/password", "loginMethod": "password", "payload": challenge}, Extra: map[string]any{"future_extension": true}}
	if err := f.Restore(cp); err != nil {
		t.Fatal(err)
	}
	web, err := f.LoginWeb(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if issueCalls != 1 || verifyCalls != 2 || manualCalls != 1 || rejectedEvents != 1 || savedSeeds != 0 {
		t.Fatalf("issue=%d verify=%d manual=%d rejected=%d savedSeeds=%d", issueCalls, verifyCalls, manualCalls, rejectedEvents, savedSeeds)
	}
	if f.checkpoint.Stage != "email_verified" || f.checkpoint.Extra["future_extension"] != true || web["mfaVerified"] != true || web["loginMethod"] != "password" {
		t.Fatal("accepted correction did not complete the preserved login")
	}
}

func TestEmailForbiddenIncorrectCodeAllowsExplicitResend(t *testing.T) {
	verifyCalls, resendCalls, snapshots, rejectedEvents := 0, 0, 0, 0
	var requestedAt time.Time
	d := doerFunc(func(_ context.Context, r transport.Request) (*transport.Response, error) {
		switch {
		case strings.HasSuffix(r.URL, "/email-otp/validate"):
			verifyCalls++
			if verifyCalls == 1 {
				return response(http.StatusForbidden, `{"error":{"code":"incorrect_code"}}`), nil
			}
			return response(http.StatusOK, `{"continue_url":"https://chat.example/complete"}`), nil
		case strings.HasSuffix(r.URL, "/email-otp/resend"):
			resendCalls++
			if snapshots != 1 {
				t.Fatal("mailbox was not snapshotted before resend")
			}
			return response(http.StatusOK, `{}`), nil
		case r.URL == "https://chat.example/complete":
			return response(http.StatusOK, "ok"), nil
		default:
			t.Fatal("resuming email verification restarted login")
			return nil, errors.New("unexpected request")
		}
	})
	inputs := []string{"000000", "r", "123456"}
	f := NewFlow(NewClient(d, nil, ""), FlowOptions{ChatGPTBase: "https://chat.example", AuthBase: "https://auth.example", Email: "fixture@example.test", Sentinel: fakeSentinel,
		Input: func(_ context.Context, stage, _ string) (string, error) {
			if stage != "email_otp" || len(inputs) == 0 {
				t.Fatal("unexpected input")
			}
			value := inputs[0]
			inputs = inputs[1:]
			return value, nil
		},
		BeforeEmailOTP:    func(context.Context) error { snapshots++; return nil },
		EmailOTPRequested: func(at time.Time) { requestedAt = at },
		Event: func(stage, _, _ string) {
			if stage == "email_otp_rejected" {
				rejectedEvents++
			}
		},
	})
	cp := Checkpoint{Version: 1, Stage: "web_email_otp", Email: f.Options.Email, Cookies: []transport.Cookie{}, Web: map[string]any{"deviceId": "fixture-device"}, OAuth: map[string]any{"pageURL": "https://auth.example/email-verification", "loginMethod": "email_otp"}}
	if err := f.Restore(cp); err != nil {
		t.Fatal(err)
	}
	if _, err := f.LoginWeb(context.Background()); err != nil {
		t.Fatal(err)
	}
	if verifyCalls != 2 || resendCalls != 1 || snapshots != 1 || rejectedEvents != 1 || requestedAt.IsZero() || len(inputs) != 0 {
		t.Fatalf("verify=%d resend=%d snapshots=%d rejected=%d", verifyCalls, resendCalls, snapshots, rejectedEvents)
	}
}

func TestMFAForbiddenPermissionChallengeAndRateLimitRemainTerminal(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"permission", 403, `{"error":{"code":"access_denied","message":"incorrect code"}}`},
		{"unclassified-forbidden", 403, `{}`},
		{"challenge-document", 403, `<html>incorrect_code</html>`},
		{"session-expired", 403, `{"error":{"code":"session_expired"}}`},
		{"rate-limit", 429, `{"error":{"code":"incorrect_code"}}`},
		{"server-failure", 503, `{"error":{"code":"incorrect_code"}}`},
		{"different-credential", 403, `{"error":{"code":"invalid_password"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			verifyCalls, inputCalls := 0, 0
			d := doerFunc(func(_ context.Context, r transport.Request) (*transport.Response, error) {
				if strings.HasSuffix(r.URL, "/mfa/issue_challenge") {
					return response(http.StatusOK, `{}`), nil
				}
				verifyCalls++
				return response(tc.status, tc.body), nil
			})
			f := NewFlow(NewClient(d, nil, ""), FlowOptions{AuthBase: "https://auth.example", TOTPSecret: "JBSWY3DPEHPK3PXP", Sentinel: fakeSentinel,
				Input: func(context.Context, string, string) (string, error) {
					inputCalls++
					return "", errors.New("unexpected input")
				},
			})
			p := authPayload{"oai-client-auth-session": map[string]any{"mfa_factors": []any{map[string]any{"factor_type": "totp", "id": "factor"}}}}
			_, err := f.verifyMFA(context.Background(), p, "https://auth.example/mfa-challenge/factor")
			if err == nil || verifyCalls != 1 || inputCalls != 0 {
				t.Fatalf("terminal response entered manual recovery: verify=%d inputs=%d err=%v", verifyCalls, inputCalls, err)
			}
		})
	}
}
