package protocol

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	"tosub2-go-port/internal/transport"
)

type doerFunc func(context.Context, transport.Request) (*transport.Response, error)

func (f doerFunc) Do(ctx context.Context, r transport.Request) (*transport.Response, error) {
	return f(ctx, r)
}
func response(status int, body string) *transport.Response {
	return &transport.Response{StatusCode: status, Header: make(http.Header), Body: []byte(body)}
}
func enrollmentFlow(t *testing.T, doer Doer, opt FlowOptions) *Flow {
	t.Helper()
	opt.Email = "enroll@example.com"
	opt.ChatGPTBase = "https://chat.example"
	opt.AuthBase = "https://auth.example"
	f := NewFlow(NewClient(doer, nil, ""), opt)
	if e := f.Restore(Checkpoint{Version: 1, Stage: "email_verified", Email: opt.Email, Cookies: []transport.Cookie{{Name: "__Secure-next-auth.session-token", Value: "synthetic", Domain: "chat.example", Path: "/", Secure: true}}, Web: map[string]any{"deviceId": "fixture-device"}}); e != nil {
		t.Fatal(e)
	}
	return f
}
func TestTOTPEnrollmentPublishesBeforeManualActivation(t *testing.T) {
	published := false
	activations := 0
	asked := false
	d := doerFunc(func(_ context.Context, r transport.Request) (*transport.Response, error) {
		switch {
		case strings.Contains(r.URL, "?action=enable"):
			return response(200, `<script>{"accessToken":"synthetic-access-token-at-least-twenty-characters"}</script>`), nil
		case strings.HasSuffix(r.URL, "/mfa_info"):
			return response(200, `{"mfa_enabled_v2":false}`), nil
		case strings.HasSuffix(r.URL, "/mfa/enroll"):
			return response(200, `{"secret":"JBSWY3DPEHPK3PXP","session_id":"fixture-session"}`), nil
		case strings.HasSuffix(r.URL, "/activate_enrollment"):
			if !published {
				t.Error("activation before seed was protected")
			}
			activations++
			if activations == 1 {
				return response(400, `{"error":{"code":"invalid_code"}}`), nil
			}
			return response(200, `{"success":true}`), nil
		default:
			t.Fatalf("unexpected request %s", r.URL)
			return nil, nil
		}
	})
	f := enrollmentFlow(t, d, FlowOptions{TOTPEnrollment: func(_ context.Context, secret, uri string) error {
		if secret == "" || !strings.HasPrefix(uri, "otpauth://") {
			t.Fatal("missing setup material")
		}
		published = true
		return nil
	}, Input: func(_ context.Context, stage, prompt string) (string, error) {
		if stage != "totp_setup" || !published {
			t.Fatal("secret not available at prompt")
		}
		asked = true
		return "123456", nil
	}})
	out, e := f.SetupTOTP(context.Background(), "")
	if e != nil {
		t.Fatal(e)
	}
	if !asked || out["activation_succeeded"] != true || activations != 2 {
		t.Fatal("manual activation not completed")
	}
}
func TestTOTPStorageFailurePreventsActivation(t *testing.T) {
	d := doerFunc(func(_ context.Context, r transport.Request) (*transport.Response, error) {
		switch {
		case strings.Contains(r.URL, "?action=enable"):
			return response(200, `{"accessToken":"synthetic-access-token-at-least-twenty-characters"}`), nil
		case strings.HasSuffix(r.URL, "/mfa_info"):
			return response(200, `{}`), nil
		case strings.HasSuffix(r.URL, "/mfa/enroll"):
			return response(200, `{"secret":"JBSWY3DPEHPK3PXP","session_id":"fixture-session"}`), nil
		default:
			t.Error("activation must not run after protected storage fails")
			return nil, errors.New("unexpected")
		}
	})
	f := enrollmentFlow(t, d, FlowOptions{TOTPEnrollment: func(context.Context, string, string) error { return errors.New("storage locked") }})
	if _, e := f.SetupTOTP(context.Background(), ""); e == nil || e.Error() != "storage locked" {
		t.Fatal(e)
	}
}
func TestAddPasswordAuthorizedFlow(t *testing.T) {
	added := false
	d := doerFunc(func(_ context.Context, r transport.Request) (*transport.Response, error) {
		switch {
		case strings.HasSuffix(r.URL, "/api/auth/csrf"):
			return response(200, `{"csrfToken":"synthetic"}`), nil
		case strings.Contains(r.URL, "/api/auth/signin/openai?"):
			if !strings.Contains(r.URL, "post_login_add_password=true") {
				t.Error("missing reauthentication intent")
			}
			return response(200, `{"url":"https://auth.example/reset-password/new-password"}`), nil
		case strings.HasSuffix(r.URL, "/reset-password/new-password"):
			return response(200, `<html></html>`), nil
		case strings.HasSuffix(r.URL, "/api/accounts/password/add"):
			added = true
			if r.Headers.Get("Openai-Sentinel-Token") == "" {
				t.Error("missing sentinel")
			}
			return response(200, `{"continue_url":"https://chat.example/"}`), nil
		default:
			t.Fatalf("unexpected %s", r.URL)
			return nil, nil
		}
	})
	f := enrollmentFlow(t, d, FlowOptions{Sentinel: fakeSentinel})
	f.checkpoint.Stage = "phone_otp"
	f.checkpoint.OAuth = map[string]any{"state": "stale-oauth", "codeVerifier": "stale-verifier"}
	if e := f.AddPassword(context.Background(), "Synthetic-Password9!", ""); e != nil {
		t.Fatal(e)
	}
	if !added {
		t.Fatal("password not submitted")
	}
	if f.checkpoint.Stage != "email_verified" || f.checkpoint.OAuth != nil {
		t.Fatal("completed password enrollment retained old OAuth checkpoint")
	}
}
func TestClientChallengeSwitchKeepsManualResponse(t *testing.T) {
	first := doerFunc(func(context.Context, transport.Request) (*transport.Response, error) {
		return response(403, `<script src="/cdn-cgi/challenge-platform/mock.js"></script>`), nil
	})
	c := NewClient(first, nil, "")
	switched := false
	c.Challenge = func(_ context.Context, raw string) error {
		switched = true
		c.UserAgent = "browser-native"
		c.Transport = doerFunc(func(_ context.Context, r transport.Request) (*transport.Response, error) {
			if r.Headers.Get("User-Agent") != "browser-native" {
				t.Error("old TLS identity transferred")
			}
			out := response(302, "")
			out.Header.Set("Location", "/next")
			return out, nil
		})
		return nil
	}
	r, e := c.Request(context.Background(), http.MethodGet, "https://fixture.example/", RequestOptions{})
	if e != nil {
		t.Fatal(e)
	}
	if !switched || r.StatusCode != 302 {
		t.Fatal("challenge switch did not preserve redirect")
	}
}

