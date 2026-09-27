package protocol

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
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

func TestFlowEmailResumeResendAndReject(t *testing.T) {
	var validate, resend int
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
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
	if e := f.Restore(Checkpoint{Version: 1, Stage: "web_email_otp", Email: "a@example.com", Cookies: []transport.Cookie{}, Web: map[string]any{"deviceId": "d"}, OAuth: map[string]any{"pageURL": s.URL + "/email-verification", "loginMethod": "email_otp"}}); e != nil {
		t.Fatal(e)
	}
	web, e := f.LoginWeb(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	if resend != 1 || validate != 2 || web["emailVerified"] != true || len(stages) != 3 {
		t.Fatalf("resend=%d validate=%d web=%v stages=%v", resend, validate, web, stages)
	}
}
func fakeSentinel(context.Context, string, string, string) (http.Header, error) {
	h := http.Header{}
	h.Set("openai-sentinel-token", "synthetic-test-fixture")
	return h, nil
}

func TestPhoneCheckpointResumeDoesNotResend(t *testing.T) {
	sends := 0
	validates := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/add-phone":
			w.Write([]byte("phone"))
		case "/api/accounts/add-phone/send":
			sends++
			t.Error("resume must not resend the existing SMS")
			w.WriteHeader(500)
		case "/api/accounts/phone-otp/validate":
			validates++
			json.NewEncoder(w).Encode(map[string]any{"continue_url": "http://localhost:1455/auth/callback?code=test-code&state=test-state"})
		default:
			t.Errorf("unexpected %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer s.Close()
	cp := Checkpoint{Version: 1, Stage: "phone_otp", Email: "a@example.com", Cookies: []transport.Cookie{}, OAuth: map[string]any{"codeVerifier": "verifier", "state": "test-state", "addPhoneUrl": s.URL + "/add-phone", "phone": "+12025550123", "phoneReferer": s.URL + "/phone-verification"}}
	f := NewFlow(nil, FlowOptions{ChatGPTBase: s.URL, AuthBase: s.URL, Email: cp.Email, Input: func(_ context.Context, stage, _ string) (string, error) {
		if stage != "sms_otp" {
			t.Fatalf("unexpected input %s", stage)
		}
		return "123456", nil
	}})
	if e := f.Restore(cp); e != nil {
		t.Fatal(e)
	}
	o, e := f.ResumeOAuth(context.Background(), cp)
	if e != nil {
		t.Fatal(e)
	}
	if sends != 0 || validates != 1 || o["callbackUrl"] == "" {
		t.Fatal(o)
	}
}

func TestIncompletePhoneCheckpointCollectsNumberBeforeSMS(t *testing.T) {
	for _, scenario := range []string{"missing-phone", "invalid-phone", "missing-referer", "configured-phone"} {
		t.Run(scenario, func(t *testing.T) {
			base := "https://auth.example.test"
			sends, validates, prompts := 0, 0, 0
			expectedPhone := "+12025550124"
			f := NewFlow(NewClient(doerFunc(func(_ context.Context, r transport.Request) (*transport.Response, error) {
				switch r.URL {
				case base + "/add-phone":
					return response(http.StatusOK, "phone"), nil
				case base + "/api/accounts/add-phone/send":
					sends++
					var body map[string]any
					if json.Unmarshal(r.Body, &body) != nil || body["phone_number"] != expectedPhone {
						t.Fatal("incomplete checkpoint sent to the stale or missing phone")
					}
					b, err := json.Marshal(map[string]any{"continue_url": base + "/phone-verification"})
					return response(http.StatusOK, string(b)), err
				case base + "/api/accounts/phone-otp/validate":
					validates++
					if sends != 1 || r.Headers.Get("Referer") != base+"/phone-verification" {
						t.Fatal("SMS validated without a newly selected number")
					}
					b, err := json.Marshal(map[string]any{"continue_url": DefaultCodexRedirectURI + "?code=fixture-code&state=fixture-state"})
					return response(http.StatusOK, string(b)), err
				default:
					t.Fatal("phone checkpoint recovery restarted unrelated authentication")
					return nil, errors.New("unexpected request")
				}
			}), nil, ""), FlowOptions{Email: "fixture@example.test", AuthBase: base, ChatGPTBase: base, Input: func(_ context.Context, stage, _ string) (string, error) {
				switch stage {
				case "phone":
					prompts++
					return expectedPhone, nil
				case "sms_otp":
					if sends != 1 {
						t.Fatal("incomplete checkpoint skipped phone collection")
					}
					return "123456", nil
				default:
					t.Fatal("unexpected authentication input")
					return "", errors.New("unexpected prompt")
				}
			}})
			oauth := map[string]any{"codeVerifier": "fixture-verifier", "state": "fixture-state", "addPhoneUrl": base + "/add-phone",
				"phone": "+12025550123", "phoneReferer": base + "/phone-verification"}
			switch scenario {
			case "missing-phone":
				delete(oauth, "phone")
			case "invalid-phone":
				oauth["phone"] = "broken-phone"
			case "missing-referer":
				delete(oauth, "phoneReferer")
			case "configured-phone":
				delete(oauth, "phone")
				f.Options.Phone = expectedPhone
			}
			cp := Checkpoint{Version: 1, Stage: "phone_otp", Email: f.Options.Email, Cookies: []transport.Cookie{}, OAuth: oauth}
			if err := f.Restore(cp); err != nil {
				t.Fatal(err)
			}
			if _, err := f.ResumeOAuth(context.Background(), cp); err != nil {
				t.Fatal(err)
			}
			expectedPrompts := 1
			if scenario == "configured-phone" {
				expectedPrompts = 0
			}
			if sends != 1 || validates != 1 || prompts != expectedPrompts {
				t.Fatal("incomplete phone checkpoint did not finish exactly once")
			}
		})
	}
}

func TestHTTPErrorDoesNotEchoSecrets(t *testing.T) {
	e := newHTTPError(400, []byte(`{"error":{"message":"invalid password secret-password cookie=abc token=xyz"}}`))
	if strings.Contains(e.Error(), "secret-password") || e.Code != "invalid_password" {
		t.Fatal(e)
	}
}

func TestCheckpointRejectsWrongEmailAndOrigin(t *testing.T) {
	f := NewFlow(nil, FlowOptions{Email: "owner@example.com"})
	cp := Checkpoint{Version: 1, Stage: "email_verified", Email: "other@example.com", Cookies: []transport.Cookie{}}
	if f.Restore(cp) == nil {
		t.Fatal("cross-account checkpoint accepted")
	}
	cp.Email = f.Options.Email
	cp.AuthBase = "https://other.example"
	if f.Restore(cp) == nil {
		t.Fatal("cross-origin checkpoint accepted")
	}
}
