package challenge

import (
	"context"
	"errors"
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

func TestSentinelValidatesRequiredPayloadAndFlow(t *testing.T) {
	tests := []struct {
		value, flow    string
		observer, want bool
	}{
		{`{"p":"","t":"","c":"","id":"fixture","flow":"login"}`, "login", false, true},
		{`{"so":"fixture","c":"","id":"fixture","flow":"login"}`, "login", true, true},
		{`{"p":"","t":"","c":"","id":"fixture","flow":"other"}`, "login", false, false},
		{`{"p":"fixture"}`, "login", false, false},
		{`private-malformed-token`, "login", false, false},
	}
	for _, tt := range tests {
		err := validateSentinelToken(tt.value, tt.flow, tt.observer)
		if (err == nil) != tt.want {
			t.Errorf("validation got success=%v want=%v", err == nil, tt.want)
		}
		if err != nil && strings.Contains(err.Error(), "private-malformed-token") {
			t.Fatal("token leaked in validation error")
		}
	}
}

func TestChromiumSentinelLoaderRecoveryAndObserverPolicy(t *testing.T) {
	if os.Getenv("TOSUB2_BROWSER_TEST") != "1" {
		t.Skip("set TOSUB2_BROWSER_TEST=1 for real Chromium SDK lifecycle fixture")
	}
	path := os.Getenv("CHROME_PATH")
	if path == "" {
		path = browser.DefaultChrome()
	}
	if path == "" {
		t.Skip("Chromium not installed; set CHROME_PATH")
	}
	var flaky, rotating, pages atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/page" {
			pages.Add(1)
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, "<!doctype html><body>authorized SDK fixture</body>")
			return
		}
		if r.URL.Path == "/flaky.js" && flaky.Add(1) == 1 {
			w.WriteHeader(503)
			return
		}
		version := int32(1)
		if r.URL.Path == "/rotating.js" {
			version = rotating.Add(1)
		}
		w.Header().Set("Content-Type", "application/javascript")
		w.Header().Set("Cache-Control", "public, max-age=3600")
		fmt.Fprintf(w, `window.SentinelSDK={token:async flow=>JSON.stringify({p:'fixture',t:'fixture',c:'fixture',id:'fixture',flow,version:%d}),sessionObserverToken:async flow=>{throw new Error('fixture-secret-never-log')}};`, version)
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	manager := browser.NewManager(path, 1)
	defer manager.Close()
	session, err := manager.Start(ctx, "", "", "", "fixture-device")
	if err != nil {
		t.Fatal(err)
	}
	solver := BrowserSolver{Session: session}
	if _, err = solver.Sentinel(ctx, srv.URL+"/page", srv.URL+"/flaky.js", "login"); err == nil {
		t.Fatal("failed SDK loader reported success")
	}
	result, err := solver.Sentinel(ctx, srv.URL+"/page", srv.URL+"/flaky.js", "login")
	if err != nil || !result.Solved || result.SessionObserverStatus != "failed" {
		t.Fatalf("SDK recovery/observer policy: %v %s", err, result.SessionObserverStatus)
	}
	if flaky.Load() != 2 || pages.Load() < 2 {
		t.Fatal("failed loader runtime was not replaced")
	}
	strict := solver
	strict.RequireSessionObserver = true
	if _, err = strict.Sentinel(ctx, srv.URL+"/page", srv.URL+"/flaky.js", "login"); err == nil || strings.Contains(err.Error(), "fixture-secret-never-log") {
		t.Fatal("required observer failure was hidden or unsafe")
	}
	result, err = solver.Sentinel(ctx, srv.URL+"/page", srv.URL+"/rotating.js", "login")
	if err != nil || !strings.Contains(result.Token, `"version":1`) {
		t.Fatalf("loader change: %v", err)
	}
	if _, err = session.Evaluate(ctx, `window.__tosub2SentinelRuntime.loadedAt=1`); err != nil {
		t.Fatal(err)
	}
	result, err = solver.Sentinel(ctx, srv.URL+"/page", srv.URL+"/rotating.js", "login")
	if err != nil || !strings.Contains(result.Token, `"version":2`) || rotating.Load() != 2 {
		t.Fatalf("stale loader cache was reused: %v requests=%d", err, rotating.Load())
	}
}

func TestChromiumCanceledSentinelAbortsFetchAndRecovers(t *testing.T) {
	if os.Getenv("TOSUB2_BROWSER_TEST") != "1" {
		t.Skip("set TOSUB2_BROWSER_TEST=1 for real Chromium SDK cancellation fixture")
	}
	path := os.Getenv("CHROME_PATH")
	if path == "" {
		path = browser.DefaultChrome()
	}
	if path == "" {
		t.Skip("Chromium not installed")
	}
	var slow atomic.Bool
	slow.Store(true)
	entered, closed := make(chan struct{}, 1), make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/page":
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, "<!doctype html><body>authorized cancellation fixture</body>")
		case "/loader.js":
			w.Header().Set("Content-Type", "application/javascript")
			w.Header().Set("Cache-Control", "no-store")
			if slow.Load() {
				fmt.Fprint(w, `window.SentinelSDK={token:async flow=>await fetch('/slow').then(r=>r.text())}`)
			} else {
				fmt.Fprint(w, `window.SentinelSDK={token:async flow=>JSON.stringify({p:'fixture',t:'fixture',c:'fixture',id:'fixture-device',flow})}`)
			}
		case "/slow":
			entered <- struct{}{}
			<-r.Context().Done()
			closed <- struct{}{}
		default:
			w.WriteHeader(204)
		}
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	manager := browser.NewManager(path, 1)
	defer manager.Close()
	session, err := manager.Start(ctx, "", "", "", "fixture-device")
	if err != nil {
		t.Fatal(err)
	}
	solver := BrowserSolver{Session: session, Timeout: 300 * time.Millisecond}
	_, err = solver.Sentinel(ctx, server.URL+"/page", server.URL+"/loader.js", "first_flow")
	if err == nil || (!errors.Is(err, context.DeadlineExceeded) && !strings.Contains(err.Error(), "token_timeout")) {
		t.Fatalf("SDK timeout classification: %v", err)
	}
	select {
	case <-entered:
	default:
		t.Fatal("slow SDK network request not exercised")
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("abandoned SDK fetch survived context timeout")
	}
	slow.Store(false)
	solver.Timeout = 3 * time.Second
	result, err := solver.Sentinel(ctx, server.URL+"/page", server.URL+"/loader.js", "second_flow")
	if err != nil || !result.Solved || !strings.Contains(result.Token, "second_flow") {
		t.Fatalf("fresh SDK call failed after timeout: %v", err)
	}
	if session.BoundDeviceID() != "fixture-device" {
		t.Fatal("SDK recovery changed session device")
	}
}
