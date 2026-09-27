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
	"tosub2-go-port/internal/transport"
)

// These are local document fixtures, not upstream CAPTCHA or account tests.
// They exercise Chromium's real load lifecycle and main-document CDP evidence.
func TestChromiumVerificationDocumentCompletionAndRejection(t *testing.T) {
	if os.Getenv("TOSUB2_BROWSER_TEST") != "1" {
		t.Skip("set TOSUB2_BROWSER_TEST=1 for real Chromium document completion fixtures")
	}
	path := os.Getenv("CHROME_PATH")
	if path == "" {
		path = browser.DefaultChrome()
	}
	if path == "" {
		t.Skip("Chromium not installed; set CHROME_PATH")
	}
	var protectedRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "text/html")
		switch r.URL.Path {
		case "/html":
			fmt.Fprint(w, "<!doctype html><title>Normal document</title><p>ready</p>")
		case "/json":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, "{\"status\":\"ready\"}")
		case "/during-navigation", "/after-navigation":
			w.Header().Set("Cf-Mitigated", "challenge")
			w.WriteHeader(403)
			fmt.Fprint(w, "<!doctype html><title>Just a moment</title><div id=challenge-stage>fixture gate</div>")
			if r.URL.Path == "/during-navigation" {
				fmt.Fprint(w, "<script>document.querySelector('#challenge-stage').remove();document.title='Fixture complete'</script>")
			} else {
				fmt.Fprint(w, "<script>setTimeout(()=>{document.querySelector('#challenge-stage').remove();document.title='Fixture complete'},350)</script>")
			}
		case "/denied":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(403)
			fmt.Fprint(w, "{\"error\":\"refused\"}")
		case "/pending":
			w.Header().Set("Cf-Mitigated", "challenge")
			w.WriteHeader(403)
			fmt.Fprint(w, "<!doctype html><title>Just a moment</title><div id=challenge-stage>fixture gate</div>")
		case "/protected":
			if protectedRequests.Add(1) == 1 {
				http.SetCookie(w, &http.Cookie{Name: "fixture-session", Value: "same-browser", Path: "/", HttpOnly: true})
				w.Header().Set("Cf-Mitigated", "challenge")
				w.WriteHeader(403)
				fmt.Fprint(w, "<!doctype html><div id=challenge-stage>fixture gate</div><script>document.querySelector('#challenge-stage').remove()</script>")
				return
			}
			cookie, err := r.Cookie("fixture-session")
			if err != nil || cookie.Value != "same-browser" {
				w.WriteHeader(401)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, "{\"accepted\":true}")
		default:
			w.WriteHeader(204)
		}
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	manager := browser.NewManager(path, 1)
	defer manager.Close()
	session, err := manager.Start(ctx, "", "", "", "local-verification-fixture")
	if err != nil {
		t.Fatal(err)
	}
	for _, route := range []string{"html", "json", "during-navigation", "after-navigation"} {
		t.Run(route, func(t *testing.T) {
			result, err := (BrowserSolver{Session: session, Timeout: 3 * time.Second}).SolveContext(ctx, server.URL+"/"+route)
			if err != nil || !result.RetryReady || result.Solved || !result.SessionBound || result.Token != "" {
				t.Fatalf("document readiness is not upstream acceptance: result=%+v err=%v", result, err)
			}
		})
	}
	t.Run("plain-denial", func(t *testing.T) {
		started := time.Now()
		result, err := (BrowserSolver{Session: session, Timeout: 3 * time.Second}).SolveContext(ctx, server.URL+"/denied")
		if !errors.Is(err, ErrVerificationRejected) || errors.Is(err, ErrVerificationPending) || result.Solved || result.RetryReady || time.Since(started) > time.Second {
			t.Fatalf("plain denial incorrectly waited for a token: result=%+v err=%v", result, err)
		}
		if !strings.Contains(err.Error(), "HTTP 403") || !strings.Contains(err.Error(), "content=json") || !strings.Contains(err.Error(), "DOM=clear") {
			t.Fatalf("denial diagnostic missing: %v", err)
		}
	})
	t.Run("provider-still-pending", func(t *testing.T) {
		result, err := (BrowserSolver{Session: session, Timeout: 500 * time.Millisecond}).SolveContext(ctx, server.URL+"/pending")
		if !errors.Is(err, ErrVerificationPending) || !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrVerificationRejected) || result.Solved || result.RetryReady {
			t.Fatalf("manual verification timeout lost its classification: result=%+v err=%v", result, err)
		}
		for _, diagnostic := range []string{"HTTP 403", "content=html", "provider-challenge", "DOM=pending"} {
			if !strings.Contains(err.Error(), diagnostic) {
				t.Fatalf("missing diagnostic %q: %v", diagnostic, err)
			}
		}
	})
	t.Run("same-browser-protected-replay", func(t *testing.T) {
		result, err := (BrowserSolver{Session: session, Timeout: 3 * time.Second}).SolveContext(ctx, server.URL+"/protected")
		if err != nil || !result.RetryReady || result.Solved || protectedRequests.Load() != 1 {
			t.Fatalf("readiness claimed upstream acceptance: result=%+v err=%v requests=%d", result, err, protectedRequests.Load())
		}
		response, err := browser.NewTransport(session, transport.NewCookieJar()).Do(ctx, transport.Request{URL: server.URL + "/protected", Timeout: 3 * time.Second})
		if err != nil || response == nil || response.StatusCode != 200 || !strings.Contains(string(response.Body), "accepted") || protectedRequests.Load() != 2 {
			t.Fatalf("same-browser protected replay failed: response=%+v err=%v requests=%d", response, err, protectedRequests.Load())
		}
	})
}
