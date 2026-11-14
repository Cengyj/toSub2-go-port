package protocol

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tosub2-go-port/internal/transport"
)

type recoveryHarness struct {
	t                                                 *testing.T
	flow                                              *Flow
	loginStarts, oauthRequests, tokenRequests, resets int
	intercept                                         func(context.Context, transport.Request) (*transport.Response, error, bool)
}

func fixtureJSON(v any) *transport.Response {
	b, _ := json.Marshal(v)
	return response(http.StatusOK, string(b))
}
func fixtureRedirect(location string) *transport.Response {
	r := response(http.StatusFound, "")
	r.Header.Set("Location", location)
	return r
}
func fixtureExpiry() *transport.Response {
	r := fixtureJSON(map[string]any{"error": map[string]any{"code": "session_expired"}})
	r.StatusCode = http.StatusUnauthorized
	return r
}
func newRecoveryHarness(t *testing.T) *recoveryHarness {
	t.Helper()
	h := &recoveryHarness{t: t}
	c := NewClient(doerFunc(h.do), nil, "")
	dir := t.TempDir()
	h.flow = NewFlow(c, FlowOptions{
		ChatGPTBase: "https://chat.example", AuthBase: "https://auth.example", Email: "fixture@example.test",
		OutputPath: filepath.Join(dir, "output.json"), Sentinel: fakeSentinel,
		Input: func(context.Context, string, string) (string, error) { return "123456", nil },
		Event: func(stage, status, prompt string) {
			if stage == "auth_expired" {
				h.resets++
			}
		},
	})
	return h
}
func (h *recoveryHarness) do(ctx context.Context, r transport.Request) (*transport.Response, error) {
	if h.intercept != nil {
		if result, err, handled := h.intercept(ctx, r); handled {
			return result, err
		}
	}
	u, err := url.Parse(r.URL)
	if err != nil {
		return nil, err
	}
	switch u.Path {
	case "/api/auth/session":
		return fixtureJSON(map[string]any{"user": map[string]any{"email": h.flow.Options.Email}}), nil
	case "/":
		h.loginStarts++
		if h.resets > 0 {
			if h.flow.Client.Jar.Value("expired-auth", "https://auth.example/") != "" {
				h.t.Error("expired auth cookie survived reset")
			}
			if _, e := os.Stat(filepath.Join(filepath.Dir(h.flow.Options.OutputPath), "login-checkpoint.json")); !errors.Is(e, os.ErrNotExist) {
				h.t.Error("expired checkpoint was not removed before fresh login")
			}
		}
		h.flow.Client.Jar.Set(transport.Cookie{Name: "oai-did", Value: "fixture-device", Domain: "chat.example", Path: "/", Secure: true})
		h.flow.Client.Jar.Set(transport.Cookie{Name: "__Host-next-auth.csrf-token", Value: "fixture-csrf", Domain: "chat.example", Path: "/", HostOnly: true, Secure: true})
		return response(200, "home"), nil
	case "/api/auth/providers":
		return fixtureJSON(map[string]any{}), nil
	case "/api/auth/csrf":
		return fixtureJSON(map[string]any{"csrfToken": "fixture-csrf"}), nil
	case "/api/auth/signin/openai":
		if u.Query().Get("post_login_add_password") == "true" {
			h.t.Error("password enrollment request not handled")
		}
		if h.resets > 0 && u.Query().Get("ext-oai-did") != "fixture-device" {
			h.t.Error("reset changed browser device identity")
		}
		return fixtureJSON(map[string]any{"url": "https://auth.example/authorize"}), nil
	case "/authorize":
		return fixtureRedirect("https://auth.example/email-verification"), nil
	case "/email-verification":
		return response(200, "email verification"), nil
	case "/api/accounts/email-otp/validate":
		return fixtureJSON(map[string]any{"continue_url": "https://chat.example/web-callback"}), nil
	case "/web-callback":
		h.flow.Client.Jar.Set(transport.Cookie{Name: "__Secure-next-auth.session-token", Value: "fixture-new-session", Domain: "chat.example", Path: "/", Secure: true, HostOnly: true})
		return response(200, "signed in"), nil
	case "/oauth/authorize":
		h.oauthRequests++
		return fixtureRedirect(DefaultCodexRedirectURI + "?" + url.Values{"code": {"fixture-code"}, "state": {u.Query().Get("state")}}.Encode()), nil
	case "/oauth/token":
		h.tokenRequests++
		return fixtureJSON(TokenSet{AccessToken: "fixture-access", RefreshToken: "fixture-refresh", IDToken: base64JWT("{\"email\":\"fixture@example.test\",\"sid\":\"fixture-account\"}")}), nil
	case "/log-in", "/reset-password/new-password":
		return response(200, "fixture page"), nil
	default:
		h.t.Errorf("unexpected request %s %s", r.Method, u.Path)
		return nil, errors.New("unexpected fixture request")
	}
}
func (h *recoveryHarness) checkpoint(t *testing.T, stage string, oauth map[string]any) {
	t.Helper()
	h.flow.checkpoint = &Checkpoint{Stage: stage, Web: map[string]any{"deviceId": "fixture-device"}, OAuth: oauth}
	h.flow.web = h.flow.checkpoint.Web
	h.flow.deviceID = "fixture-device"

}

