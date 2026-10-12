package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"tosub2-go-port/internal/config"
	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/protocol"
	"tosub2-go-port/internal/store"
	"tosub2-go-port/internal/transport"
)

func TestAuthenticationCheckpointRejectsDifferentOriginBeforeNetwork(t *testing.T) {
	cfg := config.Default()
	cfg.ChatGPTBase = "https://chat.fixture"
	cfg.AuthBase = "https://auth.fixture"
	cp := protocol.Checkpoint{Version: 1, Stage: "email_verified", Email: "fixture@example.test", Cookies: []transport.Cookie{}, ChatGPTBase: cfg.ChatGPTBase, AuthBase: cfg.AuthBase}
	if err := validateAuthenticationCheckpoint(cp, cfg, cp.Email); err != nil {
		t.Fatal(err)
	}
	for _, mismatch := range []string{"email", "auth", "chat"} {
		t.Run(mismatch, func(t *testing.T) {
			changed := cp
			switch mismatch {
			case "email":
				changed.Email = "other@example.test"
			case "auth":
				changed.AuthBase = "https://other-auth.fixture"
			case "chat":
				changed.ChatGPTBase = "https://other-chat.fixture"
			}
			if err := validateAuthenticationCheckpoint(changed, cfg, cp.Email); err == nil {
				t.Fatal("checkpoint account/origin mismatch was accepted")
			}
		})
	}
}

func runCheckpointFixture(t *testing.T, cfg config.Config, cp protocol.Checkpoint, op string) error {
	t.Helper()
	st, err := store.New(cfg.OutputRoot)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	manager := jobs.NewManager(ctx, 1, st, logger)
	defer manager.Shutdown(context.Background())
	job, err := manager.Submit(jobs.Spec{ID: "authentication-recovery", Email: cp.Email, Operation: op})
	if err != nil {
		t.Fatal(err)
	}
	return makeJobRunnerWithCredentials(cfg, manager, logger, credentials.NewMemoryStore(), true)(ctx, job, func(jobs.JobEvent) {})
}

func TestRemovedBrowserCheckpointPreservedWithoutNetwork(t *testing.T) {
	for _, stage := range []string{"web_password", "web_email_otp", "web_mfa", "phone_required", "phone_otp", "oauth_continue"} {
		t.Run(stage, func(t *testing.T) {
			clearCLISensitiveEnvironment(t)
			cfg := config.Default()
			cfg.OutputRoot = t.TempDir()
			cfg.ChatGPTBase = "http://127.0.0.1:1"
			cfg.AuthBase = cfg.ChatGPTBase
			cp := protocol.Checkpoint{Version: 1, Stage: stage, Email: "recovery@example.test", ChatGPTBase: cfg.ChatGPTBase, AuthBase: cfg.AuthBase, Cookies: []transport.Cookie{}, Web: map[string]any{"deviceId": "fixture-device"}}
			protocol.BindTransportSession(&cp, "chromium-native", "retired-browser-UA", "", true)
			path := filepath.Join(cfg.OutputRoot, "authentication-recovery", "login-checkpoint.json")
			if err := protocol.WriteCheckpoint(path, cp); err != nil {
				t.Fatal(err)
			}
			before, _ := os.ReadFile(path)
			err := runCheckpointFixture(t, cfg, cp, "full")
			if err == nil || !strings.Contains(err.Error(), "CHECKPOINT_SESSION_MISMATCH") {
				t.Fatal("removed browser checkpoint did not stop before network", err)
			}
			after, _ := os.ReadFile(path)
			if !bytes.Equal(before, after) {
				t.Fatal("unverified checkpoint was modified")
			}
		})
	}
}

func TestCallbackCheckpointExchangesPKCEWithoutMigratingCookies(t *testing.T) {
	for _, fixture := range []struct {
		name           string
		invalid        bool
		retiredProfile bool
	}{
		{name: "valid"},
		{name: "state-mismatch", invalid: true},
		{name: "retired-web-profile", retiredProfile: true},
		{name: "retired-web-profile-state-mismatch", retiredProfile: true, invalid: true},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			clearCLISensitiveEnvironment(t)
			const email = "callback-recovery@example.test"
			const grantProfile = "chrome_120"
			grantIdentity := transport.ProfileFor(grantProfile)
			var mu sync.Mutex
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				calls++
				mu.Unlock()
				if r.URL.Path != "/oauth/token" || r.Method != "POST" {
					t.Error("callback recovery performed a browser/authentication probe")
				}
				if r.Header.Get("Cookie") != "" {
					t.Error("callback recovery transferred legacy authentication cookies")
				}
				if r.UserAgent() != grantIdentity.UserAgent || r.Header.Get("Sec-CH-UA") != grantIdentity.ClientHints {
					t.Error("cookie-independent grant inherited the retired web identity instead of the configured profile")
				}
				_ = r.ParseForm()
				if r.Form.Get("code") != "fixture-unconsumed-code" || r.Form.Get("code_verifier") != "fixture-pkce-verifier" {
					t.Error("callback PKCE grant changed")
				}
				claims, _ := json.Marshal(map[string]any{"email": email, "sid": "fixture-recovery-account"})
				_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "fixture-access", "refresh_token": "fixture-refresh", "id_token": "e30." + base64.RawURLEncoding.EncodeToString(claims) + ".fixture"})
			}))
			defer server.Close()
			cfg := config.Default()
			cfg.OutputRoot = t.TempDir()
			cfg.ChatGPTBase = server.URL
			cfg.AuthBase = server.URL
			cfg.TLSProfile = grantProfile
			cfg.SameProxyRetries = 0
			cfg.ProxyURL = ""
			cfg.ProxyURLs = nil
			cp := protocol.Checkpoint{Version: 1, Stage: "callback_ready", Email: email, ChatGPTBase: server.URL, AuthBase: server.URL, Cookies: []transport.Cookie{{Name: "legacy-secret-cookie", Value: "must-never-transfer", Domain: "127.0.0.1", Path: "/", HostOnly: true}}, OAuth: map[string]any{"state": "fixture-state", "codeVerifier": "fixture-pkce-verifier", "callbackUrl": cfg.CodexRedirectURI + "?code=fixture-unconsumed-code&state=fixture-state"}}
			protocol.BindTransportSession(&cp, "chromium-native", "retired-browser-user-agent", "", true)
			if fixture.retiredProfile {
				protocol.BindTransportSession(&cp, "retired-legacy-web-profile", "retired-go-user-agent", "", false)
			}
			if fixture.invalid {
				cp.OAuth["state"] = "wrong-state"
			}
			path := filepath.Join(cfg.OutputRoot, "authentication-recovery", "login-checkpoint.json")
			if err := protocol.WriteCheckpoint(path, cp); err != nil {
				t.Fatal(err)
			}
			before, _ := os.ReadFile(path)
			err := runCheckpointFixture(t, cfg, cp, "resume")
			mu.Lock()
			count := calls
			mu.Unlock()
			if fixture.invalid {
				if err == nil || count != 0 {
					t.Fatal("mismatched callback reached token endpoint", err, count)
				}
				after, _ := os.ReadFile(path)
				if !bytes.Equal(before, after) {
					t.Fatal("invalid callback changed checkpoint")
				}
				return
			}
			if err != nil || count != 1 {
				t.Fatal("cookie-independent callback recovery failed", err, count)
			}
			if _, err = os.Stat(filepath.Join(cfg.OutputRoot, "authentication-recovery", "sub2api-import-oauth.json")); err != nil {
				t.Fatal("token grant output missing", err)
			}
		})
	}
}
