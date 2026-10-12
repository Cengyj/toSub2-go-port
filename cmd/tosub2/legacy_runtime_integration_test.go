package main

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
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

// Real Go HTTP and the explicitly selected script host exercise the production runner. The
// account service and SDK are local TLS fixtures; no upstream account is used.
func TestLegacyRuntimeProductionLoginAndResume(t *testing.T) {
	if os.Getenv("TOSUB2_LEGACY_RUNTIME_TEST") != "1" {
		t.Skip("set TOSUB2_LEGACY_RUNTIME_TEST=1 to run the local authentication host fixtures")
	}
	// Darwin uses native system roots. A dedicated test child forces its local
	// fixture CA through Go's verified fallback pool, without changing the host
	// Keychain, production trust settings, or other tests' process-wide roots.
	if os.Getenv("TOSUB2_GO_RUNTIME_FIXTURE_CHILD") != "1" && os.Getenv("TOSUB2_PRODUCTION_FIXTURE_BINARY") == "" {
		executable, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, executable, "-test.run=^TestLegacyRuntimeProductionLoginAndResume$", "-test.v")
		cmd.Env = append(os.Environ(), "TOSUB2_GO_RUNTIME_FIXTURE_CHILD=1", "GODEBUG=x509usefallbackroots=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("local HTTP/script TLS fixture: %v\n%s", err, out)
		}
		return
	}
	rootsInstalled := false
	for _, scenario := range []string{
		"new-password-login", "legacy-password-checkpoint", "legacy-authenticated-checkpoint", "new-password-mfa-rejection", "legacy-mfa-checkpoint",
		"new-email-login", "new-email-rejection-resend", "legacy-email-checkpoint", "legacy-email-checkpoint-rejection", "legacy-email-accepted-checkpoint",
	} {
		t.Run(scenario, func(t *testing.T) {
			wantsMFA := strings.Contains(scenario, "mfa")
			wantsEmail := strings.Contains(scenario, "email")
			emailRejected := wantsEmail && strings.Contains(scenario, "rejection")
			emailAccepted := scenario == "legacy-email-accepted-checkpoint"
			wantsResend := strings.HasSuffix(scenario, "resend")
			clearCLISensitiveEnvironment(t)
			cfg := config.Default()
			cfg.OutputRoot = t.TempDir()
			cfg.SameProxyRetries = 0
			cfg.RequestTimeoutMS = 10000
			var mu sync.Mutex
			counts := map[string]int{}
			var state, codeChallenge, identityUA string
			const email = "legacy-runtime@example.test"
			const password = "Fixture-Password9!"
			device := "restored-fixture-device"
			mux := http.NewServeMux()
			server := httptest.NewTLSServer(mux)
			defer server.Close()
			cfg.ChatGPTBase, cfg.AuthBase = server.URL, server.URL
			cfg.SentinelSDKURL = server.URL + "/loader.js"
			if !rootsInstalled {
				roots := x509.NewCertPool()
				roots.AddCert(server.Certificate())
				x509.SetFallbackRoots(roots)
				rootsInstalled = true
			}
			mfaPayload := map[string]any{"continue_url": server.URL + "/mfa-challenge/fixture-factor", "page": map[string]any{"type": "mfa_challenge"}, "oai-client-auth-session": map[string]any{"mfa_challenge_factors": []any{map[string]any{"factor_type": "totp", "id": "fixture-factor"}}}}
			writeJSON := func(w http.ResponseWriter, value any) {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(value)
			}
			mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				counts[r.URL.Path]++
				if identityUA == "" {
					identityUA = r.UserAgent()
				} else if identityUA != r.UserAgent() {
					t.Error("runtime changed User-Agent between SDK and account requests")
				}
				switch r.URL.Path {
				case "/":
					w.Header().Set("Content-Type", "text/html")
					_, _ = io.WriteString(w, "<html>local fixture</html>")
				case "/api/auth/providers":
					writeJSON(w, map[string]any{})
				case "/api/auth/csrf":
					http.SetCookie(w, &http.Cookie{Name: "__Host-next-auth.csrf-token", Value: "fixture-csrf", Path: "/", Secure: true, HttpOnly: true})
					writeJSON(w, map[string]any{"csrfToken": "fixture-csrf"})
				case "/api/auth/signin/openai":
					_ = r.ParseForm()
					if r.PostForm.Get("csrfToken") != "fixture-csrf" || r.URL.Query().Get("login_hint") != email {
						t.Error("signin inputs lost")
					}
					device = r.URL.Query().Get("ext-oai-did")
					if cookie, err := r.Cookie("__Host-next-auth.csrf-token"); err != nil || cookie.Value != "fixture-csrf" {
						t.Error("CSRF cookie lost between requests")
					}
					page := "/log-in/password"
					if wantsEmail {
						page = "/email-verification"
					}
					writeJSON(w, map[string]any{"url": server.URL + page})
				case "/log-in/password", "/email-verification":
					http.SetCookie(w, &http.Cookie{Name: "sdk-session", Value: "same-worker", Path: "/", Secure: true, HttpOnly: true})
					_, _ = io.WriteString(w, "<html>password fixture</html>")
				case "/loader.js":
					w.Header().Set("Content-Type", "application/javascript")
					_, _ = io.WriteString(w, `const s=document.createElement('script');s.src='`+server.URL+`/sentinel/fixture/sdk.js';document.head.appendChild(s);`)
				case "/sentinel/fixture/sdk.js":
					w.Header().Set("Content-Type", "application/javascript")
					_, _ = io.WriteString(w, `window.SentinelSDK={token:async flow=>{const r=await fetch('/fixture-proof?flow='+encodeURIComponent(flow));const p=await r.json();return JSON.stringify({p:p.proof,t:null,c:'fixture-challenge',id:document.cookie.split('oai-did=')[1].split(';')[0],flow})},sessionObserverToken:async flow=>JSON.stringify({so:'fixture-observer',c:'fixture-challenge',id:document.cookie.split('oai-did=')[1].split(';')[0],flow})};`)
				case "/fixture-proof":
					if cookie, err := r.Cookie("sdk-session"); err != nil || cookie.Value != "same-worker" {
						t.Error("dynamic SDK did not use the authentication cookie session")
					}
					if wantsMFA && counts["/api/accounts/mfa/verify"] == 1 {
						if cookie, err := r.Cookie("mfa-rejected"); err != nil || cookie.Value != "same-go-session" {
							t.Error("Go runtime lost the cookie from the rejected MFA request")
						}
					}
					if emailRejected && counts["/api/accounts/email-otp/validate"] == 1 {
						if cookie, err := r.Cookie("email-rejected"); err != nil || cookie.Value != "same-go-session" {
							t.Error("dynamic SDK lost the rejected email OTP response cookie")
						}
					}
					http.SetCookie(w, &http.Cookie{Name: "sdk-result", Value: "accepted-fixture", Path: "/", Secure: true, HttpOnly: true})
					writeJSON(w, map[string]any{"proof": "local-script-proof"})
				case "/api/accounts/password/verify":
					var body, token, observer map[string]any
					_ = json.NewDecoder(r.Body).Decode(&body)
					_ = json.Unmarshal([]byte(r.Header.Get("Openai-Sentinel-Token")), &token)
					_ = json.Unmarshal([]byte(r.Header.Get("Openai-Sentinel-So-Token")), &observer)
					if body["password"] != password || token["flow"] != "password_verify" || token["id"] != device || token["p"] != "local-script-proof" || observer["so"] != "fixture-observer" {
						t.Error("password operation lost SDK proof, observer, device or input")
					}
					if cookie, err := r.Cookie("sdk-result"); err != nil || cookie.Value != "accepted-fixture" {
						t.Error("SDK response cookie did not reach the authentication request")
					}
					cp, err := protocol.ReadCheckpoint(filepath.Join(cfg.OutputRoot, "legacy-login", "login-checkpoint.json"))
					binding, present, bindingErr := protocol.CheckpointTransportSession(cp)
					if err != nil || bindingErr != nil || !present || binding.BrowserBound || binding.TLSProfile != legacyProfilePrefix+"chrome_146" || binding.UserAgent != identityUA {
						t.Error("checkpoint did not retain the legacy runtime identity")
					}
					if wantsMFA {
						http.SetCookie(w, &http.Cookie{Name: "mfa-session", Value: "fixture-factor", Path: "/", Secure: true, HttpOnly: true})
						writeJSON(w, mfaPayload)
					} else {
						writeJSON(w, map[string]any{"continue_url": server.URL + "/api/auth/callback/openai?code=web-fixture"})
					}
				case "/api/accounts/email-otp/resend":
					if !wantsResend || counts["/api/accounts/email-otp/validate"] != 1 || r.Header.Get("Openai-Sentinel-Token") != "" || r.Header.Get("Referer") != server.URL+"/email-verification" {
						t.Error("email OTP resend changed its explicit input or proof contract")
					}
					writeJSON(w, map[string]any{})
				case "/api/accounts/email-otp/validate":
					var body, token, observer map[string]any
					_ = json.NewDecoder(r.Body).Decode(&body)
					_ = json.Unmarshal([]byte(r.Header.Get("Openai-Sentinel-Token")), &token)
					_ = json.Unmarshal([]byte(r.Header.Get("Openai-Sentinel-So-Token")), &observer)
					if !wantsEmail || emailAccepted || token["flow"] != "email_otp_validate" || token["id"] != device || token["p"] != "local-script-proof" || observer["so"] != "fixture-observer" || r.Header.Get("Referer") != server.URL+"/email-verification" {
						t.Error("email OTP validation lost its dynamic proof, observer, device or referer")
					}
					if cookie, err := r.Cookie("sdk-result"); err != nil || cookie.Value != "accepted-fixture" {
						t.Error("SDK response cookie did not reach email OTP validation")
					}
					cp, err := protocol.ReadCheckpoint(filepath.Join(cfg.OutputRoot, "legacy-login", "login-checkpoint.json"))
					binding, present, bindingErr := protocol.CheckpointTransportSession(cp)
					if err != nil || cp.Stage != "web_email_otp" || bindingErr != nil || !present || binding.BrowserBound || binding.TLSProfile != legacyProfilePrefix+"chrome_146" || binding.UserAgent != identityUA {
						t.Error("email OTP checkpoint lost the protocol session binding")
					}
					if emailRejected && counts[r.URL.Path] == 1 {
						if body["code"] != "000000" {
							t.Error("initial synthetic rejected email OTP changed")
						}
						http.SetCookie(w, &http.Cookie{Name: "email-rejected", Value: "same-go-session", Path: "/", Secure: true, HttpOnly: true})
						w.WriteHeader(http.StatusForbidden)
						writeJSON(w, map[string]any{"error": map[string]any{"code": "incorrect_code"}})
						return
					}
					if body["code"] != "123456" || counts[r.URL.Path] > 1 && !emailRejected {
						t.Error("accepted email OTP was changed or consumed more than once")
					}
					if emailRejected {
						if cookie, err := r.Cookie("email-rejected"); err != nil || cookie.Value != "same-go-session" {
							t.Error("email OTP correction lost the rejected response cookie")
						}
					}
					writeJSON(w, map[string]any{"continue_url": server.URL + "/api/auth/callback/openai?code=web-fixture"})
				case "/api/accounts/mfa/issue_challenge":
					var body map[string]any
					_ = json.NewDecoder(r.Body).Decode(&body)
					if body["type"] != "totp" || body["id"] != "fixture-factor" || body["force_fresh_challenge"] != false {
						t.Error("MFA challenge changed the selected factor")
					}
					if cookie, err := r.Cookie("mfa-session"); err != nil || cookie.Value != "fixture-factor" {
						t.Error("MFA challenge lost its authentication cookie")
					}
					writeJSON(w, map[string]any{})
				case "/api/accounts/mfa/verify":
					var body, token map[string]any
					_ = json.NewDecoder(r.Body).Decode(&body)
					_ = json.Unmarshal([]byte(r.Header.Get("Openai-Sentinel-Token")), &token)
					if body["type"] != "totp" || body["id"] != "fixture-factor" || token["flow"] != "password_verify" || token["id"] != device || token["p"] != "local-script-proof" || r.Header.Get("Referer") != server.URL+"/mfa-challenge/fixture-factor" {
						t.Error("MFA request lost factor, device, dynamic proof or referer")
					}
					if counts[r.URL.Path] == 1 {
						http.SetCookie(w, &http.Cookie{Name: "mfa-rejected", Value: "same-go-session", Path: "/", Secure: true, HttpOnly: true})
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(http.StatusForbidden)
						writeJSON(w, map[string]any{"error": map[string]any{"code": "incorrect_code"}})
						return
					}
					if body["code"] != "654321" {
						t.Error("manual synthetic MFA code was changed")
					}
					if cookie, err := r.Cookie("mfa-rejected"); err != nil || cookie.Value != "same-go-session" {
						t.Error("manual MFA recovery lost the rejected request's session")
					}
					writeJSON(w, map[string]any{"continue_url": server.URL + "/api/auth/callback/openai?code=web-fixture"})
				case "/api/auth/callback/openai":
					http.SetCookie(w, &http.Cookie{Name: "__Secure-next-auth.session-token", Value: "fixture-session", Path: "/", Secure: true, HttpOnly: true})
					http.Redirect(w, r, server.URL+"/", http.StatusSeeOther)
				case "/oauth/authorize":
					state, codeChallenge = r.URL.Query().Get("state"), r.URL.Query().Get("code_challenge")
					if cookie, err := r.Cookie("__Secure-next-auth.session-token"); err != nil || cookie.Value != "fixture-session" {
						t.Error("OAuth lost authenticated web session")
					}
					http.Redirect(w, r, cfg.CodexRedirectURI+"?"+url.Values{"state": {state}, "code": {"fixture-code"}}.Encode(), 302)
				case "/oauth/token":
					_ = r.ParseForm()
					if r.PostForm.Get("code") != "fixture-code" || !protocol.VerifyPKCE(r.PostForm.Get("code_verifier"), codeChallenge) || state == "" {
						t.Error("OAuth code was not bound to the original PKCE flow")
					}
					id := "e30." + base64.RawURLEncoding.EncodeToString([]byte(`{"email":"`+email+`","sub":"fixture-user","https://api.openai.com/auth":{"chatgpt_account_id":"fixture-account"}}`)) + ".fixture"
					writeJSON(w, protocol.TokenSet{AccessToken: "fixture-access", RefreshToken: "fixture-refresh", IDToken: id, ExpiresIn: 3600})
				default:
					t.Errorf("unexpected fixture route: %s", r.URL.Path)
					http.NotFound(w, r)
				}
			})
			if !strings.HasPrefix(scenario, "new-") {
				u, _ := url.Parse(server.URL)
				cp := protocol.Checkpoint{Version: 1, Email: email, Stage: "web_password", AuthBase: server.URL, ChatGPTBase: server.URL, Web: map[string]any{"deviceId": device}, OAuth: map[string]any{"pageURL": server.URL + "/log-in/password", "loginMethod": "password"}, Cookies: []transport.Cookie{
					{Name: "oai-did", Value: device, Domain: u.Hostname(), Path: "/", Secure: true, HostOnly: true},
					{Name: "sdk-session", Value: "same-worker", Domain: u.Hostname(), Path: "/", Secure: true, HostOnly: true, HTTPOnly: true},
				}}
				if scenario == "legacy-authenticated-checkpoint" {
					cp.Stage, cp.OAuth = "email_verified", nil
					cp.Cookies = append(cp.Cookies, transport.Cookie{Name: "__Secure-next-auth.session-token", Value: "fixture-session", Domain: u.Hostname(), Path: "/", Secure: true, HostOnly: true, HTTPOnly: true})
				}
				if scenario == "legacy-mfa-checkpoint" {
					cp.Stage = "web_mfa"
					cp.OAuth["payload"] = mfaPayload
					cp.Cookies = append(cp.Cookies, transport.Cookie{Name: "mfa-session", Value: "fixture-factor", Domain: u.Hostname(), Path: "/", Secure: true, HostOnly: true, HTTPOnly: true})
					protocol.BindTransportSession(&cp, legacyProfilePrefix+"chrome_146", transport.ProfileFor("chrome_146").UserAgent, "", false)
				}
				if wantsEmail {
					cp.Stage = "web_email_otp"
					cp.OAuth = map[string]any{"pageURL": server.URL + "/email-verification", "loginMethod": "email_otp"}
					cp.Web["emailOtpRequestedAt"] = time.Now().Add(-time.Second).UTC().Format(time.RFC3339Nano)
					if emailAccepted {
						cp.Stage = "web_continue"
						cp.OAuth["payload"] = map[string]any{"continue_url": server.URL + "/api/auth/callback/openai?code=web-fixture"}
					}
					protocol.BindTransportSession(&cp, legacyProfilePrefix+"chrome_146", transport.ProfileFor("chrome_146").UserAgent, "", false)
				}
				protocol.BindTransportSession(&cp, legacyProfilePrefix+"chrome_146", transport.ProfileFor("chrome_146").UserAgent, "", false)
				if err := protocol.WriteCheckpoint(filepath.Join(cfg.OutputRoot, "legacy-login", "login-checkpoint.json"), cp); err != nil {
					t.Fatal(err)
				}
			}
			if binary := os.Getenv("TOSUB2_PRODUCTION_FIXTURE_BINARY"); binary != "" {
				runPureGoProductionFixture(t, binary, cfg, scenario, email, password, wantsMFA, server.Certificate())
				mu.Lock()
				defer mu.Unlock()
				assertLegacyRuntimeFixtureCounts(t, scenario, counts)
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
			defer cancel()
			st, err := store.New(cfg.OutputRoot)
			if err != nil {
				t.Fatal(err)
			}
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			manager := jobs.NewManager(ctx, 1, st, logger)
			defer manager.Shutdown(context.Background())
			runner := makeJobRunnerWithCredentials(cfg, manager, logger, credentials.NewMemoryStore(), true)
			var eventMu sync.Mutex
			sessionStarts, emailRejections := 0, 0
			manager.SetRunner(func(ctx context.Context, job *jobs.Job, emit func(jobs.JobEvent)) error {
				return runner(ctx, job, func(event jobs.JobEvent) {
					if strings.HasPrefix(event.Stage, "browser_") {
						t.Error("legacy mode attempted to start Chromium")
					}
					if event.Stage == "legacy_session_start" {
						eventMu.Lock()
						sessionStarts++
						eventMu.Unlock()
					}
					if event.Stage == "email_otp_rejected" {
						eventMu.Lock()
						emailRejections++
						eventMu.Unlock()
					}
					emit(event)
				})
			})
			input := map[string]any{"password": password}
			if wantsEmail {
				// Passwordless accounts must reach email verification without
				// acquiring or consuming a password or a TOTP value.
				input = map[string]any{"emailOtp": "123456"}
				if emailRejected {
					input["emailOtp"] = "000000"
				}
			}
			if wantsMFA {
				input["totpSecret"] = "JBSWY3DPEHPK3PXP"
			}
			job, err := manager.Submit(jobs.Spec{ID: "legacy-login", Email: email, Operation: "full", Input: input})
			if err != nil {
				t.Fatal(err)
			}
			manager.Start()
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
			if emailRejected {
				current := waitJob(func(j *jobs.Job) bool {
					eventMu.Lock()
					defer eventMu.Unlock()
					return j.Status == "email_otp" && emailRejections == 1
				})
				if current.Stage != "email_otp" || current.Prompt == "" || current.FinishedAt != nil {
					t.Fatal("403 incorrect_code did not preserve the email OTP prompt")
				}
				mu.Lock()
				attempts := counts["/api/accounts/email-otp/validate"]
				mu.Unlock()
				if attempts != 1 {
					t.Fatal("rejected email OTP was automatically consumed again")
				}
				cp, err := protocol.ReadCheckpoint(filepath.Join(cfg.OutputRoot, job.ID, "login-checkpoint.json"))
				if err != nil || cp.Stage != "web_email_otp" {
					t.Fatal("email OTP correction lost its recoverable checkpoint")
				}
				if wantsResend {
					if !manager.InputForStage(job.ID, "email_otp", map[string]any{"email_otp": "r"}) {
						t.Fatal("manager rejected explicit email OTP resend")
					}
					waitJob(func(j *jobs.Job) bool {
						mu.Lock()
						defer mu.Unlock()
						return j.Status == "email_otp" && counts["/api/accounts/email-otp/resend"] == 1
					})
				}
				if !manager.InputForStage(job.ID, "email_otp", map[string]any{"email_otp": "123456"}) {
					t.Fatal("manager rejected corrected synthetic email OTP")
				}
			}
			if wantsMFA {
				current := waitJob(func(j *jobs.Job) bool { return j.Status == "mfa_otp" })
				if current.Stage != "totp" || current.Prompt == "" || current.FinishedAt != nil {
					t.Fatal("403 incorrect_code did not return to the manual MFA prompt")
				}
				mu.Lock()
				attempts := counts["/api/accounts/mfa/verify"]
				mu.Unlock()
				if attempts != 1 {
					t.Fatal("rejected automatic code was replayed before manual input")
				}
				cp, err := protocol.ReadCheckpoint(filepath.Join(cfg.OutputRoot, job.ID, "login-checkpoint.json"))
				if err != nil || cp.Stage != "web_mfa" {
					t.Fatal("manual wait lost the recoverable MFA checkpoint", err)
				}
				if !manager.InputForStage(job.ID, "mfa_otp", map[string]any{"mfa_otp": "654321"}) {
					t.Fatal("manager rejected the corrected synthetic MFA input")
				}
			}
			current := waitJob(func(j *jobs.Job) bool { return j.FinishedAt != nil })
			if current.Status != jobs.StatusSucceeded || !current.ResultSaved {
				t.Fatalf("Go flow failed to finish: %s", current.Error)
			}
			eventMu.Lock()
			starts := sessionStarts
			eventMu.Unlock()
			if starts != 1 || current.Attempts != 1 {
				t.Fatal("manual correction restarted the authentication session")
			}
			data, err := os.ReadFile(filepath.Join(cfg.OutputRoot, job.ID, "sub2api-import-oauth.json"))
			if err != nil || !strings.Contains(string(data), "fixture-refresh") {
				t.Fatal("OAuth output was not saved", err)
			}
			mu.Lock()
			defer mu.Unlock()
			assertLegacyRuntimeFixtureCounts(t, scenario, counts)
		})
	}
}

