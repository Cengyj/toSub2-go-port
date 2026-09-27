package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
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

// The fixture server owns its synthetic checkpoint stage. Obtain the cookies
// and actual native identity through a real Chrome request before persisting;
// a tls-client checkpoint must not be relabeled as browser-bound in a test.
func writeBrowserCheckpointFixture(t *testing.T, chrome, path string, cp protocol.Checkpoint) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	pool := browser.NewManager(chrome, 1)
	device, _ := cp.Web["deviceId"].(string)
	session, err := pool.StartWait(ctx, "", "", "chromium-native", device)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := pool.Stop(session); err != nil {
			t.Error(err)
		}
	}()
	jar := transport.NewCookieJar(cp.Cookies...)
	cfg := config.Default()
	cfg.ChatGPTBase, cfg.AuthBase = cp.ChatGPTBase, cp.AuthBase
	if _, err := authenticationDevice(&cp, jar, cfg); err != nil {
		t.Fatal(err)
	}
	bound := browser.NewTransport(session, jar)
	page, _ := cp.OAuth["pageURL"].(string)
	if page == "" {
		page = cp.ChatGPTBase + "/"
	}
	response, err := bound.Do(ctx, transport.Request{Method: http.MethodGet, URL: page, Timeout: 5 * time.Second})
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatal("fixture browser checkpoint request failed", err)
	}
	cp.Cookies = jar.Cookies()
	protocol.BindTransportSession(&cp, "chromium-native", session.Identity().UserAgent, "", true)
	if err := protocol.WriteCheckpoint(path, cp); err != nil {
		t.Fatal(err)
	}
}

func TestFreshAuthenticationFirstRequestAlreadyUsesChromium(t *testing.T) {
	if os.Getenv("TOSUB2_BROWSER_TEST") != "1" {
		t.Skip("set TOSUB2_BROWSER_TEST=1 for actual authentication-start identity")
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
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var mu sync.Mutex
	var firstUA string
	var firstDevice bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		if firstUA == "" {
			firstUA = r.UserAgent()
			cookie, err := r.Cookie("oai-did")
			firstDevice = err == nil && cookie.Value != ""
		}
		mu.Unlock()
		http.Error(w, "fixture stops before any account operation", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	cfg := config.Default()
	cfg.OutputRoot, cfg.ChatGPTBase, cfg.AuthBase, cfg.ChromePath = t.TempDir(), server.URL, server.URL, chrome
	cfg.ProxyURL, cfg.ProxyURLs, cfg.SameProxyRetries = "", nil, 0
	cfg.BrowserEnabled = true
	st, err := store.New(cfg.OutputRoot)
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	manager := jobs.NewManager(ctx, 1, st, logger)
	defer manager.Shutdown(context.Background())
	job, err := manager.Submit(jobs.Spec{Email: "start-identity@example.test", Operation: "full"})
	if err != nil {
		t.Fatal(err)
	}
	err = makeJobRunnerWithCredentials(cfg, manager, logger, credentials.NewMemoryStore(), true)(ctx, job, func(jobs.JobEvent) {})
	if err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("fixture did not stop at its explicit rejection", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(firstUA, "HeadlessChrome/") {
		t.Fatal("first authentication request did not use the actual headless Chromium identity")
	}
	if !firstDevice {
		t.Fatal("first authentication request did not carry its pre-bound device cookie")
	}
}
