package browser

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestHeadlessPresentCannotChangeModeFromEnvironment(t *testing.T) {
	session := &Session{headless: true}
	t.Setenv("TOSUB2_BROWSER_HEADLESS", "0")
	err := session.Present(context.Background())
	if !errors.Is(err, ErrInteractiveBrowserRequired) {
		t.Fatalf("running headless session must require a new visible session: %v", err)
	}
	for _, detail := range []string{"cannot become visible", "restart", "new browser session", "container", "desktop/display", "macOS", "TOSUB2_BROWSER_HEADLESS=0"} {
		if !strings.Contains(err.Error(), detail) {
			t.Errorf("interactive error omits %q", detail)
		}
	}
}

// This opens an actual desktop window only when explicitly requested. The page
// is an httptest fixture and contains no upstream challenge or account data.
func TestVisibleChromiumPresentLocalFixtureAndCleanup(t *testing.T) {
	if os.Getenv("TOSUB2_BROWSER_TEST") != "1" || os.Getenv("TOSUB2_VISIBLE_BROWSER_TEST") != "1" {
		t.Skip("set TOSUB2_BROWSER_TEST=1 and TOSUB2_VISIBLE_BROWSER_TEST=1 in an accessible graphical desktop for the local visible-window fixture")
	}
	path := os.Getenv("CHROME_PATH")
	if path == "" {
		path = DefaultChrome()
	}
	if path == "" {
		t.Fatal("visible-browser verification requires installed Chromium; set CHROME_PATH")
	}
	t.Setenv("TOSUB2_BROWSER_HEADLESS", "0")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "<!doctype html><title>toSub2 local visible-window test</title><p id='fixture'>Local fixture only; no account or upstream verification</p>")
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	manager := NewManager(path, 1)
	t.Cleanup(func() {
		if err := manager.Close(); err != nil {
			t.Error(err)
		}
	})
	session, err := manager.Start(ctx, "", "", "", "local-visible-fixture")
	if err != nil {
		t.Fatal(err)
	}
	if session.headless {
		t.Fatal("fixture unexpectedly started headless")
	}
	for _, arg := range session.cmd.Args {
		if strings.HasPrefix(arg, "--headless") {
			t.Fatal("visible fixture passed a headless launch flag")
		}
	}
	if err = session.Navigate(ctx, server.URL); err != nil {
		t.Fatal(err)
	}
	if err = session.Present(ctx); err != nil {
		t.Fatal("local visible Chromium Present failed:", err)
	}
	value, err := session.Evaluate(ctx, "({visibility:document.visibilityState,text:document.querySelector('#fixture').textContent})")
	if err != nil {
		t.Fatal(err)
	}
	state, ok := value.(map[string]any)
	if !ok || state["visibility"] != "visible" || state["text"] != "Local fixture only; no account or upstream verification" {
		t.Fatalf("visible local document was not presented: %v", value)
	}
	profile := session.Dir
	t.Logf("local visible Chromium Present succeeded (%s); no upstream challenge was accessed", session.Identity().BrowserVersion)
	if err = session.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-session.processDone:
	case <-time.After(3 * time.Second):
		t.Fatal("owned visible Chromium process did not exit")
	}
	if _, err = os.Stat(profile); !os.IsNotExist(err) {
		t.Fatal("owned visible Chromium profile was not removed")
	}
}
