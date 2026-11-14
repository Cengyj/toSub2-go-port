package protocol

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"tosub2-go-port/internal/transport"
)

func TestUnconfirmedAuthenticationCannotCommitCredentials(t *testing.T) {
	for _, step := range []string{"password", "totp"} {
		for _, body := range []string{
			`{}`, `null`, `{"success":true}`,
			`{"success":false,"continue_url":"/workspace"}`,
			`{"error":{"code":"invalid_otp"},"continue_url":"/workspace"}`,
			`{"continue_url":"/workspace","continue_url":"/other"}`,
			`{"continue_url":"javascript:alert(1)"}`,
			`{"continue_url":"https://untrusted.example/workspace"}`,
			`{"continue_url":"//untrusted.example/workspace","page":{"type":"workspace"}}`,
			`{"continue_url":"https://private@auth.openai.com/workspace"}`,
			`{"continue_url":"/log-in/password?retry=true"}`,
			`{"continue_url":"/log-in/password/","page":{"type":"workspace"}}`,
			`{"continue_url":"https://chatgpt.com/auth/login"}`,
			`{"continue_url":"#workspace"}`,
			`{"continue_url":"%invalid"}`,
		} {
			t.Run(step+"/"+body, func(t *testing.T) {
				writes, saved, prompts := 0, 0, 0
				client := NewClient(doerFunc(func(_ context.Context, request transport.Request) (*transport.Response, error) {
					if strings.HasSuffix(request.URL, "/issue_challenge") {
						return response(200, `{}`), nil
					}
					writes++
					return response(200, body), nil
				}), nil, "")
				flow := NewFlow(client, FlowOptions{Email: "owner@example.test", Password: "Fixture-Password9!", TOTPSecret: "JBSWY3DPEHPK3PXP",
					Input: func(context.Context, string, string) (string, error) { prompts++; return "123456", nil },
					Sentinel: func(context.Context, string, string, string) (http.Header, error) {
						return http.Header{"Openai-Sentinel-Token": []string{"fixture-sentinel"}}, nil
					},
					PasswordVerified: func(context.Context, string) error { saved++; return nil },
					TOTPVerified:     func(context.Context, string) error { saved++; return nil },
				})
				var err error
				if step == "password" {
					_, err = flow.verifyPassword(t.Context(), flow.Options.AuthBase+"/log-in/password")
				} else {
					_, err = flow.verifyMFA(t.Context(), authPayload{"oai-client-auth-session": map[string]any{"mfa_factors": []any{map[string]any{"factor_type": "totp", "id": "factor"}}}}, flow.Options.AuthBase+"/mfa-challenge/factor")
				}
				if err == nil || saved != 0 || writes != 1 || prompts != 0 || flow.verifiedPassword != "" {
					t.Fatalf("unconfirmed response saved credentials or retried: saved=%d writes=%d prompts=%d err=%v", saved, writes, prompts, err)
				}
			})
		}
	}
}

func TestAmbiguousRejectionDoesNotResubmitAuthentication(t *testing.T) {
	for _, step := range []string{"password", "totp", "email"} {
		for _, body := range []string{
			`{"error":"upstream_unknown","error":"invalid_code","message":"invalid password"}`,
			`{"error":"invalid_code","message":"invalid password"`,
		} {
			t.Run(step+"/"+body, func(t *testing.T) {
				writes, prompts, rejectedEvents := 0, 0, 0
				client := NewClient(doerFunc(func(_ context.Context, request transport.Request) (*transport.Response, error) {
					if strings.HasSuffix(request.URL, "/issue_challenge") {
						return response(200, `{}`), nil
					}
					writes++
					return response(400, body), nil
				}), nil, "")
				flow := NewFlow(client, FlowOptions{Email: "owner@example.test", Password: "Fixture-Password9!", TOTPSecret: "JBSWY3DPEHPK3PXP",
					Input: func(context.Context, string, string) (string, error) {
						prompts++
						if step == "email" && prompts == 1 {
							return "123456", nil
						}
						return "", errors.New("unexpected second input")
					},
					Sentinel: func(context.Context, string, string, string) (http.Header, error) {
						return http.Header{"Openai-Sentinel-Token": []string{"fixture-sentinel"}}, nil
					},
					Event: func(stage, _, _ string) {
						if strings.HasSuffix(stage, "_rejected") {
							rejectedEvents++
						}
					},
				})
				var err error
				wantPrompts := 0
				switch step {
				case "password":
					_, err = flow.verifyPassword(t.Context(), flow.Options.AuthBase+"/log-in/password")
				case "totp":
					_, err = flow.verifyMFA(t.Context(), authPayload{"oai-client-auth-session": map[string]any{"mfa_factors": []any{map[string]any{"factor_type": "totp", "id": "factor"}}}}, flow.Options.AuthBase+"/mfa-challenge/factor")
				case "email":
					wantPrompts = 1
					_, err = flow.verifyEmail(t.Context(), flow.Options.AuthBase+"/email-verification")
				}
				if err == nil || writes != 1 || prompts != wantPrompts || rejectedEvents != 0 {
					t.Fatalf("ambiguous response became a credential rejection: writes=%d prompts=%d rejected=%d err=%v", writes, prompts, rejectedEvents, err)
				}
			})
		}
	}
}

