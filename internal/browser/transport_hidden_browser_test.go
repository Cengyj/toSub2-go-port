package browser

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"tosub2-go-port/internal/transport"
)

func fixtureWindowTargets(t *testing.T, ctx context.Context, session *Session) (visible, hidden int) {
	t.Helper()
	raw, err := session.call(ctx, "Target.getTargets", nil)
	if err != nil {
		t.Fatal("cannot inspect owned fixture targets:", err)
	}
	var result struct {
		Targets []struct {
			ID   string `json:"targetId"`
			Type string `json:"type"`
		} `json:"targetInfos"`
	}
	if err = json.Unmarshal(raw, &result); err != nil {
		t.Fatal("cannot decode fixture target identifiers:", err)
	}
	for _, target := range result.Targets {
		// Chromium exposes hidden request targets as "other", while normal
		// tab-strip pages use "page". Browser UI/service workers are neither.
		if target.Type != "page" && target.Type != "other" {
			continue
		}
		_, err := session.call(ctx, "Browser.getWindowForTarget", map[string]any{"targetId": target.ID})
		if err == nil {
			visible++
			continue
		}
		var noWindow *cdpError
		if !errors.As(err, &noWindow) || noWindow.Code != -32000 || noWindow.Message != "Browser window not found" {
			t.Fatal("cannot establish fixture target window ownership:", err)
		}
		hidden++
	}
	return visible, hidden
}

func TestVisibleChromiumProtocolRequestsNeverCreateVisibleTabs(t *testing.T) {
	if os.Getenv("TOSUB2_BROWSER_TEST") != "1" || os.Getenv("TOSUB2_VISIBLE_BROWSER_TEST") != "1" {
		t.Skip("set TOSUB2_BROWSER_TEST=1 and TOSUB2_VISIBLE_BROWSER_TEST=1 for local windowless request target regression")
	}
	t.Setenv("TOSUB2_BROWSER_HEADLESS", "0")
	entered, release := make(chan struct{}, 1), make(chan struct{}, 1)
	slowEntered, slowClosed := make(chan struct{}, 1), make(chan struct{}, 1)
	var invalidRequests, followed atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/manual":
			w.Header().Set("Content-Type", "text/html")
			_, _ = io.WriteString(w, "<!doctype html><title>toSub2 local manual fixture</title><p>Local fixture only</p>")
		case "/request":
			body, _ := io.ReadAll(r.Body)
			cookie, err := r.Cookie("fixture-owner")
			if r.Method != "POST" || string(body) != "local-fixture-post" || err != nil || cookie.Value != "same-browser" {
				invalidRequests.Add(1)
			}
			entered <- struct{}{}
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
			w.Header().Add("Set-Cookie", "fixture-first=one; Path=/; HttpOnly")
			w.Header().Add("Set-Cookie", "fixture-second=two; Path=/")
			w.Header().Set("Location", "/must-not-follow")
			w.WriteHeader(http.StatusFound)
			_, _ = io.WriteString(w, "local fixture redirect")
		case "/must-not-follow":
			followed.Add(1)
			w.WriteHeader(http.StatusInternalServerError)
		case "/slow":
			slowEntered <- struct{}{}
			<-r.Context().Done()
			slowClosed <- struct{}{}
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	t.Cleanup(server.Close)
	session, ctx := integrationSession(t)
	if err := session.Navigate(ctx, server.URL+"/manual"); err != nil {
		t.Fatal(err)
	}
	if err := session.Present(ctx); err != nil {
		t.Fatal(err)
	}
	if err := session.SetCookiesContext(ctx, []map[string]any{{"name": "fixture-owner", "value": "same-browser", "url": server.URL, "path": "/", "httpOnly": true}}); err != nil {
		t.Fatal(err)
	}
	if _, err := session.Evaluate(ctx, "window.fixtureHiddenEvents=0;document.addEventListener('visibilitychange',()=>{if(document.hidden)window.fixtureHiddenEvents++})"); err != nil {
		t.Fatal(err)
	}
	if visible, hidden := fixtureWindowTargets(t, ctx, session); visible != 1 || hidden != 0 {
		t.Fatalf("unexpected initial owned targets: window=%d hidden=%d", visible, hidden)
	}
	client := NewTransport(session, transport.NewCookieJar())
	type outcome struct {
		response *transport.Response
		err      error
	}
	for i := 0; i < 6; i++ {
		done := make(chan outcome, 1)
		go func() {
			response, err := client.Do(ctx, transport.Request{Method: http.MethodPost, URL: server.URL + "/request", Body: []byte("local-fixture-post"), Headers: http.Header{"Content-Type": {"text/plain"}}, Timeout: 5 * time.Second})
			done <- outcome{response, err}
		}()
		select {
		case <-entered:
		case result := <-done:
			t.Fatalf("fixture POST did not arrive: %v", result.err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		if visible, hidden := fixtureWindowTargets(t, ctx, session); visible != 1 || hidden != 1 {
			t.Fatalf("request %d added a UI tab or escaped its target bound: window=%d hidden=%d", i+1, visible, hidden)
		}
		release <- struct{}{}
		result := <-done
		if result.err != nil || result.response == nil || result.response.StatusCode != http.StatusFound || result.response.Header.Get("Location") != "/must-not-follow" || len(result.response.RawSetCookie) != 2 {
			t.Fatalf("request %d lost manual redirect or raw cookie semantics: %v", i+1, result.err)
		}
		if visible, hidden := fixtureWindowTargets(t, ctx, session); visible != 1 || hidden != 0 {
			t.Fatalf("request %d left a target behind: window=%d hidden=%d", i+1, visible, hidden)
		}
	}
	if invalidRequests.Load() != 0 || followed.Load() != 0 || !client.Jar.Has("fixture-first") || !client.Jar.Has("fixture-second") {
		t.Fatal("hidden requests changed method/body, crossed cookie context, followed redirect, or lost response cookies")
	}
	requestCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan outcome, 1)
	go func() {
		response, err := client.Do(requestCtx, transport.Request{URL: server.URL + "/slow", Timeout: 5 * time.Second})
		done <- outcome{response, err}
	}()
	select {
	case <-slowEntered:
	case result := <-done:
		t.Fatalf("slow fixture request did not arrive: %v", result.err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if visible, hidden := fixtureWindowTargets(t, ctx, session); visible != 1 || hidden != 1 {
		t.Fatalf("cancelable request has a UI tab: window=%d hidden=%d", visible, hidden)
	}
	cancel()
	result := <-done
	if !errors.Is(result.err, context.Canceled) || session.closed.Load() {
		t.Fatalf("request cancellation lost its cause or closed the manual session: %v", result.err)
	}
	select {
	case <-slowClosed:
	case <-time.After(2 * time.Second):
		t.Fatal("canceled hidden target left its HTTP request running")
	}
	if visible, hidden := fixtureWindowTargets(t, ctx, session); visible != 1 || hidden != 0 {
		t.Fatalf("canceled request left a target behind: window=%d hidden=%d", visible, hidden)
	}
	value, err := session.Evaluate(ctx, "document.title+':'+document.visibilityState+':'+window.fixtureHiddenEvents")
	if err != nil || value != "toSub2 local manual fixture:visible:0" {
		t.Fatal("protocol traffic replaced or hid the manual verification page:", err)
	}
	t.Logf("%s: six hidden POST/302 requests plus cancellation retained one visible manual page; no upstream or account accessed", session.Identity().BrowserVersion)
}
