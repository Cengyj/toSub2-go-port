package browser

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
)

func TestVisibleChromiumSupportsWindowlessSameSessionTarget(t *testing.T) {
	if os.Getenv("TOSUB2_BROWSER_TEST") != "1" || os.Getenv("TOSUB2_VISIBLE_BROWSER_TEST") != "1" {
		t.Skip("set TOSUB2_BROWSER_TEST=1 and TOSUB2_VISIBLE_BROWSER_TEST=1 for a new local visible Chromium fixture")
	}
	t.Setenv("TOSUB2_BROWSER_HEADLESS", "0")
	session, ctx := integrationSession(t)
	mainID := session.pageEndpoint[strings.LastIndex(session.pageEndpoint, "/")+1:]
	mainWindow, err := session.call(ctx, "Browser.getWindowForTarget", map[string]any{"targetId": mainID})
	if err != nil {
		t.Fatalf("main fixture has no visible window: %v", err)
	}
	raw, err := session.call(ctx, "Target.createTarget", map[string]any{"url": "about:blank", "background": true, "hidden": true})
	if err != nil {
		t.Fatalf("hidden target unsupported: %v", err)
	}
	var created struct {
		ID string `json:"targetId"`
	}
	if err = json.Unmarshal(raw, &created); err != nil || created.ID == "" {
		t.Fatalf("hidden target ID missing: %v", err)
	}
	defer session.closeRequestPage(nil, created.ID)
	_, err = session.call(ctx, "Browser.getWindowForTarget", map[string]any{"targetId": created.ID})
	var failure *cdpError
	if errors.As(err, &failure) {
		t.Logf("local hidden fixture window lookup: code=%d category=%q", failure.Code, failure.Message)
	}
	if !errors.As(err, &failure) || (!strings.Contains(strings.ToLower(failure.Message), "no window") && !strings.Contains(strings.ToLower(failure.Message), "window not found")) {
		t.Fatalf("hidden target unexpectedly has a UI window: %v", err)
	}
	var window struct {
		ID int `json:"windowId"`
	}
	if err = json.Unmarshal(mainWindow, &window); err != nil || window.ID == 0 {
		t.Fatalf("main visible window missing: %v", err)
	}
	t.Logf("%s: main target has visible window; hidden request target has no window (%d)", session.Identity().BrowserVersion, failure.Code)
}