func TestAuthenticationProgressKeepsBoundContinuationContracts(t *testing.T) {
	for _, step := range []string{"password", "totp"} {
		for _, tc := range []struct {
			name, body string
			want       bool
		}{
			{"relative-next", `{"continue_url":"/fixture-next"}`, true},
			{"unknown-provider-path", `{"continue_url":"https://auth.example.test/next/from-provider?opaque=value"}`, true},
			{"default-port", `{"continue_url":"https://auth.example.test:443/workspace"}`, true},
			{"chat-callback", `{"page":{"type":"external_url","payload":{"url":"https://chat.example.test/api/auth/callback/openai?code=fixture"}}}`, true},
			{"page-only", `{"page":{"type":"workspace"}}`, true},
			{"email-page-only", `{"page":{"type":"email_verification"}}`, true},
			{"mfa-next-or-same-page", `{"continue_url":"https://auth.example.test/mfa-challenge/factor?retry=true"}`, step == "password"},
			{"mfa-next-or-same-page-only", `{"page":{"type":"mfa_challenge"}}`, step == "password"},
			{"bound-oauth", `{"page":{"type":"external_url","payload":{"url":"` + DefaultCodexRedirectURI + `?code=issued&state=current-state"}}}`, true},
			{"oauth-wrong-state", `{"continue_url":"` + DefaultCodexRedirectURI + `?code=issued&state=old-state"}`, false},
			{"oauth-missing-code", `{"continue_url":"` + DefaultCodexRedirectURI + `?state=current-state"}`, false},
			{"oauth-error", `{"continue_url":"` + DefaultCodexRedirectURI + `?error=access_denied&state=current-state"}`, false},
		} {
			t.Run(step+"/"+tc.name, func(t *testing.T) {
				writes, saved, prompts, reads := 0, 0, 0, 0
				client := NewClient(doerFunc(func(_ context.Context, request transport.Request) (*transport.Response, error) {
					if request.Method != http.MethodPost {
						reads++
					}
					if strings.HasSuffix(request.URL, "/issue_challenge") {
						return response(200, `{}`), nil
					}
					writes++
					return response(200, tc.body), nil
				}), nil, "")
				flow := NewFlow(client, FlowOptions{AuthBase: "https://auth.example.test", ChatGPTBase: "https://chat.example.test",
					Password: "Fixture-Password9!", TOTPSecret: "JBSWY3DPEHPK3PXP", Sentinel: fakeSentinel,
					Input:            func(context.Context, string, string) (string, error) { prompts++; return "123456", nil },
					PasswordVerified: func(context.Context, string) error { saved++; return nil },
					TOTPVerified:     func(context.Context, string) error { saved++; return nil },
				})
				flow.recordProgress("fixture-current-operation", map[string]any{"state": "current-state"})
				var err error
				if step == "password" {
					_, err = flow.verifyPassword(t.Context(), flow.Options.AuthBase+"/log-in/password")
				} else {
					_, err = flow.verifyMFA(t.Context(), authPayload{"oai-client-auth-session": map[string]any{"mfa_factors": []any{map[string]any{"factor_type": "totp", "id": "factor"}}}}, flow.Options.AuthBase+"/mfa-challenge/factor")
				}
				wantSaved := 0
				if tc.want {
					wantSaved = 1
				}
				if (err == nil) != tc.want || saved != wantSaved || writes != 1 || prompts != 0 || reads != 0 {
					t.Fatalf("progress contract changed: saved=%d writes=%d prompts=%d reads=%d err=%v", saved, writes, prompts, reads, err)
				}
			})
		}
	}
}
