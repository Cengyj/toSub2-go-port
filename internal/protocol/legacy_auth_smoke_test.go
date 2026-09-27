package protocol

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"tosub2-go-port/internal/transport"
)

// This ports the legacy HTTP mock contracts using TLS so __Host/__Secure cookie
// checks remain identical to production. All identities and secrets are fixtures.
type legacyAuthMode struct {
	email, profile, skipWebWorkspace, directAuthorize, directSession bool
	rejectedPassword, rejectedMFA                                    bool
	profileResult                                                    string
}
type legacyAuthFixture struct {
	t                              *testing.T
	mu                             sync.Mutex
	base, state, challenge, device string
	mode                           legacyAuthMode
	counts                         map[string]int
	invocations, proofs            map[string]bool
	webDone                        bool
	authorizeAt                    time.Time
}

var fixtureUUID = regexp.MustCompile("^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$")

func (s *legacyAuthFixture) json(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func (s *legacyAuthFixture) check(ok bool, message string) {
	if !ok {
		s.t.Error(message)
	}
}
func (s *legacyAuthFixture) post(r *http.Request, expected map[string]any, referer, sentinel string) map[string]any {
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		s.t.Error("JSON body required")
	}
	if expected != nil {
		s.check(reflect.DeepEqual(body, expected), "unexpected request body for "+r.URL.Path)
	}
	s.check(r.Method == http.MethodPost && r.Header.Get("Content-Type") == "application/json", "auth writes must use POST JSON")
	s.check(r.Header.Get("Origin") == s.base && r.Header.Get("Referer") == s.base+referer, "auth origin/referer mismatch for "+r.URL.Path)
	id := r.Header.Get("X-Access-Flow-Invocation-Id")
	s.check(fixtureUUID.MatchString(id) && !s.invocations[id], "invocation ID must be fresh UUID for each logical auth write")
	s.invocations[id] = true
	s.check(r.Header.Get("Sec-Fetch-Site") == "same-origin" && r.Header.Get("Sec-Fetch-Mode") == "cors" && r.Header.Get("Sec-Fetch-Dest") == "empty" && r.Header.Get("Priority") == "u=1, i", "auth request metadata missing")
	if sentinel == "" {
		s.check(r.Header.Get("Openai-Sentinel-Token") == "", "Sentinel token leaked to unrelated step")
		return body
	}
	var proof map[string]any
	_ = json.Unmarshal([]byte(r.Header.Get("Openai-Sentinel-Token")), &proof)
	s.check(proof["flow"] == sentinel && proof["id"] == s.device && proof["c"] == "fixture-challenge" && r.Header.Get("Openai-Sentinel-So-Token") == "fixture-observer", "matching Sentinel and Session Observer required")
	p := str(proof["p"])
	s.check(p != "" && !s.proofs[p], "Sentinel proof reused across operations")
	s.proofs[p] = true
	return body
}
func (s *legacyAuthFixture) workspacePayload() map[string]any {
	return map[string]any{"continue_url": s.base + "/workspace", "page": map[string]any{"type": "workspace"}, "oai-client-auth-session": map[string]any{"workspaces": []any{map[string]any{"id": "personal", "kind": "personal"}, map[string]any{"id": "organization", "kind": "organization"}}}}
}
func (s *legacyAuthFixture) callback() string {
	return DefaultCodexRedirectURI + "?" + url.Values{"code": {"fixture-code"}, "state": {s.state}}.Encode()
}
func (s *legacyAuthFixture) mfaPayload() map[string]any {
	return map[string]any{"continue_url": s.base + "/mfa-challenge/factor", "page": map[string]any{"type": "mfa_challenge"}, "oai-client-auth-session": map[string]any{"mfa_challenge_factors": []any{map[string]any{"factor_type": "sms", "id": "not-totp"}, map[string]any{"factor_type": "totp", "id": "factor"}}}}
}
func (s *legacyAuthFixture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	path := r.URL.Path
	s.counts[path]++
	if r.Method == http.MethodGet {
		s.check(strings.Contains(r.Header.Get("Accept"), "text/html") || strings.HasPrefix(path, "/api/auth/"), "navigation must advertise HTML")
	}
	switch path {
	case "/":
		http.SetCookie(w, &http.Cookie{Name: "__Host-next-auth.csrf-token", Value: "fixture-csrf", Path: "/", Secure: true})
		if s.webDone {
			http.SetCookie(w, &http.Cookie{Name: "__Secure-next-auth.session-token", Value: "fixture-session", Path: "/", Secure: true})
		}
		_, _ = w.Write([]byte("fixture home"))
	case "/api/auth/providers":
		s.json(w, 200, map[string]any{})
	case "/api/auth/csrf":
		s.json(w, 200, map[string]any{"csrfToken": "fixture-csrf"})
	case "/api/auth/signin/openai":
		_ = r.ParseForm()
		q := r.URL.Query()
		s.device = q.Get("ext-oai-did")
		s.check(q.Get("prompt") == "login" && q.Get("screen_hint") == "login_or_signup" && q.Get("login_hint") == "login-hint@example.test" && fixtureUUID.MatchString(s.device) && fixtureUUID.MatchString(q.Get("auth_session_logging_id")), "signin parameters incompatible")
		s.check(r.PostForm.Get("csrfToken") == "fixture-csrf" && r.PostForm.Get("callbackUrl") == s.base+"/" && r.PostForm.Get("json") == "true", "signin form changed")
		c, e := r.Cookie("__Host-next-auth.csrf-token")
		s.check(e == nil && c.Value == "fixture-csrf", "CSRF cookie lost")
		s.json(w, 200, map[string]any{"url": s.base + "/api/accounts/authorize"})
	case "/api/accounts/authorize":
		s.authorizeAt = time.Now().UTC()
		next := "/log-in/password"
		if s.mode.email {
			next = "/email-verification"
		}
		http.Redirect(w, r, s.base+next, 302)
	case "/log-in/password", "/email-verification":
		_, _ = w.Write([]byte("fixture verification"))
	case "/api/accounts/password/verify":
		body := s.post(r, nil, "/log-in/password", "password_verify")
		s.check(len(body) == 1, "password request contains unrelated fields")
		if s.mode.rejectedPassword && s.counts[path] == 1 {
			s.json(w, 401, map[string]any{"error": map[string]any{"message": "password wrong; fixture-secret-must-not-escape"}})
			return
		}
		s.check(body["password"] == "Fixture-Password9!", "unexpected synthetic password")
		s.json(w, 200, s.mfaPayload())
	case "/api/accounts/email-otp/resend":
		s.post(r, map[string]any{}, "/email-verification", "")
		s.json(w, 200, map[string]any{"success": true})
	case "/api/accounts/email-otp/validate":
		body := s.post(r, nil, "/email-verification", "email_otp_validate")
		s.check(len(body) == 1, "email OTP body changed")
		if body["code"] != "123456" {
			s.json(w, 401, map[string]any{"error": map[string]any{"code": "wrong_email_otp_code"}})
			return
		}
		if s.mode.profile {
			s.json(w, 200, map[string]any{"page": map[string]any{"type": "about_you"}})
			return
		}
		s.json(w, 200, s.mfaPayload())
	case "/api/accounts/create_account":
		body := s.post(r, nil, "/about-you", "oauth_create_account")
		s.check(len(body) == 2 && regexp.MustCompile("^[A-Za-z]+ [A-Za-z]+$").MatchString(str(body["name"])), "profile name/body invalid")
		birth, err := time.Parse("2006-01-02", str(body["birthdate"]))
		now := time.Now().UTC()
		age := now.Year() - birth.Year()
		if now.Month() < birth.Month() || now.Month() == birth.Month() && now.Day() < birth.Day() {
			age--
		}
		s.check(err == nil && age >= 20 && age <= 50, "generated age must be 20 to 50")
		switch s.mode.profileResult {
		case "about_you":
			s.json(w, 200, map[string]any{"page": map[string]any{"type": "about_you"}, "continue_url": s.base + "/about-you"})
		case "external":
			s.json(w, 200, map[string]any{"page": map[string]any{"type": "external_url"}, "continue_url": "https://untrusted.example/callback"})
		case "missing_type":
			s.json(w, 200, map[string]any{"continue_url": s.base + "/api/auth/callback/openai"})
		case "blocked":
			s.json(w, 400, map[string]any{"error": map[string]any{"code": "registration_disallowed"}})
		default:
			s.json(w, 200, map[string]any{"page": map[string]any{"type": "external_url"}, "continue_url": s.base + "/api/auth/callback/openai?code=fixture-web-code"})
		}
	case "/api/accounts/mfa/issue_challenge":
		ref := "/log-in/password"
		if s.mode.email {
			ref = "/email-verification"
		}
		s.post(r, map[string]any{"type": "totp", "id": "factor", "force_fresh_challenge": false}, ref, "")
		s.json(w, 200, map[string]any{})
	case "/api/accounts/mfa/verify":
		body := s.post(r, nil, "/mfa-challenge/factor", "password_verify")
		s.check(len(body) == 3 && body["type"] == "totp" && body["id"] == "factor", "MFA body invalid")
		if s.mode.rejectedMFA && s.counts[path] == 1 {
			s.json(w, 400, map[string]any{"error": map[string]any{"code": "invalid_otp"}})
			return
		}
		valid := s.mode.rejectedMFA && body["code"] == "654321"
		for _, delta := range []time.Duration{-30 * time.Second, 0, 30 * time.Second} {
			code, _ := GenerateTOTP("JBSWY3DPEHPK3PXP", time.Now().Add(delta))
			valid = valid || body["code"] == code
		}
		s.check(valid, "invalid generated TOTP")
		if s.mode.skipWebWorkspace {
			s.json(w, 200, map[string]any{"page": map[string]any{"type": "workspace"}, "continue_url": s.base + "/web-callback"})
			return
		}
		s.json(w, 200, s.workspacePayload())
	case "/api/accounts/workspace/select":
		ref := "/workspace"
		if s.webDone {
			ref = "/sign-in-with-chatgpt/codex/consent"
		}
		s.post(r, map[string]any{"workspace_id": "organization"}, ref, "")
		next := s.base + "/web-callback"
		if s.webDone {
			next = s.callback()
		}
		s.json(w, 200, map[string]any{"continue_url": next})
	case "/web-callback", "/api/auth/callback/openai":
		s.webDone = true
		http.Redirect(w, r, s.base+"/", 303)
	case "/oauth/authorize":
		s.check(s.webDone, "Codex started before web login")
		q := r.URL.Query()
		s.state = q.Get("state")
		s.challenge = q.Get("code_challenge")
		s.check(q.Get("client_id") == DefaultCodexClientID && q.Get("redirect_uri") == DefaultCodexRedirectURI && q.Get("response_type") == "code" && q.Get("code_challenge_method") == "S256" && s.state != "" && s.challenge != "", "PKCE authorize parameters missing")
		c, e := r.Cookie("__Secure-next-auth.session-token")
		s.check(e == nil && c.Value == "fixture-session", "web session cookie not retained")
		if s.mode.directAuthorize {
			http.Redirect(w, r, s.callback(), 302)
			return
		}
		http.Redirect(w, r, s.base+"/choose-an-account", 302)
	case "/choose-an-account":
		_, _ = w.Write([]byte("<input name=\"session_id\" value=\"us_1234567890abcdef\">"))
	case "/api/accounts/session/select":
		s.post(r, map[string]any{"session_id": "us_1234567890abcdef"}, "/choose-an-account", "")
		if s.mode.directSession {
			s.json(w, 200, map[string]any{"continue_url": s.callback()})
			return
		}
		p := s.workspacePayload()
		delete(p, "continue_url")
		s.json(w, 200, p)
	case "/oauth/token":
		_ = r.ParseForm()
		s.check(r.PostForm.Get("grant_type") == "authorization_code" && r.PostForm.Get("code") == "fixture-code" && r.PostForm.Get("client_id") == DefaultCodexClientID && r.PostForm.Get("redirect_uri") == DefaultCodexRedirectURI && VerifyPKCE(r.PostForm.Get("code_verifier"), s.challenge), "token exchange not bound to PKCE")
		s.json(w, 200, TokenSet{AccessToken: "fixture-access", RefreshToken: "fixture-refresh", IDToken: base64JWT("{\"email\":\"authenticated@example.test\",\"sid\":\"fixture-account\",\"sub\":\"fixture-user\"}")})
	default:
		s.t.Error("unexpected fixture request: " + r.Method + " " + path)
		s.json(w, 404, map[string]any{"error": "unexpected"})
	}
}
func newLegacyAuthFlow(t *testing.T, mode legacyAuthMode, inputs [][2]string) (*Flow, *legacyAuthFixture) {
	t.Helper()
	s := &legacyAuthFixture{t: t, mode: mode, counts: map[string]int{}, invocations: map[string]bool{}, proofs: map[string]bool{}}
	server := httptest.NewTLSServer(s)
	s.base = server.URL
	t.Cleanup(server.Close)
	tc := transport.NewClient(nil)
	tc.HTTP.Transport = server.Client().Transport
	index, proofIndex := 0, 0
	dir := t.TempDir()
	opt := FlowOptions{ChatGPTBase: s.base, AuthBase: s.base, Email: "login-hint@example.test", Password: "Fixture-Password9!", TOTPSecret: "JBSWY3DPEHPK3PXP", CheckpointPath: filepath.Join(dir, "login-checkpoint.json"), OutputPath: filepath.Join(dir, "sub2api-import-oauth.json"), Input: func(_ context.Context, stage, prompt string) (string, error) {
		if index >= len(inputs) {
			return "", fmt.Errorf("unexpected input stage %s", stage)
		}
		v := inputs[index]
		index++
		if stage != v[0] {
			return "", fmt.Errorf("wanted input %s, got %s", v[0], stage)
		}
		return v[1], nil
	}, Sentinel: func(_ context.Context, origin, device, flow string) (http.Header, error) {
		proofIndex++
		b, _ := json.Marshal(map[string]any{"flow": flow, "id": device, "c": "fixture-challenge", "p": fmt.Sprint(proofIndex)})
		h := http.Header{}
		h.Set("Openai-Sentinel-Token", string(b))
		h.Set("Openai-Sentinel-So-Token", "fixture-observer")
		return h, nil
	}}
	if mode.rejectedPassword {
		opt.Password = "Fixture-Rejected9!"
	}
	if mode.rejectedMFA {
		opt.TOTPSecret = "NB2W45DFOIZAQWER"
	}
	f := NewFlow(NewClient(tc, nil, ""), opt)
	t.Cleanup(func() {
		if index != len(inputs) {
			t.Errorf("consumed %d of %d expected prompts", index, len(inputs))
		}
	})
	return f, s
}
func TestLegacyPasswordEmailMFAWorkspaceSmoke(t *testing.T) {
	cases := []struct {
		name       string
		mode       legacyAuthMode
		inputs     [][2]string
		workspaces int
	}{
		{"password_mfa", legacyAuthMode{}, nil, 2},
		{"email_reject_resend_mfa", legacyAuthMode{email: true}, [][2]string{{"email_otp", "000000"}, {"email_otp", "r"}, {"email_otp", "123456"}}, 2},
		{"workspace_without_ids_skips", legacyAuthMode{skipWebWorkspace: true}, nil, 1},
		{"direct_authorize_callback", legacyAuthMode{directAuthorize: true}, nil, 1},
		{"phone_bound_direct_session_callback", legacyAuthMode{directSession: true}, nil, 1},
		{"manual_password_mfa_after_rejection", legacyAuthMode{rejectedPassword: true, rejectedMFA: true}, [][2]string{{"password", "Fixture-Password9!"}, {"totp", "invalid-local-code"}, {"totp", "654321"}}, 2},
		{"email_profile_completion", legacyAuthMode{email: true, profile: true}, [][2]string{{"email_otp", "123456"}}, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, s := newLegacyAuthFlow(t, tc.mode, tc.inputs)
			var requested []time.Time
			var events []string
			f.Options.EmailOTPRequested = func(at time.Time) { requested = append(requested, at) }
			f.Options.Event = func(stage, status, prompt string) { events = append(events, stage+":"+prompt) }
			out, err := f.Run(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			s.mu.Lock()
			defer s.mu.Unlock()
			if s.counts["/api/accounts/workspace/select"] != tc.workspaces || s.counts["/oauth/token"] != 1 {
				t.Fatalf("unexpected route counts: %v", s.counts)
			}
			if out.Export.Accounts[0].Credentials.Email != "authenticated@example.test" || out.Export.Accounts[0].Name != "oauth---authenticated@example.test" {
				t.Fatal("export must use authenticated token identity")
			}
			if tc.mode.email && (len(requested) == 0 || requested[0].After(s.authorizeAt)) {
				t.Fatal("mail cutoff lost request timestamp")
			}
			if tc.mode.email && !tc.mode.profile && (len(requested) != 2 || !requested[1].After(requested[0])) {
				t.Fatal("resend did not advance cutoff")
			}
			if _, err = os.Stat(f.Options.CheckpointPath); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("completed checkpoint not removed")
			}
			var persisted Sub2APIOAuthExport
			b, err := os.ReadFile(f.Options.OutputPath)
			if err != nil || json.Unmarshal(b, &persisted) != nil || persisted.Type != "sub2api-data" {
				t.Fatal("compatible export missing")
			}
			for _, secret := range []string{"Fixture-Password9!", "Fixture-Rejected9!", "654321", "123456", "fixture-access", "fixture-refresh", "fixture-secret-must-not-escape"} {
				if strings.Contains(strings.Join(events, "\n"), secret) {
					t.Fatal("events leaked a secret")
				}
			}
		})
	}
}
func TestLegacyProfileMustAdvanceToExpectedChatGPTCallback(t *testing.T) {
	for _, result := range []string{"about_you", "external", "missing_type", "blocked"} {
		t.Run(result, func(t *testing.T) {
			f, s := newLegacyAuthFlow(t, legacyAuthMode{email: true, profile: true, profileResult: result}, [][2]string{{"email_otp", "123456"}})
			_, err := f.Run(context.Background())
			if err == nil {
				t.Fatal("incomplete profile result accepted")
			}
			s.mu.Lock()
			defer s.mu.Unlock()
			if s.counts["/oauth/authorize"] != 0 || s.counts["/oauth/token"] != 0 {
				t.Fatal("OAuth ran after incomplete profile")
			}
			if f.checkpoint.Stage != "web_continue" {
				t.Fatal("profile recovery checkpoint lost")
			}
		})
	}
}
