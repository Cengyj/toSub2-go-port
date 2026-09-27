package protocol

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"tosub2-go-port/internal/browser"
	"tosub2-go-port/internal/challenge"
	"tosub2-go-port/internal/transport"
)

// This local fixture exercises actual Go HTTP, Chromium navigation and CDP
// request replay. It never contacts an upstream account or CAPTCHA service.
func TestChromiumProtocolReadinessDoesNotAcceptRejectedReplay(t *testing.T) {
	if os.Getenv("TOSUB2_BROWSER_TEST") != "1" {
		t.Skip("set TOSUB2_BROWSER_TEST=1 for real Chromium protocol rejection integration")
	}
	chrome := os.Getenv("CHROME_PATH")
	if chrome == "" {
		chrome = browser.DefaultChrome()
	}
	if chrome == "" {
		t.Skip("Chromium not installed; set CHROME_PATH")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	manager := browser.NewManager(chrome, 1)
	defer manager.Close()
	session, err := manager.Start(ctx, "", "", "chromium-native", "local-protocol-rejection-fixture")
	if err != nil {
		t.Fatalf("start Chromium fixture: %v", err)
	}
	browserIdentity := session.Identity()
	var postRequests, documentRequests, goRequests atomic.Int32
	var firstInvocation atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/accounts/protected" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "text/html")
		if r.Method == http.MethodGet {
			documentRequests.Add(1)
			if r.UserAgent() != browserIdentity.UserAgent {
				t.Error("document navigation did not use the bound Chromium identity")
			}
			if cookie, err := r.Cookie("fixture-go-session"); err != nil || cookie.Value != "imported" {
				t.Error("browser navigation lost the initial Go response cookie")
			}
			http.SetCookie(w, &http.Cookie{Name: "fixture-browser-session", Value: "bound", Path: "/", HttpOnly: true})
			fmt.Fprint(w, "<!doctype html><title>Local ready document</title><p>ready</p>")
			return
		}
		if r.Method != http.MethodPost {
			t.Error("unexpected protected-request method")
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil || string(body) != `{"action":"fixture"}` {
			t.Error("protected request body changed during replay")
		}
		switch postRequests.Add(1) {
		case 1:
			if r.UserAgent() != transport.NativeUserAgent {
				t.Error("initial request was not sent by the Go HTTP client")
			}
			firstInvocation.Store(r.Header.Get("X-Access-Flow-Invocation-Id"))
			http.SetCookie(w, &http.Cookie{Name: "fixture-go-session", Value: "imported", Path: "/", HttpOnly: true})
			w.Header().Set("Cf-Mitigated", "challenge")
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, "<!doctype html><title>Just a moment</title>")
		case 2:
			if r.UserAgent() != browserIdentity.UserAgent {
				t.Error("protected replay did not use the bound Chromium identity")
			}
			if invocation, _ := firstInvocation.Load().(string); invocation == "" || r.Header.Get("X-Access-Flow-Invocation-Id") != invocation {
				t.Error("logical invocation changed during protected replay")
			}
			for name, value := range map[string]string{"fixture-go-session": "imported", "fixture-browser-session": "bound"} {
				if cookie, err := r.Cookie(name); err != nil || cookie.Value != value {
					t.Error("protected replay lost same-browser session cookies")
				}
			}
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, "<!doctype html><title>Access denied</title><p>Permission denied</p>")
		default:
			t.Error("ordinary rejection caused another protected-request replay")
			w.WriteHeader(http.StatusForbidden)
		}
	}))
	defer server.Close()
	jar := transport.NewCookieJar()
	goHTTP, err := transport.NewNativeClient("", jar)
	if err != nil {
		t.Fatal(err)
	}
	defer goHTTP.Close()
	client := NewClient(doerFunc(func(ctx context.Context, request transport.Request) (*transport.Response, error) {
		goRequests.Add(1)
		return goHTTP.Do(ctx, request)
	}), jar, transport.NativeProfile)
	client.UserAgent = transport.NativeUserAgent
	client.ConnectionRetries = 3
	bound := browser.NewTransport(session, jar)
	challengeCalls := 0
	var readiness challenge.Result
	client.Challenge = func(ctx context.Context, target string) error {
		challengeCalls++
		if err := bound.ImportCookies(ctx); err != nil {
			return err
		}
		client.Transport = bound
		client.UserAgent = browserIdentity.UserAgent
		client.AcceptLanguage = browserIdentity.Language
		var err error
		readiness, err = (challenge.BrowserSolver{Session: session, Timeout: 5 * time.Second}).SolveContext(ctx, target)
		if err != nil {
			return err
		}
		if !readiness.RetryReady || readiness.Solved || !readiness.SessionBound || readiness.Token != "" {
			return errors.New("fixture document did not yield bound replay readiness")
		}
		return bound.ExportCookies(ctx)
	}
	result, err := client.JSON(ctx, http.MethodPost, server.URL+"/api/accounts/protected", map[string]string{"action": "fixture"}, RequestOptions{}, nil)
	var rejected *HTTPError
	if !errors.As(err, &rejected) || rejected.StatusCode != http.StatusForbidden || errors.Is(err, ErrBrowserChallenge) || result == nil || result.StatusCode != http.StatusForbidden {
		t.Fatalf("ready document incorrectly accepted the protected rejection: response=%v err=%v", result, err)
	}
	if !readiness.RetryReady || readiness.Solved || challengeCalls != 1 || goRequests.Load() != 1 || postRequests.Load() != 2 || documentRequests.Load() != 1 {
		t.Fatalf("unexpected readiness or retry counts: ready=%v solved=%v callback=%d go=%d protected=%d document=%d", readiness.RetryReady, readiness.Solved, challengeCalls, goRequests.Load(), postRequests.Load(), documentRequests.Load())
	}
	if client.Transport != bound || client.UserAgent != browserIdentity.UserAgent || bound.Session != session {
		t.Fatal("rejected replay changed the browser binding or returned to Go HTTP")
	}
}
