package protocol

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"tosub2-go-port/internal/transport"
)

type bpsFixture struct {
	t                                          *testing.T
	state, challenge                           string
	counts                                     map[string]int
	emailOTP, missingRefresh, callbackRedirect bool
	expiry                                     any
}

func (b *bpsFixture) request(_ context.Context, r transport.Request) (*transport.Response, error) {
	u, _ := url.Parse(r.URL)
	if u.Host != "auth.example.test" {
		b.t.Fatalf("BPS visited another host: %s", u.Host)
	}
	b.counts[u.Path]++
	result := func(v any) (*transport.Response, error) {
		raw, _ := json.Marshal(v)
		return &transport.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: raw}, nil
	}
	workspace := map[string]any{"continue_url": "https://auth.example.test/workspace", "page": map[string]any{"type": "workspace"}, "oai-client-auth-session": map[string]any{"workspaces": []any{map[string]any{"id": "personal", "kind": "personal"}, map[string]any{"id": "organization", "kind": "organization"}}}}
	callback := DefaultBPSRedirectURI + "?code=synthetic-once&state=" + url.QueryEscape(b.state)
	switch u.Path {
	case "/api/accounts/authorize":
		q := u.Query()
		b.state, b.challenge = q.Get("state"), q.Get("code_challenge")
		if q.Get("client_id") != DefaultBPSClientID || q.Get("redirect_uri") != DefaultBPSRedirectURI || q.Get("scope") != "openid offline_access email profile organization.read" || q.Get("audience") != "https://api.openai.com/v1" || q.Get("platform") != "PC" || !strings.HasPrefix(b.state, "bps.") || !strings.HasSuffix(b.state, ".PC") || q.Get("codex_cli_simplified_flow") != "" {
			b.t.Fatal("BPS authorization parameters do not match its registered application")
		}
		return &transport.Response{StatusCode: 302, Header: http.Header{"Location": {"https://auth.example.test/log-in"}}}, nil
	case "/log-in":
		return response(200, "login"), nil
	case "/api/accounts/authorize/continue":
		var data map[string]any
		_ = json.Unmarshal(r.Body, &data)
		username := payload(data["username"])
		if str(username["kind"]) != "email" || str(username["value"]) != "owner@example.test" || r.Headers.Get("Openai-Sentinel-Token") != "authorize_continue" {
			b.t.Fatal("BPS login did not submit expected username or Sentinel flow")
		}
		path, typ := "/log-in/password", "login_password"
		if b.emailOTP {
			path, typ = "/email-verification", "email_verification"
		}
		return result(map[string]any{"continue_url": "https://auth.example.test" + path, "page": map[string]any{"type": typ}})
	case "/api/accounts/password/verify":
		if r.Headers.Get("Openai-Sentinel-Token") != "password_verify" {
			b.t.Fatal("BPS password lost Sentinel context")
		}
		return result(map[string]any{"continue_url": "https://auth.example.test/mfa-challenge/fixture", "page": map[string]any{"type": "mfa_challenge"}, "oai-client-auth-session": map[string]any{"mfa_factors": []any{map[string]any{"id": "fixture", "factor_type": "totp"}}}})
	case "/api/accounts/mfa/issue_challenge":
		return result(map[string]any{})
	case "/api/accounts/mfa/verify":
		if r.Headers.Get("Openai-Sentinel-Token") != "" {
			b.t.Fatal("BPS MFA sent a password Sentinel token")
		}
		return result(workspace)
	case "/api/accounts/email-otp/validate":
		return result(workspace)
	case "/api/accounts/workspace/select":
		var data map[string]any
		_ = json.Unmarshal(r.Body, &data)
		if str(data["workspace_id"]) != "organization" {
			b.t.Fatal("BPS did not use normal workspace selection")
		}
		if b.callbackRedirect {
			callback = "https://auth.example.test/final-redirect"
		}
		return result(map[string]any{"continue_url": callback, "page": map[string]any{"type": "external_url"}, "oai-client-auth-session": workspace["oai-client-auth-session"]})
	case "/final-redirect":
		return &transport.Response{StatusCode: 302, Header: http.Header{"Location": {callback}}}, nil
	case "/oauth/token":
		q, _ := url.ParseQuery(string(r.Body))
		if u.Query().Get("unified") != "true" || q.Get("client_id") != DefaultBPSClientID {
			b.t.Fatal("BPS token used the wrong client or endpoint")
		}
		if q.Get("grant_type") == "authorization_code" && (q.Get("redirect_uri") != DefaultBPSRedirectURI || !VerifyPKCE(q.Get("code_verifier"), b.challenge)) {
			b.t.Fatal("BPS token lost PKCE binding")
		}
		token := TokenSet{AccessToken: base64JWT(`{"sub":"user","auth":{"chatgpt_account_id":"bps-account","chatgpt_user_id":"bps-user","chatgpt_plan_type":"pro"},"https://api.openai.com/profile":{"email":"owner@example.test"}}`), RefreshToken: "synthetic-refresh"}
		if b.missingRefresh {
			token.RefreshToken = ""
		}
		if b.expiry != nil {
			return result(map[string]any{"access_token": token.AccessToken, "refresh_token": token.RefreshToken, "expires_in": b.expiry})
		}
		return result(token)
	default:
		b.t.Fatalf("unexpected BPS endpoint: %s", u.Path)
		return nil, errors.New("unexpected endpoint")
	}
}

