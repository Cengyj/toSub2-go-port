package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
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
	"tosub2-go-port/internal/transport"
)

// Exercise the production runner, real Chromium Sentinel loader, checkpoint,
// manager input and protected-store callbacks. All account and server data is
// synthetic; no real identity, credential service or external SDK is contacted.
func TestProductionRunnerDoesNotReplayRejectedPassword(t *testing.T) {
	if os.Getenv("TOSUB2_BROWSER_TEST") != "1" {
		t.Skip("set TOSUB2_BROWSER_TEST=1 for actual runner/Chromium password regression")
	}
	chrome := os.Getenv("CHROME_PATH")
	if chrome == "" {
		chrome = browser.DefaultChrome()
	}
	if chrome == "" {
		t.Skip("Chromium not installed; set CHROME_PATH")
	}
	for _, source := range []string{"import", "vault"} {
		t.Run(source, func(t *testing.T) {
			clearCLISensitiveEnvironment(t)
			t.Setenv("TOSUB2_BROWSER_HEADLESS", "1")
			const email = "password-runner@example.test"
			const device = "password-runner-fixture-device"
			const initial = " \tSynthetic-Rejected9! "
			const corrected = " \tSynthetic-Corrected9! "
			var mu sync.Mutex
			var submitted []string
			var oauthState string
			posted := make(chan string, 8)
			mux := http.NewServeMux()
			server := httptest.NewServer(mux)
			defer server.Close()
			jsonOut := func(w http.ResponseWriter, status int, value any) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				_ = json.NewEncoder(w).Encode(value)
			}
			page := func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/html")
				_, _ = io.WriteString(w, "<!doctype html><html><head></head><body>authorized local fixture</body></html>")
			}
			mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/" {
					t.Errorf("unexpected fixture route %s", r.URL.Path)
					http.NotFound(w, r)
					return
				}
				page(w, r)
			})
			mux.HandleFunc("/log-in/password", page)
			mux.HandleFunc("/favicon.ico", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
			mux.HandleFunc("/loader.js", func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/javascript")
				_, _ = io.WriteString(w, "(()=>{const s=document.createElement('script');s.src='/sdk/v1.js';document.head.appendChild(s)})()")
			})
			mux.HandleFunc("/sdk/v1.js", func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/javascript")
				_, _ = io.WriteString(w, "window.SentinelSDK={token:async flow=>JSON.stringify({p:'fixture-proof',t:null,c:'fixture-challenge',id:localStorage.getItem('oai-did'),flow}),sessionObserverToken:async flow=>JSON.stringify({so:'fixture-observer',c:'fixture',id:localStorage.getItem('oai-did'),flow})}")
			})
			mux.HandleFunc("/api/accounts/password/verify", func(w http.ResponseWriter, r *http.Request) {
				var body map[string]string
				if r.Method != http.MethodPost || json.NewDecoder(r.Body).Decode(&body) != nil {
					t.Error("invalid password verification request")
				}
				var proof map[string]any
				if json.Unmarshal([]byte(r.Header.Get("Openai-Sentinel-Token")), &proof) != nil || proof["flow"] != "password_verify" || proof["id"] != device || !strings.Contains(r.UserAgent(), "Chrome/") {
					t.Error("password request lost the actual browser/Sentinel identity")
				}
				candidate := body["password"]
				mu.Lock()
				submitted = append(submitted, candidate)
				mu.Unlock()
				posted <- candidate
				if candidate != corrected {
					jsonOut(w, http.StatusUnauthorized, map[string]any{"error": map[string]any{"code": "invalid_password"}})
					return
				}
				jsonOut(w, http.StatusOK, map[string]any{"continue_url": server.URL + "/", "page": map[string]any{"type": "external_url"}})
			})
			mux.HandleFunc("/oauth/authorize", func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				oauthState = r.URL.Query().Get("state")
				mu.Unlock()
				w.Header().Set("Content-Type", "text/html")
				_, _ = io.WriteString(w, "<input name=\"session_id\" value=\"us_password_fixture_session_1234567890\">")
			})
			mux.HandleFunc("/api/accounts/session/select", func(w http.ResponseWriter, _ *http.Request) {
				mu.Lock()
				state := oauthState
				mu.Unlock()
				callback := protocol.DefaultCodexRedirectURI + "?" + url.Values{"code": {"synthetic-password-grant"}, "state": {state}}.Encode()
				jsonOut(w, http.StatusOK, map[string]any{"continue_url": callback, "page": map[string]any{"type": "external_url"}})
			})
			mux.HandleFunc("/oauth/token", func(w http.ResponseWriter, _ *http.Request) {
				claims, _ := json.Marshal(map[string]any{"email": email, "https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "synthetic-password-account"}})
				idToken := "e30." + base64.RawURLEncoding.EncodeToString(claims) + ".fixture"
				jsonOut(w, http.StatusOK, map[string]any{"access_token": "synthetic-password-access", "refresh_token": "synthetic-password-refresh", "id_token": idToken, "expires_in": 3600})
			})
			cfg := config.Default()
			cfg.OutputRoot, cfg.ChatGPTBase, cfg.AuthBase = t.TempDir(), server.URL, server.URL
			cfg.ChromePath, cfg.SentinelSDKURL, cfg.BrowserEnabled = chrome, server.URL+"/loader.js", true
			cfg.CodexRedirectURI = protocol.DefaultCodexRedirectURI
			cfg.ProxyURL, cfg.ProxyURLs = "", nil
			cfg.SameProxyRetries, cfg.SameProxyRiskRetries, cfg.RequestTimeoutMS = 0, 0, 5000
			checkpoint := filepath.Join(cfg.OutputRoot, "password-fixture", "login-checkpoint.json")
			cp := protocol.Checkpoint{Version: 1, Email: email, Stage: "web_password", ChatGPTBase: server.URL, AuthBase: server.URL, UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano), Cookies: []transport.Cookie{}, Web: map[string]any{"deviceId": device}, OAuth: map[string]any{"pageURL": server.URL + "/log-in/password", "loginMethod": "password"}}
			writeBrowserCheckpointFixture(t, chrome, checkpoint, cp)
			st, err := store.New(cfg.OutputRoot)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
			defer cancel()
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			manager := jobs.NewManager(ctx, 1, st, logger)
			vault := credentials.NewMemoryStore()
			manager.SetRunner(makeJobRunnerWithCredentials(cfg, manager, logger, vault, true))
			input := map[string]any{}
			if source == "import" {
				input["password"] = initial
			} else if err = vault.Save(email, credentials.Credentials{Password: initial}); err != nil {
				t.Fatal(err)
			}
			job, err := manager.Submit(jobs.Spec{ID: "password-fixture", Email: email, Operation: "full", Input: input})
			if err != nil {
				t.Fatal(err)
			}
			if source == "vault" {
				before, _ := manager.InputCopy(job.ID)
				if _, present := before["password"]; present || job.HasPassword {
					t.Fatal("vault fixture already contained a process-local password before checkpoint recovery")
				}
			}
			manager.Start()
			defer func() {
				shutdown, stop := context.WithTimeout(context.Background(), 10*time.Second)
				defer stop()
				if err := manager.Shutdown(shutdown); err != nil {
					t.Error("runner/browser shutdown failed", err)
				}
			}()
			waitJob := func(predicate func(*jobs.Job) bool) *jobs.Job {
				t.Helper()
				for {
					current, _ := manager.Get(job.ID)
					if predicate(current) {
						return current
					}
					if current.FinishedAt != nil {
						t.Fatalf("runner ended before expected state: %s", current.Error)
					}
					select {
					case <-ctx.Done():
						t.Fatal("runner did not reach expected state")
					case <-time.After(10 * time.Millisecond):
					}
				}
			}
			select {
			case candidate := <-posted:
				if candidate != initial {
					t.Fatal("initial password bytes were changed")
				}
			case <-ctx.Done():
				t.Fatal("initial password was not submitted")
			}
			waitJob(func(j *jobs.Job) bool { return j.Status == "password" })
			select {
			case <-posted:
				t.Fatal("rejected initial password was submitted again without user input")
			case <-time.After(600 * time.Millisecond):
			}
			current, _ := manager.Get(job.ID)
			if current.Status != "password" || !strings.Contains(current.Prompt, "拒绝了已提交的账号密码") {
				t.Fatal("rejected password did not remain at its manual input prompt")
			}
			if !current.HasPassword {
				t.Fatal("pre-verification checkpoint did not mirror loaded credentials into the manager")
			}
			saved, err := vault.Load(email)
			if err != nil || saved.Password != initial {
				t.Fatal("consuming the automatic candidate changed protected credentials", err)
			}
			waitingCheckpoint, err := protocol.ReadCheckpoint(checkpoint)
			if err != nil || waitingCheckpoint.Stage != "web_password" || waitingCheckpoint.UpdatedAt == cp.UpdatedAt {
				t.Fatal("manual wait lost the recoverable password checkpoint", err)
			}
			if !manager.InputForStage(job.ID, "password", map[string]any{"password": corrected}) {
				t.Fatal("manual corrected password was not accepted")
			}
			current = waitJob(func(j *jobs.Job) bool { return j.FinishedAt != nil })
			if current.Status != jobs.StatusSucceeded || !current.ResultSaved {
				t.Fatalf("corrected password did not finish the production flow: %s", current.Error)
			}
			mu.Lock()
			calls := append([]string(nil), submitted...)
			mu.Unlock()
			if len(calls) != 2 || calls[0] != initial || calls[1] != corrected {
				t.Fatal("automatic and manual password candidates were not each submitted exactly once")
			}
			saved, err = vault.Load(email)
			if err != nil || saved.Password != corrected || !current.HasPassword || current.LastAuthAutomated {
				t.Fatal("verified manual password or authentication metadata was not saved correctly", err)
			}
			if _, err = os.Stat(checkpoint); !os.IsNotExist(err) {
				t.Fatal("successful runner retained its consumed checkpoint")
			}
			metadata, err := os.ReadFile(filepath.Join(cfg.OutputRoot, "job-meta.json"))
			if err != nil || strings.Contains(string(metadata), "Synthetic-Rejected9!") || strings.Contains(string(metadata), "Synthetic-Corrected9!") {
				t.Fatal("password was exposed through ordinary job metadata", err)
			}
		})
	}
}
