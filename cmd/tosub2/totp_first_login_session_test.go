package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"tosub2-go-port/internal/config"
	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/protocol"
)

func TestTOTPFirstLoginNeverPersistsSession(t *testing.T) {
	for _, operation := range []string{"reset-2fa", "setup-2fa"} {
		for _, source := range []string{"fresh", "completed-bps", "bps-checkpoint"} {
			t.Run(operation+"/"+source, func(t *testing.T) {
				runTOTPFirstLoginFixture(t, operation, source)
			})
		}
	}
}

func runTOTPFirstLoginFixture(t *testing.T, operation, source string) {
	t.Helper()
	clearCLISensitiveEnvironment(t)
	factor := ""
	if operation == "reset-2fa" {
		factor = "old-factor"
	}
	remote := &sessionAuditRemote{t: t, sessionValid: true, factor: factor}
	mfaCalls := make(chan string, 8)
	var signins, passwords, callbacks atomic.Int32
	var authenticated atomic.Bool
	var cfg config.Config
	var base string
	resultPath := ""
	assertOrdinarySession := func() {
		t.Helper()
		if source != "bps-checkpoint" {
			assertNoTaskCheckpoint(t, filepath.Join(filepath.Dir(resultPath), "login-checkpoint.json"))
		}
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.Header.Get("Cookie"), "prior-bps-cookie") || strings.Contains(r.Header.Get("Authorization"), "prior-bps-token") {
			t.Error("fresh ChatGPT login imported prior BPS authentication materials")
		}
		write := func(v any) {
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(v); err != nil {
				t.Error(err)
			}
		}
		switch r.URL.Path {
		case "/":
			if !authenticated.Load() || r.URL.Query().Get("action") == "" {
				http.SetCookie(w, &http.Cookie{Name: "oai-did", Value: "first-login-fixture-device", Path: "/"})
				fmt.Fprint(w, "<html>local initial login page</html>")
				return
			}
		case "/api/auth/session":
			if !authenticated.Load() {
				write(map[string]any{})
				return
			}
			write(map[string]any{"accessToken": "synthetic-first-login-access-token-at-least-twenty-characters", "account": map[string]any{"id": "synthetic-account"}, "user": map[string]any{"email": sessionAuditEmail}})
			return
		case "/api/auth/providers":
			write(map[string]any{})
			return
		case "/api/auth/csrf":
			write(map[string]any{"csrfToken": "first-login-csrf"})
			return
		case "/api/auth/signin/openai":
			signins.Add(1)
			if r.URL.Query().Get("login_hint") != sessionAuditEmail {
				t.Error("first login changed account binding")
			}
			http.SetCookie(w, &http.Cookie{Name: "first-reset-only", Value: "first-reset-cookie-canary", Path: "/", HttpOnly: true})
			write(map[string]any{"url": base + "/log-in/password"})
			return
		case "/log-in/password":
			fmt.Fprint(w, "<html>local password page</html>")
			return
		case "/loader.js":
			w.Header().Set("Content-Type", "application/javascript")
			fmt.Fprintf(w, "const s=document.createElement('script');s.src=%q;document.head.appendChild(s);", base+"/sentinel/fixture/sdk.js")
			return
		case "/sentinel/fixture/sdk.js":
			w.Header().Set("Content-Type", "application/javascript")
			fmt.Fprint(w, "window.SentinelSDK={token:async flow=>{const r=await fetch('/fixture-proof?flow='+encodeURIComponent(flow));const p=await r.json();return JSON.stringify({p:p.proof,t:null,c:'fixture-challenge',id:document.cookie.split('oai-did=')[1].split(';')[0],flow})},sessionObserverToken:async flow=>JSON.stringify({so:'fixture-observer',c:'fixture-challenge',id:document.cookie.split('oai-did=')[1].split(';')[0],flow})};")
			return
		case "/fixture-proof":
			if c, err := r.Cookie("first-reset-only"); err != nil || c.Value != "first-reset-cookie-canary" {
				t.Error("Sentinel lost first-login temporary session")
			}
			write(map[string]any{"proof": "local-first-login-proof"})
			return
		case "/api/accounts/password/verify":
			passwords.Add(1)
			var body, proof map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			_ = json.Unmarshal([]byte(r.Header.Get("Openai-Sentinel-Token")), &proof)
			if body["password"] != "synthetic-password" || proof["p"] != "local-first-login-proof" {
				t.Error("first login bypassed password or local Sentinel")
			}
			http.SetCookie(w, &http.Cookie{Name: "first-password-progress", Value: "password-accepted-canary", Path: "/", HttpOnly: true})
			write(map[string]any{"continue_url": base + "/api/auth/callback/openai?code=first-one-use"})
			return
		case "/api/auth/callback/openai":
			callbacks.Add(1)
			authenticated.Store(true)
			http.SetCookie(w, &http.Cookie{Name: "audit-session-token", Value: sessionAuditCookie, Path: "/", HttpOnly: true})
			http.Redirect(w, r, base+"/", http.StatusFound)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/backend-api/accounts/") {
			assertOrdinarySession()
			mfaCalls <- r.Method + " " + r.URL.Path
		}
		remote.serveHTTP(w, r)
	}))
	defer upstream.Close()
	base = upstream.URL
	cfg = sessionAuditConfig(t.TempDir(), base)
	cfg.SentinelSDKURL, cfg.StartupToken = base+"/loader.js", "synthetic-first-login-console-token"
	resultPath = filepath.Join(cfg.OutputRoot, sessionAuditJobID, "totp-setup-result.json")
	vault := &resetRunnerVault{MemoryStore: credentials.NewMemoryStore()}
	m, _, logger := sessionAuditManager(t, cfg, vault)
	method := protocol.AuthorizationMethodChatGPT
	if source != "fresh" {
		method = protocol.AuthorizationMethodBPS
	}
	if _, err := m.Submit(jobs.Spec{ID: sessionAuditJobID, Email: sessionAuditEmail, ImportOnly: true, Input: map[string]any{"password": "synthetic-password", "totpSecret": sessionAuditOld, "authorizationMethod": method}}); err != nil {
		t.Fatal(err)
	}
	if source == "bps-checkpoint" {
		checkpoint := map[string]any{"version": 1, "stage": "email_verified", "authorization_method": "bps", "email": sessionAuditEmail, "cookies": []any{map[string]any{"name": "prior-bps-cookie", "value": "synthetic-stale-bps-session"}}, "oauth": map[string]any{"clientId": protocol.DefaultBPSClientID, "redirectUri": protocol.DefaultBPSRedirectURI}}
		if err := protocol.WritePrivateJSON(filepath.Join(cfg.OutputRoot, sessionAuditJobID, "login-checkpoint.json"), checkpoint); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := m.QueueOperation(sessionAuditJobID, operation, nil); err != nil {
		t.Fatal(err)
	}
	if err := sessionAuditExecute(t, cfg, m, logger, vault); err != nil {
		t.Fatal(err)
	}
	assertOrdinarySession()
	assertNoAccountSessions(t, cfg.OutputRoot)
	if signins.Load() != 1 || passwords.Load() != 1 || callbacks.Load() != 1 {
		t.Fatal("initial authentication repeated or unexpected email challenge")
	}
	if operation == "reset-2fa" {
		journal := sessionAuditJournal(t, cfg)
		for _, key := range []string{"recovery_checkpoint", "preparation_checkpoint", "initial_login_progress", "cookie_state", "cookies"} {
			if _, exists := journal[key]; exists {
				t.Fatalf("reset journal retains dedicated session state: %s", key)
			}
		}
	}
	state := remote.snapshot()
	wantDisable := 0
	wantCalls := []string{"GET /backend-api/accounts/mfa_info"}
	if operation == "reset-2fa" {
		wantDisable = 1
		wantCalls = append(wantCalls, "POST /backend-api/accounts/mfa/user/disable_in_house")
	}
	wantCalls = append(wantCalls, "POST /backend-api/accounts/mfa/enroll", "POST /backend-api/accounts/mfa/user/activate_enrollment")
	calls := make([]string, 0, len(mfaCalls))
	for len(mfaCalls) > 0 {
		calls = append(calls, <-mfaCalls)
	}
	if state.infoReads != 1 || state.disableCalls != wantDisable || state.enrollCalls != 1 || state.activateCalls != 1 || !reflect.DeepEqual(calls, wantCalls) {
		t.Fatalf("fresh-login 2FA repeated or reordered requests: %v", calls)
	}
	saved, err := vault.Load(sessionAuditEmail)
	if err != nil || saved.TOTPSecret != sessionAuditNew || saved.TOTPResetPending {
		t.Fatal("new factor not saved", err)
	}
}
