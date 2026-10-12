package protocol

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"

	"tosub2-go-port/internal/transport"
)

type legacyPhoneFixture struct {
	mu                sync.Mutex
	t                 *testing.T
	base, mode, state string
	counts            map[string]int
	ids               map[string]bool
	sent              []string
}

func (s *legacyPhoneFixture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	path := r.URL.Path
	s.counts[path]++
	out := func(status int, p any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(p)
	}
	reject := func(status int, code, message string) {
		out(status, map[string]any{"error": map[string]any{"code": code, "message": message}})
	}
	var body map[string]any
	if r.Method == http.MethodPost {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			s.t.Error(err)
		}
		id := r.Header.Get("X-Access-Flow-Invocation-Id")
		if !fixtureUUID.MatchString(id) || s.ids[id] {
			s.t.Error("logical phone requests reused invocation ID")
		}
		s.ids[id] = true
		if r.Header.Get("Origin") != s.base || r.Header.Get("Sec-Fetch-Mode") != "cors" {
			s.t.Error("phone auth headers missing")
		}
	}
	expect := func(v map[string]any, ref string) {
		if !reflect.DeepEqual(body, v) || r.Header.Get("Referer") != s.base+ref {
			s.t.Error("phone request body/referer mismatch: " + path)
		}
	}
	switch path {
	case "/oauth/authorize":
		s.state = r.URL.Query().Get("state")
		http.Redirect(w, r, s.base+"/choose-an-account", 302)
	case "/choose-an-account":
		_, _ = w.Write([]byte("<input name=\"session_id\" value=\"us_fixture_phone_session\">"))
	case "/api/accounts/session/select":
		expect(map[string]any{"session_id": "us_fixture_phone_session"}, "/choose-an-account")
		if s.mode == "unexpected_session" {
			out(200, map[string]any{"continue_url": s.base + "/device-confirmation"})
			return
		}
		// Deliberately omit continue_url: page.type alone is a supported branch.
		http.SetCookie(w, &http.Cookie{Name: "oai-client-auth-session", Value: "fixture-auth", Path: "/", Secure: true})
		out(200, map[string]any{"page": map[string]any{"type": "add_phone"}})
	case "/add-phone":
		if s.mode == "login_redirect" {
			http.Redirect(w, r, s.base+"/log-in", 302)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "add_phone_ready", Value: "1", Path: "/", Secure: true})
		_, _ = w.Write([]byte("fixture add phone"))
	case "/log-in":
		_, _ = w.Write([]byte("fixture login"))
	case "/api/accounts/add-phone/send":
		phone := str(body["phone_number"])
		s.sent = append(s.sent, phone)
		if c, e := r.Cookie("add_phone_ready"); e != nil || c.Value != "1" {
			s.t.Error("send was called before add-phone page initialized its cookies")
		}
		if c, e := r.Cookie("oai-client-auth-session"); e != nil || c.Value != "fixture-auth" {
			s.t.Error("session selection cookie lost")
		}
		if r.Header.Get("Referer") != s.base+"/add-phone" {
			s.t.Error("send must refer to initialized add-phone page")
		}
		if s.mode == "invalid_step" {
			reject(400, "invalid_auth_step", "Invalid authorization step")
			return
		}
		if s.mode == "challenge" {
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(409)
			_, _ = w.Write([]byte("<html><title>Just a moment</title><div id=\"challenge-platform\"></div></html>"))
			return
		}
		if s.mode == "expired" {
			reject(409, "invalid_state", "sign-in session no longer valid")
			return
		}
		if channel, ok := body["channel"]; ok {
			if channel != "sms" || len(body) != 2 {
				s.t.Error("primary phone send contract changed")
			}
			reject(400, "", "unexpected channel field")
			return
		}
		if len(body) != 1 {
			s.t.Error("fallback must remove only channel")
		}
		if s.mode == "invalid_number_state" && phone == "+12025550123" {
			reject(400, "invalid_state", "sign-in session no longer valid")
			return
		}
		if s.mode == "resend_failure" && s.counts[path] == 4 {
			reject(400, "", "fixture resend temporarily unavailable")
			return
		}
		out(200, map[string]any{"continue_url": s.base + "/phone-verification", "page": map[string]any{"type": "phone_otp_verification"}})
	case "/api/accounts/phone-otp/validate":
		if len(body) != 1 || r.Header.Get("Referer") != s.base+"/phone-verification" {
			s.t.Error("SMS verify body/referer changed")
		}
		if s.mode == "sms_expired" {
			reject(400, "invalid_state", "session expired")
			return
		}
		if body["code"] != "123456" {
			reject(400, "", "fixture SMS was not accepted")
			return
		}
		out(200, map[string]any{"oai-client-auth-session": map[string]any{"workspaces": []any{map[string]any{"id": "personal", "kind": "personal"}}}})
	case "/api/accounts/workspace/select":
		expect(map[string]any{"workspace_id": "personal"}, "/sign-in-with-chatgpt/codex/consent")
		out(200, map[string]any{"continue_url": DefaultCodexRedirectURI + "?" + url.Values{"code": {"phone-code"}, "state": {s.state}}.Encode()})
	default:
		s.t.Error("unexpected phone fixture request: " + path)
		out(404, map[string]any{})
	}
}
func newLegacyPhoneFlow(t *testing.T, mode string, inputs [][2]string) (*Flow, *legacyPhoneFixture) {
	t.Helper()
	s := &legacyPhoneFixture{t: t, mode: mode, counts: map[string]int{}, ids: map[string]bool{}}
	srv := httptest.NewTLSServer(s)
	s.base = srv.URL
	t.Cleanup(srv.Close)
	tc := transport.NewClient(nil)
	tc.HTTP.Transport = srv.Client().Transport
	index := 0
	f := NewFlow(NewClient(tc, nil, ""), FlowOptions{ChatGPTBase: s.base, AuthBase: s.base, Email: "phone@example.test", Input: func(_ context.Context, stage, prompt string) (string, error) {
		if index >= len(inputs) {
			return "", fmt.Errorf("unexpected prompt %s", stage)
		}
		v := inputs[index]
		index++
		if stage != v[0] {
			return "", fmt.Errorf("wanted %s input, got %s", v[0], stage)
		}
		return v[1], nil
	}})
	if err := f.Restore(Checkpoint{Version: 1, Stage: "email_verified", Email: f.Options.Email, Cookies: []transport.Cookie{}, Web: map[string]any{"deviceId": "fixture-device"}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if index != len(inputs) {
			t.Errorf("consumed %d of %d expected phone prompts", index, len(inputs))
		}
	})
	return f, s
}
func TestLegacyPhoneFallbackChangeAndResend(t *testing.T) {
	for _, tc := range []struct {
		name             string
		inputs           [][2]string
		sends, validates int
	}{
		{"normal", [][2]string{{"phone", "+12025550123"}, {"sms_otp", "000000"}, {"sms_otp", "p"}, {"phone", "+12025550124"}, {"sms_otp", "123456"}}, 4, 2},
		{"invalid_number_state", [][2]string{{"phone", "+12025550123"}, {"phone", "+12025550124"}, {"sms_otp", "123456"}}, 4, 1},
		{"resend_failure", [][2]string{{"phone", "+12025550123"}, {"sms_otp", "r"}, {"sms_otp", "123456"}}, 4, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, s := newLegacyPhoneFlow(t, tc.name, tc.inputs)
			o, err := f.RunOAuth(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			s.mu.Lock()
			defer s.mu.Unlock()
			if s.counts["/api/accounts/add-phone/send"] != tc.sends || s.counts["/api/accounts/phone-otp/validate"] != tc.validates || s.counts["/add-phone"] != 1 || s.counts["/api/accounts/workspace/select"] != 1 {
				t.Fatalf("wrong request sequence counts: %v", s.counts)
			}
			if o["workspaceId"] != "personal" || f.checkpoint.Stage != "callback_ready" {
				t.Fatal("phone flow did not reach callback")
			}
		})
	}
}
func TestLegacyPhoneStopsOnInvalidStepChallengeAndExpiredSession(t *testing.T) {
	for _, tc := range []struct {
		mode    string
		inputs  [][2]string
		sends   int
		code    string
		expired bool
	}{
		{"invalid_step", [][2]string{{"phone", "+12025550123"}}, 1, "invalid_auth_step", false},
		{"challenge", [][2]string{{"phone", "+12025550123"}}, 1, "AUTH_CHALLENGE_REQUIRED", false},
		{"expired", [][2]string{{"phone", "+12025550123"}}, 2, "invalid_state", true},
		{"sms_expired", [][2]string{{"phone", "+12025550123"}, {"sms_otp", "123456"}}, 2, "invalid_state", true},
		{"login_redirect", nil, 0, "PHONE_LOGIN_REQUIRED", true},
		{"unexpected_session", nil, 0, "SESSION_SELECTION_INVALID", false},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			f, s := newLegacyPhoneFlow(t, tc.mode, tc.inputs)
			_, err := f.RunOAuth(context.Background())
			if err == nil || !strings.Contains(err.Error(), tc.code) || isExpiredSession(err) != tc.expired {
				t.Fatalf("wrong stop reason: %v", err)
			}
			s.mu.Lock()
			defer s.mu.Unlock()
			if s.counts["/api/accounts/add-phone/send"] != tc.sends || s.counts["/api/accounts/workspace/select"] != 0 {
				t.Fatalf("operation repeated after terminal error: %v", s.counts)
			}
		})
	}
}
func TestLegacyPhoneCheckpointReopensPageWithoutRepeatingSend(t *testing.T) {
	f, s := newLegacyPhoneFlow(t, "normal", [][2]string{{"phone", "+12025550123"}})
	originalInput := f.Options.Input
	f.Options.Input = func(ctx context.Context, stage, prompt string) (string, error) {
		if stage == "sms_otp" {
			return "", context.Canceled
		}
		return originalInput(ctx, stage, prompt)
	}
	if _, err := f.RunOAuth(context.Background()); !errors.Is(err, context.Canceled) {
		t.Fatalf("wanted interruption, got %v", err)
	}
	if f.checkpoint.Stage != "phone_otp" {
		t.Fatal("SMS checkpoint missing")
	}
	b, _ := json.Marshal(f.checkpoint)
	var cp Checkpoint
	if err := json.Unmarshal(b, &cp); err != nil {
		t.Fatal(err)
	}
	second := NewFlow(NewClient(f.Client.Transport, nil, ""), f.Options)
	second.Options.Input = func(_ context.Context, stage, prompt string) (string, error) {
		if stage != "sms_otp" {
			t.Errorf("resumed stage is %s", stage)
		}
		return "123456", nil
	}
	if err := second.Restore(cp); err != nil {
		t.Fatal(err)
	}
	if _, err := second.ResumeOAuth(context.Background(), cp); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.counts["/add-phone"] != 2 || s.counts["/api/accounts/add-phone/send"] != 2 || s.counts["/api/accounts/session/select"] != 1 {
		t.Fatalf("resume replayed selection/send: %v", s.counts)
	}
}