func assertLegacyRuntimeFixtureCounts(t *testing.T, scenario string, counts map[string]int) {
	t.Helper()
	if counts["/oauth/token"] != 1 {
		t.Fatal("one-use authorization code was not exchanged exactly once")
	}
	if strings.HasPrefix(scenario, "legacy-") && counts["/api/auth/signin/openai"] != 0 {
		t.Fatal("checkpoint recovery unnecessarily restarted login")
	}
	if scenario == "legacy-authenticated-checkpoint" || scenario == "legacy-email-accepted-checkpoint" {
		if counts["/loader.js"] != 0 || counts["/api/accounts/email-otp/validate"] != 0 || counts["/api/accounts/password/verify"] != 0 {
			t.Fatal("accepted checkpoint replayed an already consumed login input")
		}
	} else if strings.Contains(scenario, "email") {
		validations, resends := 1, 0
		if strings.Contains(scenario, "rejection") {
			validations = 2
		}
		if strings.HasSuffix(scenario, "resend") {
			resends = 1
		}
		if counts["/api/accounts/email-otp/validate"] != validations || counts["/api/accounts/email-otp/resend"] != resends || counts["/fixture-proof"] != validations || counts["/api/accounts/password/verify"] != 0 || counts["/api/accounts/mfa/verify"] != 0 {
			t.Fatal("email OTP recovery replayed input, changed proof count or used another login method")
		}
	} else if strings.Contains(scenario, "mfa") {
		passwords := 1
		if scenario == "legacy-mfa-checkpoint" {
			passwords = 0
		}
		if counts["/api/accounts/mfa/issue_challenge"] != 1 || counts["/api/accounts/mfa/verify"] != 2 || counts["/fixture-proof"] != passwords+2 || counts["/api/accounts/password/verify"] != passwords {
			t.Fatal("MFA recovery changed its session or replayed input")
		}
	} else if counts["/fixture-proof"] != 1 || counts["/api/accounts/password/verify"] != 1 {
		t.Fatal("real dynamic runtime was not exercised exactly once")
	}
}