func (b *bpsFixture) flow() *Flow {
	return NewFlow(NewClient(doerFunc(b.request), nil, ""), FlowOptions{
		AuthorizationMethod: "bps", Email: "owner@example.test", AuthBase: "https://auth.example.test", Password: "synthetic-password", TOTPSecret: "JBSWY3DPEHPK3PXP",
		Sentinel: func(_ context.Context, _, _, flow string) (http.Header, error) {
			return http.Header{"Openai-Sentinel-Token": {flow}}, nil
		},
		Input: func(context.Context, string, string) (string, error) { return "123456", nil },
	})
}

func TestBPSIndependentOAuthAndIDTokenOptionalExport(t *testing.T) {
	for _, emailOTP := range []bool{false, true} {
		b := &bpsFixture{t: t, counts: map[string]int{}, emailOTP: emailOTP}
		result, err := b.flow().Run(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		a := result.Export.Accounts[0]
		if a.Extra["client_id"] != DefaultBPSClientID || a.Credentials.ClientID != DefaultBPSClientID || a.Credentials.ChatGPTAccountID != "bps-account" || a.Credentials.Email != "owner@example.test" || a.Credentials.IDToken != "" || a.Extra["chatgpt_user_id"] != "bps-user" || b.counts["/oauth/token"] != 1 {
			t.Fatal("BPS export lost its application identity or claims")
		}
	}
}

func TestBPSTokenExpiryMetadataCannotDiscardAcceptedGrant(t *testing.T) {
	for _, expiry := range []any{"3600", float64(3600), "malformed", map[string]any{"unexpected": true}} {
		b := &bpsFixture{t: t, counts: map[string]int{}, expiry: expiry}
		f := b.flow()
		var saved bool
		f.Options.SaveCheckpoint = func(c Checkpoint) error {
			if HasSavedOAuthGrant(c) {
				saved = true
				if c.OAuth["tokenResponse"].(TokenSet).AccessToken == "" {
					t.Fatal("accepted access token missing from checkpoint")
				}
			}
			return nil
		}
		if _, err := f.Run(context.Background()); err != nil || !saved || b.counts["/oauth/token"] != 1 {
			t.Fatal("optional expiry discarded the accepted grant", err)
		}
	}
}

func TestBPSExportRejectsMissingOrMismatchedIdentity(t *testing.T) {
	for _, claims := range []string{
		`{"sid":"session-not-account","email":"owner@example.test"}`,
		`{"auth":{"chatgpt_account_id":"account"}}`,
		`{"auth":{"chatgpt_account_id":"account"},"email":"different@example.test"}`,
	} {
		if _, err := BuildSub2APIOAuthExport(TokenSet{AccessToken: base64JWT(claims), RefreshToken: "synthetic"}, DefaultBPSClientID, "owner@example.test", "", 0, 0, 0); err == nil {
			t.Fatal("BPS exported a missing or different identity")
		}
	}
	result, err := BuildSub2APIOAuthExport(TokenSet{AccessToken: "synthetic", RefreshToken: "synthetic", IDToken: base64JWT(`{"email":"owner@example.test","sid":"fixture"}`)}, DefaultCodexClientID, "owner@example.test", "", 0, 0, 0)
	if err != nil || result.Accounts[0].Credentials.ClientID != DefaultCodexClientID || result.Accounts[0].Extra["client_id"] != DefaultCodexClientID {
		t.Fatal("ChatGPT export did not retain its registered client", err)
	}
}

func TestBPSCheckpointResumesAcceptedPasswordAndWorkspaceWithoutReplay(t *testing.T) {
	for _, stopAt := range []string{"mfa_challenge", "external_url"} {
		t.Run(stopAt, func(t *testing.T) {
			b := &bpsFixture{t: t, counts: map[string]int{}}
			f := b.flow()
			var saved Checkpoint
			f.Options.SaveCheckpoint = func(cp Checkpoint) error {
				data, _ := json.Marshal(cp)
				_ = json.Unmarshal(data, &saved)
				if pageType(payload(cp.OAuth["current"])) == stopAt {
					return errors.New("synthetic checkpoint interruption")
				}
				return nil
			}
			if _, err := f.Run(context.Background()); err == nil {
				t.Fatal("expected interruption")
			}
			if saved.AuthorizationMethod != "bps" || saved.OAuth["clientId"] != DefaultBPSClientID || saved.OAuth["redirectUri"] != DefaultBPSRedirectURI {
				t.Fatal("BPS checkpoint lost method/client binding")
			}
			next := b.flow()
			if err := next.Restore(saved); err != nil {
				t.Fatal(err)
			}
			if _, err := next.Run(context.Background()); err != nil {
				t.Fatal(err)
			}
			if b.counts["/api/accounts/password/verify"] != 1 || b.counts["/api/accounts/workspace/select"] != 1 || b.counts["/oauth/token"] != 1 {
				t.Fatal("BPS resume replayed accepted credentials or workspace selection")
			}
			other := NewFlow(nil, FlowOptions{Email: saved.Email, AuthBase: saved.AuthBase})
			if err := other.Restore(saved); err == nil {
				t.Fatal("ChatGPT accepted a BPS checkpoint")
			}
		})
	}
}

func TestBPSMissingRefreshSavesAcceptedGrantAndNeverRedeemsCodeAgain(t *testing.T) {
	b := &bpsFixture{t: t, counts: map[string]int{}, missingRefresh: true}
	f := b.flow()
	f.Options.CheckpointPath = filepath.Join(t.TempDir(), "checkpoint.json")
	if _, err := f.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "BPS_REFRESH_TOKEN_MISSING") {
		t.Fatal("missing refresh token did not explain why export cannot continue", err)
	}
	cp, err := ReadCheckpoint(f.Options.CheckpointPath)
	if err != nil || !HasSavedOAuthGrant(cp) {
		t.Fatal("accepted BPS grant was discarded", err)
	}
	next := b.flow()
	if err := next.Restore(cp); err != nil {
		t.Fatal(err)
	}
	if _, err := next.Run(context.Background()); err == nil || b.counts["/oauth/token"] != 1 {
		t.Fatal("accepted BPS code was redeemed again")
	}
}

