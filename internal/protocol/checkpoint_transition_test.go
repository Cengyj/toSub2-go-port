package protocol

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"tosub2-go-port/internal/transport"
)

func TestLoginCheckpointResumesAfterConsumedPasswordOrOTPAndMFA(t *testing.T) {
	for _, method := range []string{"password", "email_otp"} {
		for _, failAt := range []string{"workspace", "callback"} {
			t.Run(method+"/"+failAt, func(t *testing.T) {
				var loginCalls, mfaCalls, issueCalls, workspaceCalls, callbacks int
				interrupted := false
				d := doerFunc(func(_ context.Context, r transport.Request) (*transport.Response, error) {
					switch {
					case strings.HasSuffix(r.URL, "/password/verify"), strings.HasSuffix(r.URL, "/email-otp/validate"):
						loginCalls++
						return response(200, "{\"continue_url\":\"https://auth.example/mfa-challenge/factor\",\"page\":{\"type\":\"mfa_challenge\"},\"oai-client-auth-session\":{\"mfa_factors\":[{\"factor_type\":\"totp\",\"id\":\"factor\"}]}}"), nil
					case strings.HasSuffix(r.URL, "/mfa/issue_challenge"):
						issueCalls++
						return response(200, "{}"), nil
					case strings.HasSuffix(r.URL, "/mfa/verify"):
						mfaCalls++
						return response(200, "{\"continue_url\":\"https://auth.example/workspace\",\"page\":{\"type\":\"workspace\"},\"oai-client-auth-session\":{\"workspaces\":[{\"id\":\"org\",\"kind\":\"organization\"}]}}"), nil
					case strings.HasSuffix(r.URL, "/workspace/select"):
						workspaceCalls++
						if failAt == "workspace" && !interrupted {
							interrupted = true
							return nil, context.Canceled
						}
						return response(200, "{\"continue_url\":\"https://chat.example/complete\"}"), nil
					case strings.HasSuffix(r.URL, "/complete"):
						callbacks++
						if failAt == "callback" && !interrupted {
							interrupted = true
							return nil, context.Canceled
						}
						return response(200, "ok"), nil
					default:
						t.Fatalf("unexpected request %s", r.URL)
						return nil, errors.New("unexpected request")
					}
				})
				opt := FlowOptions{ChatGPTBase: "https://chat.example", AuthBase: "https://auth.example", Email: "fixture@example.test", Password: "Fixture-Password9!", TOTPSecret: "JBSWY3DPEHPK3PXP", Sentinel: fakeSentinel, Input: func(context.Context, string, string) (string, error) { return "123456", nil }}
				page := "https://auth.example/log-in/password"
				stage := "web_password"
				if method == "email_otp" {
					page, stage = "https://auth.example/email-verification", "web_email_otp"
				}
				f := NewFlow(NewClient(d, nil, ""), opt)
				cp := Checkpoint{Version: 1, Stage: stage, Email: opt.Email, Cookies: []transport.Cookie{}, Web: map[string]any{"deviceId": "fixture-device"}, OAuth: map[string]any{"pageURL": page, "loginMethod": method}, Extra: map[string]any{"future_extension": true}}
				if err := f.Restore(cp); err != nil {
					t.Fatal(err)
				}
				if _, err := f.LoginWeb(context.Background()); !errors.Is(err, context.Canceled) {
					t.Fatalf("interruption missing: %v", err)
				}
				if f.checkpoint.Stage != "web_continue" {
					t.Fatalf("stage did not advance after consumed authentication: %s", f.checkpoint.Stage)
				}
				raw, err := json.Marshal(f.checkpoint)
				if err != nil {
					t.Fatal(err)
				}
				var restored Checkpoint
				if err = json.Unmarshal(raw, &restored); err != nil {
					t.Fatal(err)
				}
				resumed := NewFlow(NewClient(d, nil, ""), opt)
				if err = resumed.Restore(restored); err != nil {
					t.Fatal(err)
				}
				web, err := resumed.LoginWeb(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				if loginCalls != 1 || issueCalls != 1 || mfaCalls != 1 {
					t.Fatalf("consumed password/OTP/MFA was replayed: login=%d issue=%d verify=%d", loginCalls, issueCalls, mfaCalls)
				}
				wantWorkspace, wantCallbacks := 2, 1
				if failAt == "callback" {
					wantWorkspace, wantCallbacks = 1, 2
				}
				if workspaceCalls != wantWorkspace || callbacks != wantCallbacks {
					t.Fatalf("workspace=%d callbacks=%d", workspaceCalls, callbacks)
				}
				if web["mfaVerified"] != true || web["loginMethod"] != method || resumed.checkpoint.Extra["future_extension"] != true || resumed.checkpoint.Stage != "email_verified" {
					t.Fatal("restored metadata or final stage was lost")
				}
			})
		}
	}
}

func TestOAuthCallbackRequiresStateAndRedactsUntrustedErrors(t *testing.T) {
	for _, raw := range []string{"http://localhost:1455/auth/callback?code=c", "http://localhost:1455/auth/callback?code=c&state=s"} {
		if _, err := ParseOAuthCallback(raw, ""); err == nil {
			t.Fatal("callback without expected state accepted")
		}
	}
	for _, code := range []string{"access_denied", "secret-in-error-field"} {
		_, err := ParseOAuthCallback("http://localhost:1455/auth/callback?state=s&error="+code+"&error_description=plain-sensitive-token-without-label", "s")
		if err == nil || strings.Contains(err.Error(), "plain-sensitive") || strings.Contains(err.Error(), "secret-in-error") {
			t.Fatalf("unsafe callback error %v", err)
		}
	}
}
