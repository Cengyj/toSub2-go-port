package protocol

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"tosub2-go-port/internal/transport"
)

func loginTOTPFactor(id string) map[string]any {
	return map[string]any{"factor_type": "totp", "id": id}
}

func TestMFALoginUsesBoundSecondFactorAndPersistsBinding(t *testing.T) {
	const secret = "JBSWY3DPEHPK3PXP"
	var paths []string
	boundCalls, legacyCalls := 0, 0
	client := NewClient(doerFunc(func(_ context.Context, r transport.Request) (*transport.Response, error) {
		paths = append(paths, r.URL)
		var submitted map[string]any
		if err := json.Unmarshal(r.Body, &submitted); err != nil {
			t.Fatal(err)
		}
		if submitted["id"] != "second-factor" {
			t.Fatal("request selected the default factor instead of the saved binding")
		}
		if strings.HasSuffix(r.URL, "/issue_challenge") {
			return response(200, `{}`), nil
		}
		if !strings.HasSuffix(r.URL, "/mfa/verify") || r.Headers.Get("Referer") != "https://auth.example/mfa-challenge/second-factor" {
			t.Fatal("verification did not use the selected factor's challenge context")
		}
		return response(200, `{"continue_url":"https://chat.example/"}`), nil
	}), nil, "")
	flow := NewFlow(client, FlowOptions{AuthBase: "https://auth.example", ChatGPTBase: "https://chat.example", TOTPSecret: secret, TOTPFactorID: "second-factor", Sentinel: fakeSentinel,
		TOTPVerified: func(context.Context, string) error { legacyCalls++; return nil },
		TOTPFactorVerified: func(_ context.Context, acceptedSeed, factorID string) error {
			boundCalls++
			if acceptedSeed != secret || factorID != "second-factor" {
				t.Fatal("accepted binding did not match the submitted credentials")
			}
			return nil
		},
	})
	p := authPayload{"continue_url": "https://auth.example/mfa-challenge/first-factor", "oai-client-auth-session": map[string]any{
		"mfa_challenge_factors": []any{loginTOTPFactor("first-factor")},
		"mfa_factors":           []any{loginTOTPFactor("first-factor"), loginTOTPFactor("second-factor")},
	}}
	if _, err := flow.verifyMFA(t.Context(), p, "https://auth.example/log-in/password"); err != nil {
		t.Fatal(err)
	}
	if len(paths) != 2 || boundCalls != 1 || legacyCalls != 0 {
		t.Fatalf("requests=%d binding callbacks=%d legacy callbacks=%d", len(paths), boundCalls, legacyCalls)
	}
}

func TestMFALoginDoesNotGuessMissingOrAmbiguousFactorBinding(t *testing.T) {
	for _, tc := range []struct {
		name, binding, wantError string
	}{
		{"unbound-multiple", "", "MFA_FACTOR_AMBIGUOUS"},
		{"bound-factor-no-longer-advertised", "old-factor", "MFA_FACTOR_BINDING_MISSING"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requests, inputs, saves := 0, 0, 0
			flow := NewFlow(NewClient(doerFunc(func(context.Context, transport.Request) (*transport.Response, error) {
				requests++
				return nil, errors.New("unexpected request")
			}), nil, ""), FlowOptions{TOTPSecret: "JBSWY3DPEHPK3PXP", TOTPFactorID: tc.binding, Sentinel: fakeSentinel,
				Input:              func(context.Context, string, string) (string, error) { inputs++; return "123456", nil },
				TOTPFactorVerified: func(context.Context, string, string) error { saves++; return nil },
			})
			p := authPayload{"oai-client-auth-session": map[string]any{"mfa_factors": []any{loginTOTPFactor("first-factor"), loginTOTPFactor("second-factor")}}}
			_, err := flow.verifyMFA(t.Context(), p, "")
			if err == nil || !strings.HasPrefix(err.Error(), tc.wantError+":") || requests != 0 || inputs != 0 || saves != 0 {
				t.Fatalf("err=%v requests=%d inputs=%d saves=%d", err, requests, inputs, saves)
			}
			for _, private := range []string{"first-factor", "second-factor", "old-factor", flow.Options.TOTPSecret} {
				if strings.Contains(err.Error(), private) {
					t.Fatal("factor selection diagnostic exposed private material")
				}
			}
		})
	}
}

