package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"tosub2-go-port/internal/browser"
	"tosub2-go-port/internal/config"
	"tosub2-go-port/internal/transport"
)

func TestBrowserAuthenticationZeroRotationBudgetStopsBeforeChrome(t *testing.T) {
	cfg := config.Default()
	cfg.ProxyMaxAttempts = 0
	cfg.ProxyMaxAttemptsSet = true
	pool := browser.NewManager(filepath.Join(t.TempDir(), "missing-browser"), 1)
	defer pool.Close()
	_, _, _, _, err := prepareBrowserAuthentication(context.Background(), cfg, pool, "http://fixture-sid-session:private@127.0.0.1:1", "fixture-device")
	if err == nil || !strings.Contains(err.Error(), "budget exhausted") || strings.Contains(err.Error(), "private") {
		t.Fatal("zero rotation budget reached Chrome or exposed proxy", err)
	}
}

func TestFreshBrowserProxySelectionKeepsReceivedSessionAndCleansFailedProfile(t *testing.T) {
	if os.Getenv("TOSUB2_BROWSER_TEST") != "1" {
		t.Skip("set TOSUB2_BROWSER_TEST=1 for pre-authentication Chromium proxy selection")
	}
	chrome := os.Getenv("CHROME_PATH")
	if chrome == "" {
		chrome = browser.DefaultChrome()
	}
	if chrome == "" {
		t.Skip("Chromium not installed; set CHROME_PATH")
	}
	for _, firstResponse := range []bool{false, true} {
		t.Run(map[bool]string{false: "connection-failure-rotates", true: "challenge-response-keeps-session"}[firstResponse], func(t *testing.T) {
			clearCLISensitiveEnvironment(t)
			t.Setenv("TOSUB2_BROWSER_HEADLESS", "1")
			profiles := t.TempDir()
			t.Setenv("TMPDIR", profiles)
			t.Setenv("TMP", profiles)
			t.Setenv("TEMP", profiles)
			var failedConnections atomic.Int32
			failed, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer failed.Close()
			go func() {
				for {
					conn, err := failed.Accept()
					if err != nil {
						return
					}
					failedConnections.Add(1)
					_ = conn.Close()
				}
			}()
			var firstCalls, secondCalls atomic.Int32
			first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Hostname() != "identity.fixture" {
					w.WriteHeader(502)
					return
				}
				firstCalls.Add(1)
				http.SetCookie(w, &http.Cookie{Name: "fixture-selected-session", Value: "first", Path: "/"})
				w.WriteHeader(403)
				io.WriteString(w, "<html>Security verification required</html>")
			}))
			defer first.Close()
			second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Hostname() != "identity.fixture" {
					w.WriteHeader(502)
					return
				}
				secondCalls.Add(1)
				if cookie, err := r.Cookie("oai-did"); err != nil || cookie.Value != "fixture-bound-device" {
					t.Error("proxy attempt lost pre-bound device")
				}
				if !strings.Contains(r.UserAgent(), "HeadlessChrome/") {
					t.Error("proxy attempt was not native Chromium")
				}
				http.SetCookie(w, &http.Cookie{Name: "fixture-selected-session", Value: "second", Path: "/"})
				io.WriteString(w, "fixture connected")
			}))
			defer second.Close()
			cfg := config.Default()
			cfg.ChatGPTBase = "http://identity.fixture"
			cfg.AuthBase = cfg.ChatGPTBase
			cfg.ProxyURL = ""
			cfg.ProxyURLs = []string{"http://" + failed.Addr().String(), second.URL}
			cfg.ProxyMaxAttempts = 2
			cfg.SameProxyRetries = 0
			cfg.SameProxyRetryDelaySet = true
			cfg.SameProxyRetryDelayMS = 0
			cfg.RequestTimeoutMS = 1500
			if firstResponse {
				cfg.ProxyURLs[0] = first.URL
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			pool := browser.NewManager(chrome, 1)
			defer pool.Close()
			session, bound, jar, selected, err := prepareBrowserAuthentication(ctx, cfg, pool, "", "fixture-bound-device")
			if err != nil {
				t.Fatal("browser proxy selection failed", err)
			}
			wantProxy, wantCookie := second.URL, "second"
			if firstResponse {
				wantProxy, wantCookie = first.URL, "first"
			}
			if selected != wantProxy || jar.Value("fixture-selected-session", cfg.ChatGPTBase+"/") != wantCookie {
				t.Fatal("HTTP response did not lock proxy and cookie identity")
			}
			if firstResponse && (secondCalls.Load() != 0 || firstCalls.Load() != 1) {
				t.Fatal("security rejection caused proxy rotation")
			}
			if !firstResponse && (failedConnections.Load() == 0 || secondCalls.Load() != 1) {
				t.Fatal("failed proxy did not precede fresh browser selection")
			}
			entries, _ := os.ReadDir(profiles)
			active := 0
			for _, e := range entries {
				if strings.HasPrefix(e.Name(), "tosub2-browser-") {
					active++
				}
			}
			if active != 1 {
				t.Fatal("failed proxy left an additional Chromium profile", active)
			}
			if _, err := bound.Do(ctx, transport.Request{URL: cfg.ChatGPTBase + "/again", Timeout: time.Second}); err != nil {
				t.Fatal(err)
			}
			if session.Proxy != selected || session.BoundDeviceID() != "fixture-bound-device" {
				t.Fatal("subsequent request changed selected identity")
			}
			dir := session.Dir
			if err := pool.Stop(session); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("selected browser profile survived cleanup")
			}
		})
	}
}