func TestConnectionRetriesNeverReplayWrites(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		calls := 0
		client := NewClient(doerFunc(func(context.Context, transport.Request) (*transport.Response, error) {
			calls++
			if calls == 1 {
				return nil, io.EOF
			}
			return response(200, `{}`), nil
		}), nil, "")
		client.ConnectionRetries = 2
		client.RetryDelay = 1
		_, err := client.Request(context.Background(), method, "https://fixture.example", RequestOptions{})
		if method == http.MethodGet && (err != nil || calls != 2) {
			t.Fatal("idempotent read not retried")
		}
		if method == http.MethodPost && (err == nil || calls != 1) {
			t.Fatal("write was replayed")
		}
	}
}
func TestSessionOnlyResumeRetainsPrivateLegacyShape(t *testing.T) {
	path := t.TempDir() + "/session.json"
	h := newRecoveryHarness(t)
	f := h.flow
	f.Options.WebOnly, f.Options.OutputMode, f.Options.SessionOutputPath = true, "session", path
	h.checkpoint(t, "email_verified", nil)
	result, err := f.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if h.loginStarts != 1 || h.oauthRequests != 0 || h.tokenRequests != 0 {
		t.Fatal("web-only did not freshly authenticate")
	}
	if result.Web["emailVerified"] != true || len(result.Export.Accounts) != 0 {
		t.Fatal("web-only exchanged OAuth")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err = json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"generated_at", "chatgpt_base", "auth_base", "warning", "cookies", "web", "codex"} {
		if _, ok := raw[key]; !ok {
			t.Fatalf("legacy key %s missing", key)
		}
	}
}
func doNotUseClient() *Client {
	return NewClient(doerFunc(func(context.Context, transport.Request) (*transport.Response, error) {
		return nil, errors.New("unexpected network call")
	}), nil, "")
}
