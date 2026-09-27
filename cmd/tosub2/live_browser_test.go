package main

import (
	"context"
	"os"
	"testing"
	"time"

	"tosub2-go-port/internal/browser"
	"tosub2-go-port/internal/challenge"
	"tosub2-go-port/internal/config"
)

// This opt-in contacts only the public login page/SDK in a fresh browser. It
// submits no account identifier, password, OTP, OAuth grant or paid SMS request.
// A generated SDK token does not prove that an authenticated endpoint accepts it.
func TestLivePublicSentinelBrowserCompatibility(t *testing.T) {
	if os.Getenv("TOSUB2_LIVE_BROWSER_TEST") != "1" {
		t.Skip("live upstream SDK requires explicit TOSUB2_LIVE_BROWSER_TEST=1; local Chromium fixtures run separately")
	}
	cfg := config.Default()
	chrome := cfg.ChromePath
	if chrome == "" {
		chrome = browser.DefaultChrome()
	}
	if chrome == "" {
		t.Skip("Chromium not installed; set CHROME_PATH for live public SDK verification")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 65*time.Second)
	defer cancel()
	manager := browser.NewManager(chrome, 1)
	defer manager.Close()
	session, err := manager.Start(ctx, cfg.ProxyURL, "", "chromium-native", "")
	if err != nil {
		t.Fatal(err)
	}
	solver := challenge.BrowserSolver{Session: session, Timeout: 45 * time.Second}
	result, err := solver.Sentinel(ctx, cfg.AuthBase+"/log-in", cfg.SentinelSDKURL, "authorize_continue")
	if err != nil {
		t.Fatalf("live public SDK not verified: %v; no account request was submitted", err)
	}
	if result.Provider != "sentinel" || result.Token == "" {
		t.Fatal("live loader did not produce a Sentinel token")
	}
	t.Logf("real public SDK generated a token in %s; Session Observer status=%s; account endpoint acceptance remains untested", session.Identity().BrowserVersion, result.SessionObserverStatus)
}
