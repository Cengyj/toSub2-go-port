package protocol

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"tosub2-go-port/internal/transport"
)

// The legacy assertOk error includes the response body, and rejection helpers
// inspect its meaning regardless of whether the provider wrapped it in error.
// Keep that control flow without retaining response bodies in public errors.
func TestLegacyRejectedCredentialResponseShapes(t *testing.T) {
	shapes := []struct {
		name string
		body func(string) string
	}{
		{"nested-error", func(message string) string {
			return jsonErrorFixture(map[string]any{"error": map[string]any{"message": message}})
		}},
		{"top-level-message", func(message string) string { return jsonErrorFixture(map[string]any{"message": message}) }},
		{"top-level-code", func(message string) string {
			return jsonErrorFixture(map[string]any{"code": strings.ReplaceAll(message, " ", "_")})
		}},
		{"oauth-description", func(message string) string {
			return jsonErrorFixture(map[string]any{"error": "invalid_request", "error_description": message})
		}},
		{"json-string", func(message string) string { return jsonErrorFixture(message) }},
		{"plain-text", func(message string) string { return message }},
	}
	for _, kind := range []string{"password", "email_otp", "totp"} {
		for _, shape := range shapes {
			t.Run(kind+"/"+shape.name, func(t *testing.T) {
				var attempts atomic.Int32
				message := "incorrect code"
				if kind == "password" {
					message = "incorrect password"
				}
				body := shape.body(message + " fixture-private-response-echo")
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method != http.MethodPost {
						t.Error("expected credential verification POST")
					}
					w.Header().Set("Content-Type", "application/json")
					if strings.HasSuffix(r.URL.Path, "/issue_challenge") {
						_, _ = w.Write([]byte("{}"))
						return
					}
					if attempts.Add(1) == 1 {
						w.WriteHeader(http.StatusBadRequest)
						_, _ = w.Write([]byte(body))
						return
					}
					_, _ = w.Write([]byte("{\"continue_url\":\"/fixture-next\"}"))
				}))
				defer server.Close()
				inputs := 0
				flow := NewFlow(NewClient(transport.NewClient(nil), nil, ""), FlowOptions{AuthBase: server.URL, Password: "Fixture-Initial9!", Sentinel: fakeSentinel, Input: func(_ context.Context, stage, prompt string) (string, error) {
					inputs++
					if inputs > 2 {
						return "", errors.New("unexpected repeated input")
					}
					if strings.Contains(prompt, "fixture-private-response-echo") {
						t.Error("remote error body leaked into prompt")
					}
					if kind == "password" {
						return "Fixture-Corrected9!", nil
					}
					return "123456", nil
				}})
				var result authPayload
				var err error
				switch kind {
				case "password":
					result, err = flow.verifyPassword(context.Background(), server.URL+"/log-in/password")
				case "email_otp":
					result, err = flow.verifyEmail(context.Background(), server.URL+"/email-verification")
				case "totp":
					result, err = flow.verifyMFA(context.Background(), authPayload{"oai-client-auth-session": map[string]any{"mfa_challenge_factors": []any{map[string]any{"factor_type": "totp", "id": "fixture-factor"}}}}, server.URL+"/mfa-challenge/totp")
				}
				if err != nil || continuation(result) != "/fixture-next" || attempts.Load() != 2 {
					t.Fatalf("legacy rejection did not permit corrected input: attempts=%d err=%v", attempts.Load(), err)
				}
				if err := newHTTPError(http.StatusBadRequest, []byte(body)); strings.Contains(err.Error(), "fixture-private") {
					t.Fatal("public error retained remote response content")
				}
			})
		}
	}
}

func TestPermissionErrorCannotBecomeSessionExpiryFromItsMessage(t *testing.T) {
	for _, code := range []string{"access_denied", "permission_denied", "forbidden"} {
		body := jsonErrorFixture(map[string]any{"error": map[string]any{"code": code, "message": "The session is no longer valid for this permission."}})
		if isExpiredSession(newHTTPError(http.StatusForbidden, []byte(body))) {
			t.Errorf("explicit %s permission rejection became an automatic re-login", code)
		}
	}
}

func TestLegacyEmptyPasswordStopsBeforeSentinelOrSubmission(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_, _ = w.Write([]byte("{\"continue_url\":\"/fixture-next\"}"))
	}))
	defer server.Close()
	proofs := 0
	flow := NewFlow(NewClient(transport.NewClient(nil), nil, ""), FlowOptions{AuthBase: server.URL, Sentinel: func(ctx context.Context, auth, device, name string) (http.Header, error) {
		proofs++
		return fakeSentinel(ctx, auth, device, name)
	}, Input: func(context.Context, string, string) (string, error) {
		return "", nil
	}})
	_, err := flow.verifyPassword(context.Background(), server.URL+"/log-in/password")
	if err == nil || requests.Load() != 0 || proofs != 0 {
		t.Fatalf("empty password did not stop locally: requests=%d proofs=%d err=%v", requests.Load(), proofs, err)
	}
}

func jsonErrorFixture(value any) string {
	b, _ := json.Marshal(value)
	return string(b)
}
