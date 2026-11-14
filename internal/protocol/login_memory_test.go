package protocol

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"tosub2-go-port/internal/transport"
)

func TestFreshLoginIgnoresLegacyFilesAndNeverProbesCookies(t *testing.T) {
	h := newRecoveryHarness(t)
	root := filepath.Dir(h.flow.Options.OutputPath)
	oldPath := filepath.Join(root, "login-checkpoint.json")
	old := []byte(`{"authorization_method":"bps","stage":"callback_ready","cookies":[{"name":"old-auth","value":"OLD-COOKIE-CANARY"}],"oauth":{"codeVerifier":"OLD-PKCE-CANARY","callbackUrl":"https://old.invalid/?code=old"}}`)
	if err := os.WriteFile(oldPath, old, 0600); err != nil {
		t.Fatal(err)
	}
	h.flow.Options.GrantPath = filepath.Join(root, "oauth-grant.json")
	var calls []string
	h.intercept = func(_ context.Context, r transport.Request) (*transport.Response, error, bool) {
		path := pathOf(r.URL)
		calls = append(calls, path)
		if strings.Contains(r.Headers.Get("Cookie"), "OLD-COOKIE-CANARY") {
			t.Fatal("old cookie used")
		}
		if path == "/api/auth/session" {
			t.Fatal("login added a cookie probe")
		}
		if path == "/oauth/token" {
			entries, err := os.ReadDir(root)
			if err != nil || len(entries) != 1 || entries[0].Name() != "login-checkpoint.json" {
				t.Fatal("unfinished login wrote recovery material", err)
			}
		}
		return nil, nil, false
	}
	if _, err := h.flow.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	expected := []string{"/", "/api/auth/providers", "/api/auth/csrf", "/api/auth/signin/openai", "/authorize", "/email-verification", "/api/accounts/email-otp/validate", "/web-callback", "/oauth/authorize", "/oauth/token"}
	if !reflect.DeepEqual(calls, expected) {
		t.Fatal("fresh login sequence changed", calls)
	}
	preserved, _ := os.ReadFile(oldPath)
	if string(preserved) != string(old) {
		t.Fatal("new operation consumed old login input")
	}
	entries, _ := os.ReadDir(root)
	if len(entries) != 2 {
		t.Fatal("unexpected login files", len(entries))
	}
	data, err := os.ReadFile(h.flow.Options.OutputPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{"OLD-COOKIE-CANARY", "OLD-PKCE-CANARY", "fixture-new-session", "codeVerifier", "callbackUrl"} {
		if strings.Contains(string(data), marker) {
			t.Fatal("login material saved", marker)
		}
	}
}

func TestCanceledInputCannotResumeAfterNewOperation(t *testing.T) {
	h := newRecoveryHarness(t)
	h.flow.Options.GrantPath = filepath.Join(filepath.Dir(h.flow.Options.OutputPath), "oauth-grant.json")
	h.flow.Options.Input = func(context.Context, string, string) (string, error) { return "", context.Canceled }
	if _, err := h.flow.Run(t.Context()); !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation lost", err)
	}
	entries, _ := os.ReadDir(filepath.Dir(h.flow.Options.OutputPath))
	if len(entries) != 0 {
		t.Fatal("canceled login persisted continuation")
	}
	h.flow.Options.Input = func(context.Context, string, string) (string, error) { return "123456", nil }
	if _, err := h.flow.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	if h.loginStarts != 2 || h.oauthRequests != 1 || h.tokenRequests != 1 {
		t.Fatal("canceled login was continued", h.loginStarts, h.oauthRequests, h.tokenRequests)
	}
}

func TestIssuedGrantSurvivesCancellationWithoutSavingCode(t *testing.T) {
	h := newRecoveryHarness(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	h.flow.Options.GrantPath = filepath.Join(filepath.Dir(h.flow.Options.OutputPath), "oauth-grant.json")
	h.intercept = func(_ context.Context, r transport.Request) (*transport.Response, error, bool) {
		if pathOf(r.URL) == "/oauth/token" {
			cancel()
		}
		return nil, nil, false
	}
	if _, err := h.flow.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation after issued grant lost", err)
	}
	grant, err := ReadSavedOAuthGrant(h.flow.Options.GrantPath)
	if err != nil {
		t.Fatal("issued grant lost", err)
	}
	data, _ := json.Marshal(grant)
	for _, key := range []string{"cookies", "state", "codeVerifier", "callbackUrl", "web"} {
		if strings.Contains(string(data), `"`+key+`"`) {
			t.Fatal("login material in grant", key)
		}
	}
	before := h.tokenRequests
	if _, err := FinalizeSavedOAuthGrant(t.Context(), grant, h.flow.Options); err != nil {
		t.Fatal("local completion failed", err)
	}
	if h.tokenRequests != before || h.loginStarts != 1 {
		t.Fatal("local completion replayed network")
	}
}
