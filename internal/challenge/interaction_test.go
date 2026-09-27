package challenge

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"tosub2-go-port/internal/browser"
)

func TestChromiumChallengeWaitsForFreshProviderResponse(t *testing.T) {
	if os.Getenv("TOSUB2_BROWSER_TEST") != "1" {
		t.Skip("set TOSUB2_BROWSER_TEST=1 for real browser challenge interaction fixture")
	}
	path := os.Getenv("CHROME_PATH")
	if path == "" {
		path = browser.DefaultChrome()
	}
	if path == "" {
		t.Skip("Chromium not installed")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<!doctype html><title>Just a moment</title><input name="cf-turnstile-response" type="hidden"><script>setTimeout(()=>{document.querySelector('input').value='fresh-provider-response'},2100)</script>`)
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	manager := browser.NewManager(path, 1)
	defer manager.Close()
	session, err := manager.Start(ctx, "", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err = session.SetCookiesContext(ctx, []map[string]any{{"name": "cf_clearance", "value": "previously-revoked-clearance", "url": server.URL, "path": "/", "httpOnly": true}}); err != nil {
		t.Fatal(err)
	}
	var prompts atomic.Int32
	solver := BrowserSolver{Session: session, OnInteractionRequired: func(prompt string) {
		prompts.Add(1)
		if !strings.Contains(prompt, "Chromium") && !strings.Contains(prompt, "TOSUB2_BROWSER_HEADLESS") {
			t.Error("interaction prompt lacks usable browser instructions")
		}
	}}
	started := time.Now()
	result, err := solver.SolveContext(ctx, server.URL)
	if err != nil || result.Provider != "turnstile" || result.Token != "fresh-provider-response" {
		t.Fatalf("fresh response: %v provider=%s", err, result.Provider)
	}
	if time.Since(started) < 2*time.Second {
		t.Fatal("stale clearance prematurely ended challenge")
	}
	if prompts.Load() != 1 {
		t.Fatalf("interaction callback count=%d", prompts.Load())
	}
}