func TestStructuredPhoneEventsOverrideLegacyHookAndClearMetadata(t *testing.T) {
	f, _ := newLegacyPhoneFlow(t, "normal", [][2]string{{"phone", "+12025550123"}, {"sms_otp", "000000"}, {"sms_otp", "p"}, {"phone", "+12025550124"}, {"sms_otp", "123456"}})
	legacyCalls := 0
	events := map[string][]map[string]any{}
	f.Options.Event = func(string, string, string) { legacyCalls++ }
	f.Options.EventData = func(stage, status, prompt string, data map[string]any) {
		if data != nil {
			events[stage] = append(events[stage], data)
		}
	}
	if _, err := f.RunOAuth(context.Background()); err != nil {
		t.Fatal(err)
	}
	if legacyCalls != 0 {
		t.Fatal("both event hooks fired")
	}
	if len(events["phone_sent"]) != 2 || events["phone_sent"][0]["currentPhone"] != "+12025550123" || events["phone_sent"][1]["currentPhone"] != "+12025550124" {
		t.Fatal("phone acceptance metadata missing")
	}
	if len(events["sms_otp_rejected"]) != 1 || events["sms_otp_rejected"][0]["currentPhone"] != "+12025550123" || events["sms_otp_rejected"][0]["phoneError"] == "" {
		t.Fatal("SMS rejection must retain phone with safe error")
	}
	if events["phone_changed"][0]["currentPhone"] != "" || events["phone_changed"][0]["phoneError"] != "" || events["phone_verified"][0]["currentPhone"] != "+12025550124" || events["phone_verified"][0]["phoneError"] != "" {
		t.Fatal("phone metadata not cleared after change/verification")
	}
	b, _ := json.Marshal(events)
	if strings.Contains(string(b), "000000") || strings.Contains(string(b), "123456") {
		t.Fatal("phone events exposed an OTP")
	}
}