func TestBPSRefreshUsesUnifiedEndpoint(t *testing.T) {
	b := &bpsFixture{t: t, counts: map[string]int{}}
	if _, err := RefreshOAuthToken(context.Background(), doerFunc(b.request), "https://auth.example.test", DefaultBPSClientID, "synthetic-refresh"); err != nil {
		t.Fatal(err)
	}
}

func TestBPSCallbackRedirectInterceptsBeforeDispatch(t *testing.T) {
	calls := 0
	c := NewClient(doerFunc(func(_ context.Context, r transport.Request) (*transport.Response, error) {
		calls++
		if r.URL != "https://auth.example.test/redirect" {
			t.Fatal("dispatched HTTPS callback containing a one-use code")
		}
		return &transport.Response{StatusCode: 302, Header: http.Header{"Location": {DefaultBPSRedirectURI + "?code=private&state=fixture"}}}, nil
	}), nil, "")
	f := NewFlow(c, FlowOptions{AuthorizationMethod: "bps"})
	r, err := f.follow(context.Background(), "https://auth.example.test/redirect", "")
	if err != nil || !f.isCallback(r.FinalURL) || calls != 1 {
		t.Fatal("callback interception failed", err)
	}
}

func TestAuthorizationMethodValidationAndCheckpointIdentity(t *testing.T) {
	if _, err := NewFlow(nil, FlowOptions{AuthorizationMethod: "invalid", Email: "owner@example.test"}).Run(context.Background()); err == nil {
		t.Fatal("unknown method accepted")
	}
	f := NewFlow(nil, FlowOptions{AuthorizationMethod: "bps", Email: "owner@example.test"})
	for _, field := range []string{"clientId", "redirectUri"} {
		cp := Checkpoint{Version: 1, Stage: "callback_ready", AuthorizationMethod: "bps", Email: f.Options.Email, Cookies: []transport.Cookie{}, OAuth: map[string]any{"clientId": DefaultBPSClientID, "redirectUri": DefaultBPSRedirectURI}}
		cp.OAuth[field] = "different-client"
		if err := f.Restore(cp); err == nil {
			t.Fatal("BPS accepted another OAuth identity")
		}
	}
}
