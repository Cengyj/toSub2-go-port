package protocol

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"tosub2-go-port/internal/transport"
)

func TestTOTPVerifiedOnlyCommitsAutomaticallyVerifiedSeed(t *testing.T) {
	storageErr := errors.New("fixture secure storage unavailable")
	for _, test := range []struct {
		name                                      string
		noSecret, rejectAutomatic, storageFailure bool
		wantHook, wantManual                      int
	}{
		{name: "automatic-success", wantHook: 1},
		{name: "automatic-rejected-manual-success", rejectAutomatic: true, wantManual: 1},
		{name: "manual-success", noSecret: true, wantManual: 1},
		{name: "secure-storage-failure", storageFailure: true, wantHook: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			verificationCalls, hookCalls, manualCalls := 0, 0, 0
			secret := "JBSWY3DPEHPK3PXP"
			if test.noSecret {
				secret = ""
			}
			d := doerFunc(func(ctx context.Context, r transport.Request) (*transport.Response, error) {
				if strings.HasSuffix(r.URL, "/issue_challenge") {
					return response(200, `{}`), nil
				}
				if !strings.HasSuffix(r.URL, "/mfa/verify") {
					t.Fatalf("unexpected request path")
					return nil, nil
				}
				verificationCalls++
				if test.rejectAutomatic && verificationCalls == 1 {
					return response(400, `{"error":{"code":"invalid_totp"}}`), nil
				}
				if test.noSecret || test.rejectAutomatic {
					var body map[string]any
					json.Unmarshal(r.Body, &body)
					if body["code"] != "123456" {
						t.Error("manual OTP was not submitted")
					}
				}
				return response(200, `{"continue_url":"https://chat.example/"}`), nil
			})
			f := NewFlow(NewClient(d, nil, "auto"), FlowOptions{Email: "owner@example.test", AuthBase: "https://auth.example", TOTPSecret: secret, Input: func(context.Context, string, string) (string, error) { manualCalls++; return "123456", nil }, Sentinel: func(context.Context, string, string, string) (http.Header, error) {
				return http.Header{"Openai-Sentinel-Token": []string{"synthetic-sentinel"}}, nil
			}, TOTPVerified: func(ctx context.Context, verified string) error {
				hookCalls++
				if verified != secret || verified == "" {
					t.Error("verified callback received an unrelated seed")
				}
				if test.storageFailure {
					return storageErr
				}
				return nil
			}})
			p := authPayload{"continue_url": "https://auth.example/mfa-challenge/totp", "oai-client-auth-session": map[string]any{"mfa_challenge_factors": []any{map[string]any{"factor_type": "totp", "id": "fixture-factor"}}}}
			_, err := f.verifyMFA(context.Background(), p, "https://auth.example/mfa-challenge/totp")
			if test.storageFailure {
				if !errors.Is(err, storageErr) {
					t.Fatalf("secure-store failure not propagated: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if hookCalls != test.wantHook || manualCalls != test.wantManual {
				t.Fatalf("verified hooks=%d manual prompts=%d", hookCalls, manualCalls)
			}
		})
	}
}
