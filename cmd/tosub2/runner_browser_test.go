package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
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
	"tosub2-go-port/internal/logging"
	"tosub2-go-port/internal/protocol"
	"tosub2-go-port/internal/transport"
)

// This executes the production runner, actual Chromium SDK and BrowserTransport,
// HTTP mailbox, interactive stdin, manager and private export. Only the remote
// providers and OS vault are fixtures. No real account or remote SDK is used.
func TestCLIResumeBrowserEmailMFAWorkspaceAndOAuth(t *testing.T) {
	if os.Getenv("TOSUB2_BROWSER_TEST") != "1" {
		t.Skip("set TOSUB2_BROWSER_TEST=1 for production CLI/browser account flow")
	}
	chrome := os.Getenv("CHROME_PATH")
	if chrome == "" {
		chrome = browser.DefaultChrome()
	}
	if chrome == "" {
		t.Skip("Chromium not installed; set CHROME_PATH")
	}
	for _, scenario := range []string{"automatic-mail", "manual-mail", "timeout-manual", "timeout-resend-auto", "same-code-new-message"} {
		t.Run(scenario, func(t *testing.T) {
			automatic := scenario != "manual-mail"
			timeoutMail := strings.HasPrefix(scenario, "timeout-")
			clearCLISensitiveEnvironment(t)
			t.Setenv("TOSUB2_BROWSER_HEADLESS", "1")
			email := "runner@example.test"
			device := "runner-browser-device"
			otp := "654321"
			dir := t.TempDir()
			var mu sync.Mutex
			counts := map[string]int{}
			var state, challenge, authUA string
			mailTime := time.Now().UTC().Format(time.RFC3339Nano)
			mailStates := []string{}
			sawAutomaticWaiting := false
			mux := http.NewServeMux()
			mux.HandleFunc("/favicon.ico", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
			server := httptest.NewServer(mux)
			defer server.Close()
			jsonOut := func(w http.ResponseWriter, v any) {
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(v)
			}
			page := func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/html")
				io.WriteString(w, "<!doctype html><html><head></head><body>authorized local fixture</body></html>")
			}
			workspace := func() map[string]any {
				return map[string]any{"continue_url": server.URL + "/workspace", "page": map[string]any{"type": "workspace"}, "oai-client-auth-session": map[string]any{"workspaces": []any{map[string]any{"id": "personal", "kind": "personal"}, map[string]any{"id": "org", "kind": "organization"}}}}
			}
			mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/" {
					t.Errorf("unexpected remote route %s", r.URL.Path)
					http.NotFound(w, r)
					return
				}
				page(w, r)
			})
			for _, path := range []string{"/email-verification", "/workspace", "/mfa-challenge/factor", "/log-in/password"} {
				mux.HandleFunc(path, page)
			}
			mux.HandleFunc("/loader.js", func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				counts["loader"]++
				mu.Unlock()
				w.Header().Set("Content-Type", "application/javascript")
				io.WriteString(w, "(()=>{const s=document.createElement('script');s.src='/sdk/v1.js';document.head.appendChild(s)})()")
			})
			mux.HandleFunc("/sdk/v1.js", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/javascript")
				io.WriteString(w, "window.SentinelSDK={token:async flow=>await fetch('/sdk/token?flow='+encodeURIComponent(flow)+'&device='+encodeURIComponent(localStorage.getItem('oai-did'))).then(r=>r.text()),sessionObserverToken:async flow=>JSON.stringify({so:'fixture-observer',c:'fixture',id:localStorage.getItem('oai-did'),flow})}")
			})
			mux.HandleFunc("/sdk/token", func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				counts["sdk-token"]++
				mu.Unlock()
				if r.URL.Query().Get("device") != device {
					t.Error("SDK lost device identity")
				}
				jsonOut(w, map[string]any{"p": "fixture-proof", "t": nil, "c": "fixture-challenge", "id": device, "flow": r.URL.Query().Get("flow")})
			})
			mux.HandleFunc("/mail", func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				counts["mail"]++
				mu.Unlock()
				if r.Method != "POST" || r.Header.Get("Authorization") != "Bearer synthetic-mail-key" {
					t.Error("mail request options lost")
				}
				var b map[string]any
				json.NewDecoder(r.Body).Decode(&b)
				if b["email"] != email {
					t.Error("mail body lost")
				}
				mu.Lock()
				validated := counts["/api/accounts/email-otp/validate"]
				resent := counts["/api/accounts/email-otp/resend"]
				mu.Unlock()
				message := map[string]any{"message_id": fmt.Sprintf("message-%d", validated), "received_at": mailTime, "message": "Your verification code is " + otp}
				if timeoutMail {
					// An undated already-delivered message cannot be auto-submitted
					// on resume. Resend snapshots it, then returns a new identity
					// with the same digits to prove identity-based deduplication.
					delete(message, "received_at")
					message["message_id"] = fmt.Sprintf("resend-message-%d", resent)
				}
				jsonOut(w, message)
			})
			authCall := func(r *http.Request, flow string) map[string]any {
				t.Helper()
				var b map[string]any
				if json.NewDecoder(r.Body).Decode(&b) != nil {
					t.Error("invalid authentication JSON")
				}
				mu.Lock()
				defer mu.Unlock()
				counts[r.URL.Path]++
				if authUA == "" {
					authUA = r.UserAgent()
				} else if authUA != r.UserAgent() {
					t.Error("browser identity changed during auth")
				}
				if !strings.Contains(r.UserAgent(), "Chrome/") {
					t.Error("authentication did not use browser")
				}
				if flow != "" {
					var token map[string]any
					if json.Unmarshal([]byte(r.Header.Get("Openai-Sentinel-Token")), &token) != nil || token["flow"] != flow || token["id"] != device {
						t.Error("wrong Sentinel flow/device")
					}
					if r.Header.Get("OpenAI-Sentinel-SO-Token") == "" {
						t.Error("observer token missing")
					}
				}
				return b
			}
			mux.HandleFunc("/api/accounts/email-otp/resend", func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				counts["/api/accounts/email-otp/resend"]++
				if len(mailStates) == 0 || mailStates[len(mailStates)-1] != "ready" {
					t.Error("resend did not snapshot mailbox before sending")
				}
				mu.Unlock()
				jsonOut(w, map[string]any{})
			})
			mux.HandleFunc("/api/accounts/email-otp/validate", func(w http.ResponseWriter, r *http.Request) {
				b := authCall(r, "email_otp_validate")
				mu.Lock()
				validationCount := counts["/api/accounts/email-otp/validate"]
				mu.Unlock()
				if b["code"] != otp || (scenario == "same-code-new-message" && validationCount == 1) {
					w.WriteHeader(400)
					jsonOut(w, map[string]any{"error": map[string]any{"code": "invalid_otp"}})
					return
				}
				http.SetCookie(w, &http.Cookie{Name: "fixture-auth", Value: "accepted", Path: "/", HttpOnly: true})
				jsonOut(w, map[string]any{"continue_url": server.URL + "/mfa-challenge/factor", "page": map[string]any{"type": "mfa_challenge"}, "oai-client-auth-session": map[string]any{"mfa_factors": []any{map[string]any{"factor_type": "totp", "id": "factor"}}}})
			})
			mux.HandleFunc("/api/accounts/mfa/issue_challenge", func(w http.ResponseWriter, r *http.Request) { authCall(r, ""); jsonOut(w, map[string]any{}) })
			mux.HandleFunc("/api/accounts/mfa/verify", func(w http.ResponseWriter, r *http.Request) {
				b := authCall(r, "")
				code, _ := b["code"].(string)
				if len(code) != 6 {
					t.Error("MFA input not delivered")
				}
				if _, err := r.Cookie("fixture-auth"); err != nil {
					t.Error("browser auth Cookie was lost")
				}
				jsonOut(w, workspace())
			})
			mux.HandleFunc("/api/accounts/workspace/select", func(w http.ResponseWriter, r *http.Request) {
				b := authCall(r, "")
				if b["workspace_id"] != "org" {
					t.Error("explicit workspace lost")
				}
				mu.Lock()
				n := counts[r.URL.Path]
				savedState := state
				mu.Unlock()
				next := server.URL + "/api/auth/callback/openai"
				if n == 2 {
					next = "http://localhost:1455/auth/callback?code=fixture-auth-code&state=" + savedState
				}
				jsonOut(w, map[string]any{"continue_url": next, "page": map[string]any{"type": "external_url"}})
			})
			mux.HandleFunc("/api/auth/callback/openai", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, server.URL+"/", 302) })
			mux.HandleFunc("/oauth/authorize", func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				state = r.URL.Query().Get("state")
				challenge = r.URL.Query().Get("code_challenge")
				counts["authorize"]++
				mu.Unlock()
				if r.URL.Query().Get("code_challenge_method") != "S256" {
					t.Error("PKCE method missing")
				}
				http.Redirect(w, r, server.URL+"/choose-an-account", 302)
			})
			mux.HandleFunc("/choose-an-account", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/html")
				io.WriteString(w, "<input name=\"session_id\" value=\"us_fixture_session_1234567890\">")
			})
			mux.HandleFunc("/api/accounts/session/select", func(w http.ResponseWriter, r *http.Request) {
				b := authCall(r, "")
				if b["session_id"] != "us_fixture_session_1234567890" {
					t.Error("session selection lost")
				}
				jsonOut(w, workspace())
			})
			mux.HandleFunc("/oauth/token", func(w http.ResponseWriter, r *http.Request) {
				r.ParseForm()
				sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
				mu.Lock()
				counts["exchange"]++
				want := challenge
				mu.Unlock()
				if base64.RawURLEncoding.EncodeToString(sum[:]) != want || r.Form.Get("code") != "fixture-auth-code" {
					t.Error("OAuth exchange does not match PKCE grant")
				}
				jwt := "e30." + base64.RawURLEncoding.EncodeToString([]byte("{\"email\":\""+email+"\",\"https://api.openai.com/auth\":{\"chatgpt_account_id\":\"fixture-account\"}}")) + ".fixture"
				jsonOut(w, map[string]any{"access_token": "synthetic-runner-access", "refresh_token": "synthetic-runner-refresh", "id_token": jwt, "expires_in": 3600})
			})
			checkpoint := filepath.Join(dir, "checkpoint.json")
			cp := protocol.Checkpoint{Version: 1, Email: email, Stage: "web_email_otp", ChatGPTBase: server.URL, AuthBase: server.URL, UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano), Cookies: []transport.Cookie{}, Web: map[string]any{"deviceId": device, "emailOtpRequestedAt": time.Now().UTC().Format(time.RFC3339Nano)}, OAuth: map[string]any{"pageURL": server.URL + "/email-verification", "loginMethod": "email_otp"}}
			writeBrowserCheckpointFixture(t, chrome, checkpoint, cp)
			cfg, err := config.Parse([]string{"--resume-checkpoint=" + checkpoint, "--output-root=" + filepath.Join(dir, "jobs"), "--chatgpt-base=" + server.URL, "--auth-base=" + server.URL, "--sentinel-sdk-url=" + server.URL + "/loader.js", "--chrome-path=" + chrome, "--workspace-id=org", "--sub2api-out=" + filepath.Join(dir, "oauth.json"), "--out=" + filepath.Join(dir, "session.json")})
			if err != nil {
				t.Fatal(err)
			}
			if timeoutMail {
				cfg.MailPollTimeoutMS, cfg.MailPollIntervalMS = 180, 20
			}
			if scenario == "same-code-new-message" {
				cfg.MailPollIntervalMS = 20
			}
			vault := credentials.NewMemoryStore()
			saved := credentials.Credentials{}
			input := "000000\n" + otp + "\n123456\n"
			if automatic {
				saved.MailAPIURL = server.URL + "/mail"
				saved.MailMethod = "POST"
				saved.MailHeaders = map[string]string{"Authorization": "Bearer synthetic-mail-key"}
				saved.MailBody = "{\"email\":\"" + email + "\"}"
				saved.TOTPSecret = "JBSWY3DPEHPK3PXP"
				input = ""
				if scenario == "timeout-manual" {
					input = "000000\n" + otp + "\n"
				}
				if scenario == "timeout-resend-auto" {
					input = "r\n"
				}
			}
			if err = vault.Save(email, saved); err != nil {
				t.Fatal(err)
			}
			var out, logs bytes.Buffer
			ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
			defer cancel()
			_, err = runCLIWithRunnerFactory(ctx, cfg, logging.NewWithWriter(&logs), strings.NewReader(input), &out, func(c config.Config, m *jobs.Manager, l *slog.Logger) jobs.Runner {
				runner := makeJobRunnerWithCredentials(c, m, l, vault, true)
				return func(ctx context.Context, job *jobs.Job, emit func(jobs.JobEvent)) error {
					return runner(ctx, job, func(event jobs.JobEvent) {
						if status, ok := event.Data["mailStatus"].(string); ok {
							if event.Stage == "email_otp" && event.Status == jobs.StatusWaitingInput && status == "polling" {
								sawAutomaticWaiting = true
							} else if event.Stage != "" || event.Status != "" {
								t.Error("mail progress changed the protocol/input stage")
							}
							mu.Lock()
							if len(mailStates) == 0 || mailStates[len(mailStates)-1] != status {
								mailStates = append(mailStates, status)
							}
							mu.Unlock()
						}
						emit(event)
						if event.Stage == "email_otp" && event.Data["mailStatus"] == "polling" {
							current, _ := m.Get(job.ID)
							if current.Status != "email_otp" {
								t.Error("automatic mailbox polling hid the manual console override")
							}
						}
					})
				}
			})
			if err != nil {
				t.Fatalf("production CLI account flow: %v; output=%s; logs=%s", err, out.String(), logs.String())
			}
			mu.Lock()
			defer mu.Unlock()
			if counts["exchange"] != 1 || counts["authorize"] != 1 || counts["/api/accounts/workspace/select"] != 2 || counts["sdk-token"] < 1 {
				t.Fatalf("incomplete production chain: %v", counts)
			}
			if automatic && (!sawAutomaticWaiting || counts["mail"] < 1) {
				t.Fatal("automatic mailbox never polled")
			}
			if (!automatic || scenario == "timeout-manual" || scenario == "same-code-new-message") && counts["/api/accounts/email-otp/validate"] != 2 {
				t.Fatal("manual rejected OTP did not retry exactly once")
			}
			joinedMailStates := "," + strings.Join(mailStates, ",") + ","
			if timeoutMail && !strings.Contains(joinedMailStates, ",timeout,") {
				t.Fatal("automatic timeout did not preserve manual login", mailStates)
			}
			if scenario == "timeout-resend-auto" && (counts["/api/accounts/email-otp/resend"] != 1 || counts["/api/accounts/email-otp/validate"] != 1 || !strings.Contains(joinedMailStates, ",ready,polling,found,")) {
				t.Fatal("resend did not safely restore automatic polling", mailStates, counts)
			}
			data, err := os.ReadFile(filepath.Join(dir, "oauth.json"))
			if err != nil {
				t.Fatal(err)
			}
			var doc protocol.Sub2APIOAuthExport
			if json.Unmarshal(data, &doc) != nil || len(doc.Accounts) != 1 || doc.Accounts[0].Credentials.Email != email {
				t.Fatal("compatible OAuth export missing")
			}
			if _, err = os.Stat(filepath.Join(dir, "session.json")); err != nil {
				t.Fatal("both mode session output missing")
			}
			if _, err = os.Stat(checkpoint); !os.IsNotExist(err) {
				t.Fatal("consumed checkpoint retained")
			}
			for _, secret := range []string{otp, "synthetic-runner-access", "synthetic-runner-refresh", "synthetic-mail-key", "fixture-proof"} {
				if strings.Contains(out.String()+logs.String(), secret) {
					t.Fatal("private material leaked to CLI output")
				}
			}
		})
	}
}
