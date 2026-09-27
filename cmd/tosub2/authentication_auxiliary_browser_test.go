package main

import (
	"context"
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

	"tosub2-go-port/internal/browser"
	"tosub2-go-port/internal/config"
	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/protocol"
	"tosub2-go-port/internal/store"
)

func TestProductionAuxiliaryFlowsKeepInitialBrowserSession(t *testing.T) {
	if os.Getenv("TOSUB2_BROWSER_TEST") != "1" {
		t.Skip("set TOSUB2_BROWSER_TEST=1 for complete local auxiliary Chromium flows")
	}
	chrome := os.Getenv("CHROME_PATH")
	if chrome == "" {
		chrome = browser.DefaultChrome()
	}
	if chrome == "" {
		t.Skip("Chromium not installed; set CHROME_PATH")
	}
	for _, operation := range []string{"setup-2fa", "add-password"} {
		t.Run(operation, func(t *testing.T) {
			clearCLISensitiveEnvironment(t)
			t.Setenv("TOSUB2_BROWSER_HEADLESS", "1")
			const email = "auxiliary-identity@example.test"
			const originalPassword = "Fixture-Original-Password-123!"
			const addedPassword = "Fixture-New-Password-456!"
			const seed = "JBSWY3DPEHPK3PXP"
			var mu sync.Mutex
			ua, device := "", ""
			counts := map[string]int{}
			var base string
			page := func(w http.ResponseWriter) {
				w.Header().Set("Content-Type", "text/html")
				io.WriteString(w, "<!doctype html><html><head></head><body>authorized fixture</body></html>")
			}
			jsonOut := func(w http.ResponseWriter, v any) {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(v)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				cookie, cookieErr := r.Cookie("oai-did")
				mu.Lock()
				counts[r.URL.Path]++
				if ua == "" {
					ua = r.UserAgent()
					if cookieErr == nil {
						device = cookie.Value
					}
				}
				wantUA, wantDevice := ua, device
				mu.Unlock()
				if !strings.Contains(r.UserAgent(), "HeadlessChrome/") || r.UserAgent() != wantUA || cookieErr != nil || cookie.Value != wantDevice || wantDevice == "" {
					t.Error("auxiliary flow changed its actual browser identity")
				}
				if r.URL.Path != "/" {
					if c, e := r.Cookie("fixture-native-session"); e != nil || c.Value != "same-context" {
						t.Error("authentication cookie did not remain in the initial browser context")
					}
				}
				switch r.URL.Path {
				case "/":
					http.SetCookie(w, &http.Cookie{Name: "fixture-native-session", Value: "same-context", Path: "/", HttpOnly: true})
					if r.URL.Query().Get("action") == "enable" {
						w.Header().Set("Content-Type", "text/html")
						io.WriteString(w, "<script>{\"accessToken\":\"fixture-mfa-access-token-at-least-twenty-characters\"}</script>")
						return
					}
					page(w)
				case "/api/auth/providers":
					jsonOut(w, map[string]any{})
				case "/api/auth/csrf":
					http.SetCookie(w, &http.Cookie{Name: "__Host-next-auth.csrf-token", Value: "fixture-csrf", Path: "/", Secure: true})
					jsonOut(w, map[string]any{"csrfToken": "fixture-csrf"})
				case "/api/auth/signin/openai":
					if r.URL.Query().Get("ext-oai-did") != wantDevice {
						t.Error("signin device did not match first browser request")
					}
					path := "/log-in/password"
					if r.URL.Query().Get("post_login_add_password") == "true" {
						path = "/reset-password/new-password"
					}
					jsonOut(w, map[string]any{"url": base + path})
				case "/log-in/password", "/reset-password/new-password":
					page(w)
				case "/loader.js":
					w.Header().Set("Content-Type", "application/javascript")
					io.WriteString(w, "(()=>{let s=document.createElement('script');s.src='/sdk/current.js';document.head.appendChild(s)})()")
				case "/sdk/current.js":
					w.Header().Set("Content-Type", "application/javascript")
					io.WriteString(w, "window.SentinelSDK={token:async flow=>JSON.stringify({p:'fixture-proof',t:null,c:'fixture',id:localStorage.getItem('oai-did'),flow}),sessionObserverToken:async flow=>JSON.stringify({so:'fixture-observer',c:'fixture',id:localStorage.getItem('oai-did'),flow})}")
				case "/api/accounts/password/verify", "/api/accounts/password/add":
					var body map[string]any
					_ = json.NewDecoder(r.Body).Decode(&body)
					var proof map[string]any
					_ = json.Unmarshal([]byte(r.Header.Get("Openai-Sentinel-Token")), &proof)
					expected := originalPassword
					flow := "password_verify"
					if r.URL.Path == "/api/accounts/password/add" {
						expected = addedPassword
						flow = "password_reset"
					}
					if body["password"] != expected || proof["id"] != wantDevice || proof["flow"] != flow {
						t.Error("auxiliary credential mutation lost its SDK/device binding")
					}
					http.SetCookie(w, &http.Cookie{Name: "__Secure-next-auth.session-token", Value: "synthetic-session", Path: "/", Secure: true, HttpOnly: true})
					jsonOut(w, map[string]any{"continue_url": base + "/", "page": map[string]any{"type": "external_url"}})
				case "/backend-api/accounts/mfa_info":
					mu.Lock()
					activated := counts["/backend-api/accounts/mfa/user/activate_enrollment"] > 0
					mu.Unlock()
					jsonOut(w, map[string]any{"mfa_enabled_v2": activated, "factors": map[string]any{"totp": []any{map[string]any{"factor_type": "totp", "id": "fixture"}}}})
				case "/backend-api/accounts/mfa/enroll":
					if r.Header.Get("Oai-Device-Id") != wantDevice || !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer fixture-mfa-") {
						t.Error("MFA enrollment left initial identity")
					}
					jsonOut(w, map[string]any{"secret": seed, "session_id": "fixture-enrollment"})
				case "/backend-api/accounts/mfa/user/activate_enrollment":
					var body map[string]any
					_ = json.NewDecoder(r.Body).Decode(&body)
					code, _ := body["code"].(string)
					if len(code) != 6 || body["session_id"] != "fixture-enrollment" || r.Header.Get("Oai-Device-Id") != wantDevice {
						t.Error("MFA activation binding changed")
					}
					jsonOut(w, map[string]any{"success": true})
				case "/favicon.ico":
					w.WriteHeader(204)
				default:
					t.Error("unexpected auxiliary fixture route", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			base = server.URL
			cfg := config.Default()
			cfg.OutputRoot = t.TempDir()
			cfg.ChromePath = chrome
			cfg.ChatGPTBase = base
			cfg.AuthBase = base
			cfg.SentinelSDKURL = base + "/loader.js"
			cfg.ProxyURL = ""
			cfg.ProxyURLs = nil
			cfg.SameProxyRetries = 0
			st, err := store.New(cfg.OutputRoot)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
			defer cancel()
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			manager := jobs.NewManager(ctx, 1, st, logger)
			defer manager.Shutdown(context.Background())
			vault := credentials.NewMemoryStore()
			job, err := manager.Submit(jobs.Spec{ID: "auxiliary-identity", Email: email, Operation: operation, Input: map[string]any{"password": originalPassword, "newPassword": addedPassword}})
			if err != nil {
				t.Fatal(err)
			}
			var events []jobs.JobEvent
			err = makeJobRunnerWithCredentials(cfg, manager, logger, vault, true)(ctx, job, func(event jobs.JobEvent) { events = append(events, event) })
			if err != nil {
				t.Fatal("complete local auxiliary flow failed", err)
			}
			saved, err := vault.Load(email)
			if err != nil {
				t.Fatal(err)
			}
			cp, err := protocol.ReadCheckpoint(filepath.Join(cfg.OutputRoot, job.ID, "login-checkpoint.json"))
			if err != nil {
				t.Fatal(err)
			}
			binding, present, err := protocol.CheckpointTransportSession(cp)
			if err != nil || !present || !binding.BrowserBound || binding.TLSProfile != "chromium-native" {
				t.Fatal("auxiliary checkpoint lost native binding", err)
			}
			mu.Lock()
			count := map[string]int{}
			for k, v := range counts {
				count[k] = v
			}
			mu.Unlock()
			if count["/api/accounts/password/verify"] != 1 || count["/sdk/current.js"] < 1 {
				t.Fatal("auxiliary fixture skipped fresh login or real SDK", count)
			}
			if operation == "setup-2fa" && (saved.TOTPSecret != seed || count["/backend-api/accounts/mfa/enroll"] != 1 || count["/backend-api/accounts/mfa/user/activate_enrollment"] != 1) {
				t.Fatal("TOTP enrollment did not complete once")
			}
			if operation == "add-password" && (saved.Password != addedPassword || count["/api/accounts/password/add"] != 1) {
				t.Fatal("password addition did not complete once")
			}
			for _, event := range events {
				if event.Stage == "browser_bound_transport" && event.Data["tlsProfile"] != "chromium-native" {
					t.Error("public task event reported the configured Go profile as actual browser TLS")
				}
			}
		})
	}
}
