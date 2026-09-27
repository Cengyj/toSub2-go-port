package main

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
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

// Keep the real runner, protocol state machine, dynamic SDK, redirect handling
// and Chromium network stack. The local server binds its synthetic session to
// the first ClientHello, native UA and device. This is not upstream acceptance.
func TestFreshAuthenticationTLSIdentityThroughOAuth(t *testing.T) {
	if os.Getenv("TOSUB2_BROWSER_TEST") != "1" {
		t.Skip("set TOSUB2_BROWSER_TEST=1 for the real Chromium HTTPS identity fixture")
	}
	if runtime.GOOS == "windows" {
		t.Skip("fixture certificate pin uses a POSIX-only temporary Chrome launcher")
	}
	chrome := os.Getenv("CHROME_PATH")
	if chrome == "" {
		chrome = browser.DefaultChrome()
	}
	if chrome == "" {
		t.Skip("Chromium unavailable; set CHROME_PATH")
	}
	clearCLISensitiveEnvironment(t)
	t.Setenv("TOSUB2_BROWSER_HEADLESS", "1")
	const email = "tls-identity@example.test"
	const password = "Synthetic-Identity-Pass9!"
	const sessionCookie = "synthetic-tls-bound-session"
	var mu sync.Mutex
	hellos := map[string]string{}
	counts := map[string]int{}
	var firstHello, firstUA, device, state, codeChallenge string
	var identityErrors []string
	mux := http.NewServeMux()
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hello := hellos[r.RemoteAddr]
		did, didErr := r.Cookie("oai-did")
		if firstHello == "" {
			firstHello, firstUA = hello, r.UserAgent()
			if didErr == nil {
				device = did.Value
			}
			if hello == "" || !strings.Contains(firstUA, "HeadlessChrome/") || device == "" {
				identityErrors = append(identityErrors, "first request lacked native TLS/UA/device binding")
			}
		} else {
			cookie, err := r.Cookie("__Host-fixture-session")
			if hello != firstHello || r.UserAgent() != firstUA || didErr != nil || did.Value != device || err != nil || cookie.Value != sessionCookie {
				if r.URL.Path != "/identity-probe" {
					identityErrors = append(identityErrors, fmt.Sprintf("binding changed at %s (TLS=%t UA=%t device=%t cookie=%t)", r.URL.Path, hello == firstHello, r.UserAgent() == firstUA, didErr == nil && did.Value == device, err == nil && cookie.Value == sessionCookie))
				}
				mu.Unlock()
				http.Error(w, "synthetic session identity rejected", http.StatusForbidden)
				return
			}
		}
		counts[r.URL.Path]++
		mu.Unlock()
		mux.ServeHTTP(w, r)
	}))
	server.EnableHTTP2 = true
	server.TLS = &tls.Config{GetConfigForClient: func(h *tls.ClientHelloInfo) (*tls.Config, error) {
		mu.Lock()
		hellos[h.Conn.RemoteAddr().String()] = tlsIdentityFixtureHello(h)
		mu.Unlock()
		return nil, nil
	}}
	server.StartTLS()
	defer server.Close()
	jsonOut := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	page := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, "<!doctype html><html><head><link rel=icon href=data:,></head><body>Local authorized fixture</body></html>")
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "__Host-fixture-session", Value: sessionCookie, Path: "/", Secure: true, HttpOnly: true})
		page(w, r)
	})
	mux.HandleFunc("/favicon.ico", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) })
	mux.HandleFunc("/identity-probe", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) })
	mux.HandleFunc("/api/auth/providers", func(w http.ResponseWriter, _ *http.Request) {
		jsonOut(w, map[string]any{"openai": map[string]any{"id": "openai"}})
	})
	mux.HandleFunc("/api/auth/csrf", func(w http.ResponseWriter, _ *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "__Host-next-auth.csrf-token", Value: "synthetic-csrf", Path: "/", Secure: true, HttpOnly: true})
		jsonOut(w, map[string]any{"csrfToken": "synthetic-csrf"})
	})
	mux.HandleFunc("/api/auth/signin/openai", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.ParseForm() != nil || r.Form.Get("csrfToken") != "synthetic-csrf" {
			t.Error("fresh signin lost its method or CSRF form")
		}
		mu.Lock()
		sameDevice := r.URL.Query().Get("ext-oai-did") == device
		mu.Unlock()
		if !sameDevice {
			t.Error("signin changed device identity")
		}
		jsonOut(w, map[string]any{"url": server.URL + "/authorize-web"})
	})
	mux.HandleFunc("/authorize-web", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "__Host-redirect-cookie", Value: "synthetic-redirect", Path: "/", Secure: true, HttpOnly: true})
		http.Redirect(w, r, "/log-in/password", http.StatusFound)
	})
	mux.HandleFunc("/log-in/password", func(w http.ResponseWriter, r *http.Request) {
		if cookie, err := r.Cookie("__Host-redirect-cookie"); err != nil || cookie.Value != "synthetic-redirect" {
			t.Error("manual 302 redirect lost Set-Cookie")
		}
		page(w, r)
	})
	mux.HandleFunc("/loader.js", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/javascript")
		_, _ = io.WriteString(w, "(()=>{let s=document.createElement('script');s.src='/sdk/v20260927.js';document.head.appendChild(s)})()")
	})
	mux.HandleFunc("/sdk/v20260927.js", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/javascript")
		_, _ = io.WriteString(w, "window.SentinelSDK={token:async flow=>await fetch('/sdk/proof?flow='+encodeURIComponent(flow)+'&device='+encodeURIComponent(localStorage.getItem('oai-did'))).then(r=>r.text()),sessionObserverToken:async flow=>JSON.stringify({so:'synthetic-observer',c:'fixture',id:localStorage.getItem('oai-did'),flow})}")
	})
	mux.HandleFunc("/sdk/proof", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		id := device
		mu.Unlock()
		if r.URL.Query().Get("device") != id {
			t.Error("SDK JavaScript device differed from the first request")
		}
		jsonOut(w, map[string]any{"p": "synthetic-proof", "t": nil, "c": "fixture", "id": id, "flow": r.URL.Query().Get("flow")})
	})
	mux.HandleFunc("/api/accounts/password/verify", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		if r.Method != http.MethodPost || json.NewDecoder(r.Body).Decode(&body) != nil || body["password"] != password {
			t.Error("unexpected synthetic password submission")
		}
		for _, key := range []string{"Openai-Sentinel-Token", "OpenAI-Sentinel-SO-Token"} {
			var proof map[string]any
			mu.Lock()
			id := device
			mu.Unlock()
			if json.Unmarshal([]byte(r.Header.Get(key)), &proof) != nil || proof["id"] != id || proof["flow"] != "password_verify" {
				t.Error("password request lost SDK flow/device identity")
			}
		}
		jsonOut(w, map[string]any{"continue_url": server.URL + "/api/auth/callback/openai", "page": map[string]any{"type": "external_url"}})
	})
	mux.HandleFunc("/api/auth/callback/openai", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "__Secure-next-auth.session-token", Value: "synthetic-login-session", Path: "/", Secure: true, HttpOnly: true})
		http.Redirect(w, r, "/", http.StatusSeeOther)
	})
	mux.HandleFunc("/oauth/authorize", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		state, codeChallenge = r.URL.Query().Get("state"), r.URL.Query().Get("code_challenge")
		mu.Unlock()
		if cookie, err := r.Cookie("__Secure-next-auth.session-token"); err != nil || cookie.Value != "synthetic-login-session" {
			t.Error("manual 303 redirect lost login Cookie")
		}
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, "<input name=\"session_id\" value=\"us_fixture_identity_1234567890\">")
	})
	mux.HandleFunc("/api/accounts/session/select", func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		s := state
		mu.Unlock()
		callback := protocol.DefaultCodexRedirectURI + "?" + url.Values{"code": {"synthetic-identity-code"}, "state": {s}}.Encode()
		jsonOut(w, map[string]any{"continue_url": callback, "page": map[string]any{"type": "external_url"}})
	})
	mux.HandleFunc("/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		digest := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
		mu.Lock()
		challenge := codeChallenge
		mu.Unlock()
		if r.Method != http.MethodPost || r.Form.Get("code") != "synthetic-identity-code" || base64.RawURLEncoding.EncodeToString(digest[:]) != challenge {
			t.Error("OAuth exchange lost its one-time code/PKCE contract")
		}
		claims, _ := json.Marshal(map[string]any{"email": email, "https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "synthetic-identity-account"}})
		jsonOut(w, map[string]any{"access_token": "synthetic-identity-access", "refresh_token": "synthetic-identity-refresh", "id_token": "e30." + base64.RawURLEncoding.EncodeToString(claims) + ".fixture", "expires_in": 3600})
	})

	// Trust only this synthetic certificate, only in this temporary launcher.
	// Production TLS validation and the macOS system trust store are unchanged.
	spki := sha256.Sum256(server.Certificate().RawSubjectPublicKeyInfo)
	launcher := filepath.Join(t.TempDir(), "fixture-chrome")
	shellQuote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
	script := "#!/bin/sh\nexec " + shellQuote(chrome) + " --ignore-certificate-errors-spki-list=" + shellQuote(base64.StdEncoding.EncodeToString(spki[:])) + " \"$@\"\n"
	if err := os.WriteFile(launcher, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.OutputRoot, cfg.ChatGPTBase, cfg.AuthBase = t.TempDir(), server.URL, server.URL
	cfg.ChromePath, cfg.SentinelSDKURL, cfg.BrowserEnabled = launcher, server.URL+"/loader.js", true
	cfg.ProxyURL, cfg.ProxyURLs, cfg.SameProxyRetries, cfg.SameProxyRiskRetries, cfg.RequestTimeoutMS = "", nil, 0, 0, 10000
	cfg.CodexRedirectURI = protocol.DefaultCodexRedirectURI
	st, err := store.New(cfg.OutputRoot)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	manager := jobs.NewManager(ctx, 1, st, logger)
	defer manager.Shutdown(context.Background())
	job, err := manager.Submit(jobs.Spec{ID: "tls-identity", Email: email, Operation: "full", Input: map[string]any{"password": password}})
	if err != nil {
		t.Fatal(err)
	}
	if err := makeJobRunnerWithCredentials(cfg, manager, logger, credentials.NewMemoryStore(), true)(ctx, job, func(jobs.JobEvent) {}); err != nil {
		mu.Lock()
		problems := append([]string(nil), identityErrors...)
		mu.Unlock()
		t.Fatal("HTTPS identity fixture runner failed:", err, problems)
	}
	mu.Lock()
	problems := append([]string(nil), identityErrors...)
	ua, id := firstUA, device
	for _, path := range []string{"/api/auth/providers", "/api/auth/csrf", "/api/auth/signin/openai", "/authorize-web", "/loader.js", "/sdk/v20260927.js", "/sdk/proof", "/api/accounts/password/verify", "/oauth/authorize", "/oauth/token"} {
		if counts[path] == 0 {
			problems = append(problems, "missing actual request: "+path)
		}
	}
	if counts["/api/accounts/password/verify"] != 1 || counts["/oauth/token"] != 1 {
		problems = append(problems, "unexpected repeated one-time operation")
	}
	mu.Unlock()
	if len(problems) != 0 {
		t.Fatal(problems)
	}
	if _, err := os.Stat(filepath.Join(cfg.OutputRoot, job.ID, "sub2api-import-oauth.json")); err != nil {
		t.Fatal("final compatible export missing")
	}

	// Negative control: identical synthetic UA/device/Cookie must not make a
	// different Go TLS ClientHello count as the established browser session.
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/identity-probe", nil)
	req.Header.Set("User-Agent", ua)
	req.AddCookie(&http.Cookie{Name: "oai-did", Value: id})
	req.AddCookie(&http.Cookie{Name: "__Host-fixture-session", Value: sessionCookie})
	res, err := server.Client().Do(req)
	if err != nil {
		t.Fatal("negative control request failed")
	}
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatal("fixture accepted Go TLS with copied browser Cookie and UA")
	}
}

func tlsIdentityFixtureHello(h *tls.ClientHelloInfo) string {
	// Current Chromium also greases signature algorithms, not only ciphers
	// and supported groups. The reserved value changes between handshakes.
	withoutGREASE := func(v uint16) bool { return v&0x0f0f != 0x0a0a || byte(v>>8) != byte(v) }
	var ciphers, curves, versions, signatures []uint16
	for _, x := range h.CipherSuites {
		if withoutGREASE(x) {
			ciphers = append(ciphers, x)
		}
	}
	for _, x := range h.SupportedCurves {
		if withoutGREASE(uint16(x)) {
			curves = append(curves, uint16(x))
		}
	}
	for _, x := range h.SupportedVersions {
		if withoutGREASE(x) {
			versions = append(versions, x)
		}
	}
	for _, x := range h.SignatureSchemes {
		if withoutGREASE(uint16(x)) {
			signatures = append(signatures, uint16(x))
		}
	}
	return fmt.Sprint(ciphers, curves, versions, signatures, h.SupportedProtos)
}