func TestFreshBrowserProbeCancellationReleasesPoolAndProfile(t *testing.T) {
	if os.Getenv("TOSUB2_BROWSER_TEST") != "1" {
		t.Skip("set TOSUB2_BROWSER_TEST=1 for authentication probe cancellation")
	}
	chrome := os.Getenv("CHROME_PATH")
	if chrome == "" {
		chrome = browser.DefaultChrome()
	}
	if chrome == "" {
		t.Skip("Chromium not installed; set CHROME_PATH")
	}
	clearCLISensitiveEnvironment(t)
	t.Setenv("TOSUB2_BROWSER_HEADLESS", "1")
	profiles := t.TempDir()
	t.Setenv("TMPDIR", profiles)
	t.Setenv("TMP", profiles)
	t.Setenv("TEMP", profiles)
	started := make(chan struct{})
	disconnected := make(chan struct{})
	var startOnce, doneOnce sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		startOnce.Do(func() { close(started) })
		<-r.Context().Done()
		doneOnce.Do(func() { close(disconnected) })
	}))
	defer server.Close()
	cfg := config.Default()
	cfg.ChatGPTBase = server.URL
	cfg.AuthBase = server.URL
	cfg.ProxyURL = ""
	cfg.ProxyURLs = nil
	cfg.SameProxyRetries = 0
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool := browser.NewManager(chrome, 1)
	defer pool.Close()
	result := make(chan error, 1)
	go func() {
		_, _, _, _, err := prepareBrowserAuthentication(ctx, cfg, pool, "", "fixture-cancel-device")
		result <- err
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("probe never reached local fixture")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("cancellation lost its cause", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("probe did not clean up on cancellation")
	}
	select {
	case <-disconnected:
	case <-time.After(time.Second):
		t.Fatal("canceled probe left origin request running")
	}
	entries, _ := os.ReadDir(profiles)
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "tosub2-browser-") {
			t.Fatal("canceled probe retained credential profile")
		}
	}
	verify, done := context.WithTimeout(context.Background(), 5*time.Second)
	defer done()
	session, err := pool.StartWait(verify, "", "", "chromium-native", "fixture-new-session")
	if err != nil {
		t.Fatal("canceled probe retained browser capacity", err)
	}
	dir := session.Dir
	if err = pool.Stop(session); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Clean(dir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("replacement session cleanup failed")
	}
}