func TestFreshLoginExpiryRestartsOnce(t *testing.T) {
	h := newRecoveryHarness(t)
	h.flow.deviceID = "fixture-device"
	h.intercept = func(_ context.Context, r transport.Request) (*transport.Response, error, bool) {
		if strings.Contains(r.URL, "/oauth/authorize") && h.oauthRequests == 0 {
			h.oauthRequests++
			return fixtureExpiry(), nil, true
		}
		return nil, nil, false
	}
	if _, err := h.flow.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h.loginStarts != 2 || h.resets != 1 || h.oauthRequests != 2 || h.tokenRequests != 1 {
		t.Fatalf("login=%d resets=%d oauth=%d exchange=%d", h.loginStarts, h.resets, h.oauthRequests, h.tokenRequests)
	}
}

func TestOAuthTokenFailureNeverRestartsLoginOrReplaysExchange(t *testing.T) {
	h := newRecoveryHarness(t)
	h.checkpoint(t, "callback_ready", map[string]any{"state": "fixture-state", "codeVerifier": "fixture-verifier", "callbackUrl": DefaultCodexRedirectURI + "?code=fixture-code&state=fixture-state"})
	h.intercept = func(_ context.Context, r transport.Request) (*transport.Response, error, bool) {
		if strings.HasSuffix(r.URL, "/oauth/token") {
			h.tokenRequests++
			return fixtureExpiry(), nil, true
		}
		return nil, nil, false
	}
	if _, err := h.flow.Run(context.Background()); err == nil {
		t.Fatal("expected token error")
	}
	if h.tokenRequests != 1 || h.loginStarts != 1 || h.resets != 0 {
		t.Fatal("uncertain token exchange was replayed")
	}
}

func TestExpiryClassifierUsesExplicitSessionSignals(t *testing.T) {
	for _, code := range []string{"session_expired", "session_invalid", "invalid_session", "invalid_state", "login_required"} {
		b, _ := json.Marshal(map[string]any{"error": map[string]any{"code": code, "message": "untrusted-description"}})
		if !isExpiredSession(newHTTPError(409, b)) {
			t.Errorf("explicit session code not classified: %s", code)
		}
	}
	for _, code := range []string{"invalid_password", "invalid_otp", "invalid_authorization_step", "account_deactivated", "access_denied", "registration_disallowed"} {
		b, _ := json.Marshal(map[string]any{"error": map[string]any{"code": code}})
		if isExpiredSession(newHTTPError(401, b)) {
			t.Errorf("non-expiry code classified as expired: %s", code)
		}
	}
	for _, err := range []error{errors.New("SESSION_SELECTION_INVALID"), errors.New("TOTP_ACCESS_TOKEN_MISSING"), errors.New("CHECKPOINT_INVALID"), errors.New("fixture connection reset"), context.Canceled, context.DeadlineExceeded} {
		if isExpiredSession(err) {
			t.Errorf("unconfirmed failure allowed relogin: %v", err)
		}
	}
	_, err := ParseOAuthCallback(DefaultCodexRedirectURI+"?state=s&error=login_required", "s")
	if !isExpiredSession(err) {
		t.Fatalf("explicit callback expiry lost: %v", err)
	}
	_, err = ParseOAuthCallback(DefaultCodexRedirectURI+"?state=wrong&error=login_required", "s")
	if isExpiredSession(err) {
		t.Fatal("state mismatch treated as expiry")
	}
}

func TestUnrecognizedOAuthPageDoesNotRestartLogin(t *testing.T) {
	h := newRecoveryHarness(t)
	h.checkpoint(t, "email_verified", nil)
	h.intercept = func(_ context.Context, r transport.Request) (*transport.Response, error, bool) {
		if strings.Contains(r.URL, "/oauth/authorize") {
			h.oauthRequests++
			return response(200, "browser verification still required"), nil, true
		}
		return nil, nil, false
	}
	if _, err := h.flow.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "SESSION_SELECTION_INVALID") {
		t.Fatalf("unexpected result %v", err)
	}
	if h.resets != 0 || h.loginStarts != 1 || h.oauthRequests != 1 {
		t.Fatal("unconfirmed browser challenge restarted login")
	}
}
