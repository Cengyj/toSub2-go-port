package protocol

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"tosub2-go-port/internal/accountplan"
	"tosub2-go-port/internal/transport"
)

func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// TokenSet is the OAuth response used by Codex. ID tokens are deliberately
// retained because Sub2API imports all three token types.
type TokenSet struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	TokenType    string `json:"token_type,omitempty"`
	ExpiresIn    int    `json:"expires_in,omitempty"`
}

func (t TokenSet) Validate() error {
	if t.AccessToken == "" || t.RefreshToken == "" || t.IDToken == "" {
		return errors.New("token response missing access_token, refresh_token, or id_token")
	}
	return nil
}

func ExchangeOAuthCode(ctx context.Context, client Doer, authBase, clientID, code, verifier, redirectURI string) (TokenSet, error) {
	return oauthTokenRequest(ctx, client, authBase, url.Values{"grant_type": {"authorization_code"}, "client_id": {clientID}, "code": {code}, "redirect_uri": {redirectURI}, "code_verifier": {verifier}}, true)
}
func RefreshOAuthToken(ctx context.Context, client Doer, authBase, clientID, refreshToken string) (TokenSet, error) {
	return oauthTokenRequest(ctx, client, authBase, url.Values{"grant_type": {"refresh_token"}, "client_id": {clientID}, "refresh_token": {refreshToken}}, false)
}

// AuthorizationURL builds the Codex OAuth URL without hiding any parameters
// from the caller. State must be checked with ParseCallback before exchanging
// a returned code.
func AuthorizationURL(authBase, clientID, redirectURI string, p PKCE) (string, error) {
	if err := p.Validate(); err != nil {
		return "", err
	}
	base := strings.TrimRight(authBase, "/") + "/oauth/authorize"
	q := url.Values{}
	q.Set("client_id", clientID)
	q.Set("code_challenge", p.CodeChallenge)
	q.Set("code_challenge_method", "S256")
	q.Set("codex_cli_simplified_flow", "true")
	q.Set("id_token_add_organizations", "true")
	q.Set("redirect_uri", redirectURI)
	q.Set("response_type", "code")
	q.Set("scope", "openid profile email offline_access")
	q.Set("state", p.State)
	return base + "?" + q.Encode(), nil
}

type OAuthCallback struct {
	Code             string
	State            string
	Error            string
	ErrorDescription string
}

func ParseOAuthCallback(raw string, expectedState string) (OAuthCallback, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return OAuthCallback{}, errors.New("OAuth callback URL is invalid")
	}
	q := u.Query()
	cb := OAuthCallback{Code: q.Get("code"), State: q.Get("state"), Error: q.Get("error"), ErrorDescription: q.Get("error_description")}
	if expectedState == "" || cb.State != expectedState {
		return cb, errors.New("OAuth state mismatch")
	}
	if cb.Error != "" {
		if cb.Error == "login_required" {
			return cb, expiredSession("OAUTH_LOGIN_REQUIRED")
		}
		// Remote error descriptions can contain arbitrary echoed credentials.
		// Keep the parsed description available to callers but never log it.
		code := "authorization_error"
		switch cb.Error {
		case "invalid_request", "unauthorized_client", "access_denied", "unsupported_response_type", "invalid_scope", "server_error", "temporarily_unavailable", "login_required", "interaction_required", "consent_required":
			code = cb.Error
		}
		return cb, fmt.Errorf("OAuth authorization failed (%s)", code)
	}
	if cb.Code == "" {
		return cb, errors.New("OAuth callback URL does not contain code")
	}
	return cb, nil
}
func oauthTokenRequest(ctx context.Context, client Doer, authBase string, form url.Values, requireAll bool) (TokenSet, error) {
	if client == nil {
		client = transport.NewClient(nil)
	}
	endpoint := strings.TrimRight(authBase, "/") + "/oauth/token"
	req := transport.Request{Method: http.MethodPost, URL: endpoint, Headers: make(http.Header), Body: []byte(form.Encode())}
	req.Headers.Set("Accept", "application/json")
	req.Headers.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, e := client.Do(ctx, req)
	if e != nil {
		return TokenSet{}, e
	}
	var raw map[string]any
	if e = json.Unmarshal(resp.Body, &raw); e != nil {
		return TokenSet{}, fmt.Errorf("token endpoint returned non-JSON HTTP %d: %w", resp.StatusCode, e)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Token endpoints may echo request material in an error body. Keep only
		// the status and a stable machine-readable error code.
		code := "oauth_token_error"
		if v, ok := raw["error"].(string); ok {
			switch v {
			case "invalid_grant", "invalid_client", "invalid_request", "unauthorized_client", "unsupported_grant_type", "invalid_scope", "access_denied", "server_error", "temporarily_unavailable":
				code = v
			}
		}
		return TokenSet{}, fmt.Errorf("token request failed with HTTP %d (%s)", resp.StatusCode, code)
	}
	b, _ := json.Marshal(raw)
	var t TokenSet
	if e = json.Unmarshal(b, &t); e != nil {
		return TokenSet{}, e
	}
	if t.AccessToken == "" {
		return TokenSet{}, errors.New("token response missing access_token")
	}
	if requireAll {
		if e = t.Validate(); e != nil {
			return TokenSet{}, e
		}
	}
	return t, nil
}