// Headers and Set-Cookie can arrive while the body never completes. This is
// already an observed HTTP session, not permission to replay on the same proxy
// or obtain a new proxy identity after the transport reports a timeout.
func TestFreshBrowserPartialResponseNeverRetriesOrRotates(t *testing.T) {
	if os.Getenv("TOSUB2_BROWSER_TEST") != "1" {
		t.Skip("set TOSUB2_BROWSER_TEST=1 for Chromium partial-response identity regression")
	}
	chrome := os.Getenv("CHROME_PATH")
	if chrome == "" {
		chrome = browser.DefaultChrome()
	}
	if chrome == "" {
		t.Skip("Chromium not installed; set CHROME_PATH")
	}
	for _, status := range []int{http.StatusForbidden, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			clearCLISensitiveEnvironment(t)
			t.Setenv("TOSUB2_BROWSER_HEADLESS", "1")
			profiles := t.TempDir()
			t.Setenv("TMPDIR", profiles)
			t.Setenv("TMP", profiles)
			t.Setenv("TEMP", profiles)
			var firstCalls, secondCalls atomic.Int32
			disconnected := make(chan struct{}, 1)
			first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Hostname() != "identity.fixture" {
					w.WriteHeader(http.StatusBadGateway)
					return
				}
				firstCalls.Add(1)
				w.Header().Set("Content-Type", "text/html")
				http.SetCookie(w, &http.Cookie{Name: "fixture-partial-session", Value: "synthetic-private-session", Path: "/", HttpOnly: true})
				w.WriteHeader(status)
				_, _ = io.WriteString(w, "<!doctype html><title>Waiting</title>")
				w.(http.Flusher).Flush()
				<-r.Context().Done()
				select {
				case disconnected <- struct{}{}:
				default:
				}
			}))
			defer first.Close()
			second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Hostname() == "identity.fixture" {
					secondCalls.Add(1)
				}
				w.WriteHeader(http.StatusNoContent)
			}))
			defer second.Close()
			cfg := config.Default()
			cfg.ChatGPTBase = "http://identity.fixture"
			cfg.AuthBase = cfg.ChatGPTBase
			cfg.ProxyURL = ""
			cfg.ProxyURLs = []string{first.URL, second.URL}
			cfg.ProxyMaxAttempts = 2
			cfg.SameProxyRetries = 1
			cfg.SameProxyRetryDelaySet = true
			cfg.SameProxyRetryDelayMS = 0
			cfg.RequestTimeoutMS = 500
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			pool := browser.NewManager(chrome, 1)
			defer pool.Close()
			session, bound, jar, selected, err := prepareBrowserAuthentication(ctx, cfg, pool, "", "fixture-partial-device")
			if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, browser.ErrResponseStarted) {
				t.Fatal("incomplete received response lost its timeout/session cause", err)
			}
			if session != nil || bound != nil || jar != nil || selected != "" {
				t.Fatal("incomplete probe returned a usable authentication identity")
			}
			if firstCalls.Load() != 1 || secondCalls.Load() != 0 {
				t.Fatal("received HTTP headers were incorrectly treated as a connection failure", firstCalls.Load(), secondCalls.Load())
			}
			if strings.Contains(err.Error(), "synthetic-private-session") {
				t.Fatal("partial response error exposed a cookie")
			}
			select {
			case <-disconnected:
			case <-time.After(time.Second):
				t.Fatal("partial-response timeout left the proxy request running")
			}
			entries, readErr := os.ReadDir(profiles)
			if readErr != nil {
				t.Fatal(readErr)
			}
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), "tosub2-browser-") {
					t.Fatal("incomplete received response retained its browser profile")
				}
			}
			verify, done := context.WithTimeout(context.Background(), 5*time.Second)
			defer done()
			replacement, err := pool.StartWait(verify, "", "", "chromium-native", "fixture-replacement-device")
			if err != nil {
				t.Fatal("partial-response timeout retained browser capacity", err)
			}
			if err = pool.Stop(replacement); err != nil {
				t.Fatal(err)
			}
		})
	}
}
