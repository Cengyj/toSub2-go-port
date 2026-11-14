package protocol

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"tosub2-go-port/internal/transport"
)

func TestFlowPasswordMFAWorkspaceOAuth(t *testing.T) {
	mux := http.NewServeMux()
	selected := false
	wsCount := 0
	oauthState := ""
	mfaID := "factor"
	email := "flow@example.com"
	srv := httptest.NewTLSServer(mux)
	defer srv.Close()
	base := srv.URL
	jsonOut := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("action") == "enable" {
			w.Header().Add("Set-Cookie", "__Secure-next-auth.session-token=s; Secure; Path=/")
			w.Write([]byte(`<script>accessToken":"` + strings.Repeat("x", 30) + `"</script>`))
			return
		}
		w.Header().Add("Set-Cookie", "__Host-next-auth.csrf-token=x; Secure; Path=/")
		w.Write([]byte("ok"))
	})
	mux.HandleFunc("/api/auth/session", func(w http.ResponseWriter, r *http.Request) {
		jsonOut(w, map[string]any{"user": map[string]any{"email": email}})
	})
	mux.HandleFunc("/api/auth/providers", func(w http.ResponseWriter, r *http.Request) { jsonOut(w, map[string]any{}) })
	mux.HandleFunc("/api/auth/csrf", func(w http.ResponseWriter, r *http.Request) { jsonOut(w, map[string]any{"csrfToken": "x"}) })
	mux.HandleFunc("/api/auth/signin/openai", func(w http.ResponseWriter, r *http.Request) { jsonOut(w, map[string]any{"url": base + "/authorize"}) })
	mux.HandleFunc("/authorize", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, base+"/log-in/password", 302) })
	mux.HandleFunc("/log-in/password", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("password")) })
	mux.HandleFunc("/api/accounts/password/verify", func(w http.ResponseWriter, r *http.Request) {
		jsonOut(w, map[string]any{"continue_url": base + "/mfa-challenge/" + mfaID, "page": map[string]any{"type": "mfa_challenge"}, "oai-client-auth-session": map[string]any{"mfa_factors": []any{map[string]any{"factor_type": "totp", "id": mfaID}}}})
	})
	mux.HandleFunc("/api/accounts/mfa/issue_challenge", func(w http.ResponseWriter, r *http.Request) { jsonOut(w, map[string]any{}) })
	mux.HandleFunc("/api/accounts/mfa/verify", func(w http.ResponseWriter, r *http.Request) {
		jsonOut(w, map[string]any{"continue_url": base + "/workspace", "page": map[string]any{"type": "workspace"}, "oai-client-auth-session": map[string]any{"workspaces": []any{map[string]any{"id": "org", "kind": "organization"}}}})
	})
	mux.HandleFunc("/api/accounts/workspace/select", func(w http.ResponseWriter, r *http.Request) {
		selected = true
		wsCount++
		if wsCount == 1 {
			jsonOut(w, map[string]any{"continue_url": base + "/web-callback"})
			return
		}
		jsonOut(w, map[string]any{"continue_url": "http://localhost:1455/auth/callback?code=mock&state=" + oauthState})
	})
	mux.HandleFunc("/web-callback", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, base+"/", 302) })
	mux.HandleFunc("/oauth/authorize", func(w http.ResponseWriter, r *http.Request) {
		oauthState = r.URL.Query().Get("state")
		http.Redirect(w, r, base+"/choose-an-account", 302)
	})
	mux.HandleFunc("/choose-an-account", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<input name="session_id" value="us_1234567890abcdef">`))
	})
	mux.HandleFunc("/api/accounts/session/select", func(w http.ResponseWriter, r *http.Request) {
		jsonOut(w, map[string]any{"oai-client-auth-session": map[string]any{"workspaces": []any{map[string]any{"id": "org", "kind": "organization"}}}})
	})
	mux.HandleFunc("/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		id := base64JWT(`{"email":"` + email + `","sid":"acct","sub":"user"}`)
		jsonOut(w, map[string]any{"access_token": "a", "refresh_token": "r", "id_token": id})
	})
	tc := transport.NewClient(nil)
	tc.HTTP.Transport = srv.Client().Transport
	c := NewClient(tc, nil, "")
	f := NewFlow(c, FlowOptions{ChatGPTBase: base, AuthBase: base, Email: email, Password: "pw", TOTPSecret: "JBSWY3DPEHPK3PXP", Input: func(context.Context, string, string) (string, error) { return "123456", nil }, Sentinel: func(context.Context, string, string, string) (http.Header, error) {
		h := http.Header{}
		h.Set("openai-sentinel-token", `{"token":"test"}`)
		return h, nil
	}})
	out, err := f.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !selected {
		t.Fatal("workspace not selected")
	}
	if out.Export.Accounts[0].Credentials.Email != email {
		t.Fatal(out.Export)
	}
}
func base64JWT(payload string) string { return "e30." + raw64([]byte(payload)) + ".x" }
func raw64(b []byte) string           { return base64.RawURLEncoding.EncodeToString(b) }

func TestFlowEmailResendAndRejectInCurrentCall(t *testing.T) {
	var validate, resend int
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/auth/session":
			json.NewEncoder(w).Encode(map[string]any{"user": map[string]any{"email": "a@example.com"}})
		case "/api/accounts/email-otp/resend":
			resend++
			json.NewEncoder(w).Encode(map[string]any{})
		case "/api/accounts/email-otp/validate":
			validate++
			if validate == 1 {
				w.WriteHeader(400)
				w.Write([]byte(`{"error":{"message":"wrong email otp code; secret MUSTNOTLEAK"}}`))
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"continue_url": "http://" + r.Host + "/"})
		case "/":
			w.Write([]byte("ok"))
		default:
			t.Errorf("resume unexpectedly restarted: %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer s.Close()
	values := []string{"r", "000000", "123456"}
	var stages []string
	f := NewFlow(nil, FlowOptions{ChatGPTBase: s.URL, AuthBase: s.URL, Email: "a@example.com", Input: func(_ context.Context, stage, _ string) (string, error) {
		stages = append(stages, stage)
		v := values[0]
		values = values[1:]
		return v, nil
	}, Sentinel: fakeSentinel})
	_, e := f.verifyEmail(context.Background(), s.URL+"/email-verification")
	if e != nil {
		t.Fatal(e)
	}
	if resend != 1 || validate != 2 || len(stages) != 3 {
		t.Fatalf("resend=%d validate=%d stages=%v", resend, validate, stages)
	}
}
func fakeSentinel(context.Context, string, string, string) (http.Header, error) {
	h := http.Header{}
	h.Set("openai-sentinel-token", "synthetic-test-fixture")
	return h, nil
}

func TestHTTPErrorDoesNotEchoSecrets(t *testing.T) {
	e := newHTTPError(400, []byte(`{"error":{"message":"invalid password secret-password cookie=abc token=xyz"}}`))
	if strings.Contains(e.Error(), "secret-password") || e.Code != "invalid_password" {
		t.Fatal(e)
	}
}

func TestAllLegacyLoginProgressIsRejectedWithoutRequests(t *testing.T) {
	calls := 0
	f := NewFlow(NewClient(doerFunc(func(context.Context, transport.Request) (*transport.Response, error) { calls++; return nil, nil }), nil, ""), FlowOptions{Email: "current@example.test"})
	for _, stage := range []string{"email_verified", "web_password", "callback_ready", "tokens_received"} {
		legacy := map[string]any{"stage": stage, "email": "current@example.test", "cookies": []any{map[string]any{"name": "old", "value": "private"}}}
		if f.Restore(legacy) == nil {
			t.Fatal("restored legacy login")
		}
		if _, err := f.ResumeOAuth(t.Context(), legacy); err == nil {
			t.Fatal("resumed legacy code")
		}
	}
	if calls != 0 {
		t.Fatal("rejected login caused network probe")
	}
}