func TestSelectTOTPLoginFactorDeduplicatesBothListsAndSkipsRecovery(t *testing.T) {
	p := authPayload{"oai-client-auth-session": map[string]any{
		"mfa_challenge_factors": []any{
			map[string]any{"factor_type": "sms", "id": "sms"},
			map[string]any{"factor_type": "totp", "id": "recovery", "is_recovery": true},
			loginTOTPFactor(""), loginTOTPFactor("only-factor"),
		},
		"mfa_factors": []any{loginTOTPFactor("only-factor")},
	}}
	if id, err := selectTOTPLoginFactor(p, ""); err != nil || id != "only-factor" {
		t.Fatalf("id=%q err=%v", id, err)
	}
	if _, err := selectTOTPLoginFactor(p, "recovery"); err == nil || !strings.HasPrefix(err.Error(), "MFA_FACTOR_BINDING_MISSING:") {
		t.Fatalf("recovery factor was selected: %v", err)
	}
}

func TestTOTPFactorVerifiedRequiresAcceptedAutomaticSeed(t *testing.T) {
	storageErr := errors.New("fixture binding storage failed")
	for _, tc := range []struct {
		name                        string
		manual, rejected, noAdvance bool
		storageFailure              bool
		wantCalls                   int
	}{
		{name: "unbound-single-auto", wantCalls: 1},
		{name: "manual-code", manual: true},
		{name: "rejected-auto-then-manual", rejected: true},
		{name: "unconfirmed-response", noAdvance: true},
		{name: "binding-storage-failure", storageFailure: true, wantCalls: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			verifications, saves := 0, 0
			client := NewClient(doerFunc(func(_ context.Context, r transport.Request) (*transport.Response, error) {
				if strings.HasSuffix(r.URL, "/issue_challenge") {
					return response(200, `{}`), nil
				}
				verifications++
				if tc.rejected && verifications == 1 {
					return response(403, `{"error":{"code":"incorrect_code"}}`), nil
				}
				if tc.noAdvance {
					return response(200, `{}`), nil
				}
				return response(200, `{"continue_url":"https://chat.example/"}`), nil
			}), nil, "")
			secret := "JBSWY3DPEHPK3PXP"
			if tc.manual {
				secret = ""
			}
			flow := NewFlow(client, FlowOptions{AuthBase: "https://auth.example", ChatGPTBase: "https://chat.example", TOTPSecret: secret, Sentinel: fakeSentinel,
				Input: func(context.Context, string, string) (string, error) { return "123456", nil },
				TOTPFactorVerified: func(_ context.Context, gotSecret, factorID string) error {
					saves++
					if gotSecret != secret || gotSecret == "" || factorID != "only-factor" {
						t.Fatal("binding callback did not describe accepted automatic credentials")
					}
					if tc.storageFailure {
						return storageErr
					}
					return nil
				},
			})
			p := authPayload{"oai-client-auth-session": map[string]any{"mfa_factors": []any{loginTOTPFactor("only-factor")}}}
			_, err := flow.verifyMFA(t.Context(), p, "https://auth.example/log-in/password")
			if tc.storageFailure {
				if !errors.Is(err, storageErr) {
					t.Fatalf("storage error not propagated: %v", err)
				}
			} else if tc.noAdvance {
				if err == nil || !strings.HasPrefix(err.Error(), "MFA_RESULT_UNKNOWN:") {
					t.Fatalf("unconfirmed success accepted: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if saves != tc.wantCalls {
				t.Fatalf("binding callbacks=%d want=%d", saves, tc.wantCalls)
			}
			wantBinding := ""
			if tc.wantCalls == 1 && !tc.storageFailure {
				wantBinding = "only-factor"
			}
			if flow.Options.TOTPFactorID != wantBinding {
				t.Fatal("in-memory binding changed without accepted automatic verification and persistence")
			}
		})
	}
}