func DecodeJWTClaims(token string) (map[string]any, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, errors.New("malformed JWT")
	}
	b, e := base64.RawURLEncoding.DecodeString(parts[1])
	if e != nil {
		return nil, e
	}
	var claims map[string]any
	if e = json.Unmarshal(b, &claims); e != nil {
		return nil, e
	}
	return claims, nil
}

type OAuthAccountCredentials struct {
	AccessToken      string `json:"access_token"`
	ChatGPTAccountID string `json:"chatgpt_account_id"`
	Email            string `json:"email"`
	IDToken          string `json:"id_token"`
	RefreshToken     string `json:"refresh_token"`
}
type OAuthAccount struct {
	Name               string                  `json:"name"`
	Platform           string                  `json:"platform"`
	Type               string                  `json:"type"`
	Credentials        OAuthAccountCredentials `json:"credentials"`
	Extra              map[string]any          `json:"extra"`
	Concurrency        int                     `json:"concurrency"`
	Priority           int                     `json:"priority"`
	RateMultiplier     float64                 `json:"rate_multiplier"`
	AutoPauseOnExpired bool                    `json:"auto_pause_on_expired"`
}
type Sub2APIOAuthExport struct {
	Type       string         `json:"type"`
	Version    int            `json:"version"`
	ExportedAt string         `json:"exported_at"`
	Proxies    []any          `json:"proxies"`
	Accounts   []OAuthAccount `json:"accounts"`
}

func BuildSub2APIOAuthExport(tokens TokenSet, clientID, email, accountName string, concurrency, priority int, rateMultiplier float64) (Sub2APIOAuthExport, error) {
	if e := tokens.Validate(); e != nil {
		return Sub2APIOAuthExport{}, e
	}
	claims, e := DecodeJWTClaims(tokens.IDToken)
	if e != nil {
		return Sub2APIOAuthExport{}, e
	}
	auth, _ := claims["https://api.openai.com/auth"].(map[string]any)
	// sid is an OIDC session identifier. Prefer the explicit account claim
	// whenever available; retain sid only for exports from the legacy mock/API.
	accountID := claimAccountID(claims)
	if accountID == "" {
		if accessClaims, err := DecodeJWTClaims(tokens.AccessToken); err == nil {
			accountID = claimAccountID(accessClaims)
		}
	}
	if accountID == "" {
		accountID, _ = claims["sid"].(string)
	}
	if authenticatedEmail, _ := claims["email"].(string); authenticatedEmail != "" {
		email = authenticatedEmail
	}
	userID, _ := auth["user_id"].(string)
	if userID == "" {
		userID, _ = claims["sub"].(string)
	}
	if accountName == "" {
		accountName = "oauth---" + email
		if email == "" {
			accountName += nowRFC3339()
		}
	}
	if concurrency <= 0 {
		concurrency = 10
	}
	if priority == 0 {
		priority = 1
	}
	if rateMultiplier <= 0 {
		rateMultiplier = 1
	}
	a := OAuthAccount{Name: accountName, Platform: "openai", Type: "oauth", Credentials: OAuthAccountCredentials{AccessToken: tokens.AccessToken, ChatGPTAccountID: accountID, Email: email, IDToken: tokens.IDToken, RefreshToken: tokens.RefreshToken}, Extra: map[string]any{"account_id": accountID, "chatgpt_account_id": accountID, "chatgpt_user_id": userID, "client_id": clientID, "email": email, "openai_long_context_billing_enabled": false, "openai_oauth_responses_websockets_v2_enabled": false, "openai_oauth_responses_websockets_v2_mode": "off", "privacy_mode": "training_set_failed"}, Concurrency: concurrency, Priority: priority, RateMultiplier: rateMultiplier, AutoPauseOnExpired: true}
	if plan := accountplan.FromTokens(tokens.IDToken, tokens.AccessToken); plan != "unknown" {
		a.Extra["plan_type"] = plan
	}
	return Sub2APIOAuthExport{Type: "sub2api-data", Version: 1, ExportedAt: nowRFC3339(), Proxies: []any{}, Accounts: []OAuthAccount{a}}, nil
}

func claimAccountID(claims map[string]any) string {
	if auth, ok := claims["https://api.openai.com/auth"].(map[string]any); ok {
		for _, key := range []string{"chatgpt_account_id", "account_id"} {
			if value, ok := auth[key].(string); ok && value != "" {
				return value
			}
		}
	}
	for _, key := range []string{"chatgpt_account_id", "account_id"} {
		if value, ok := claims[key].(string); ok && value != "" {
			return value
		}
	}
	return ""
}
