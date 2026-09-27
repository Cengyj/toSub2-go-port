package browser

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"tosub2-go-port/internal/transport"
)

func TestBrowserProtocolTargetsPreserveVerificationPageVisibility(t *testing.T) {
	s, ctx := integrationSession(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "<!doctype html><title>Local visibility fixture</title><body>ready</body>")
	}))
	defer server.Close()
	if err := s.Navigate(ctx, server.URL); err != nil {
		t.Fatal(err)
	}
	if _, err := s.call(ctx, "Page.bringToFront", nil); err != nil {
		t.Fatal(err)
	}
	value, err := s.Evaluate(ctx, "window.fixtureHiddenEvents=0;document.addEventListener('visibilitychange',()=>{if(document.hidden)window.fixtureHiddenEvents++});document.visibilityState")
	if err != nil || value != "visible" {
		t.Fatalf("verification fixture was not initially visible: %v value=%v", err, value)
	}
	page, targetID, err := s.newPage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer s.closeRequestPage(page, targetID)
	// Hidden targets can report document.visibilityState="visible" while they
	// have no browser window or tab-strip entry. Check the actual UI ownership.
	_, err = s.call(ctx, "Browser.getWindowForTarget", map[string]any{"targetId": targetID})
	var noWindow *cdpError
	if !errors.As(err, &noWindow) || noWindow.Code != -32000 || noWindow.Message != "Browser window not found" {
		t.Fatalf("protocol target has an unexpected UI window: %v", err)
	}
	if value, err = s.Evaluate(ctx, "document.visibilityState"); err != nil || value != "visible" {
		t.Fatalf("verification page was hidden by request target: %v value=%v", err, value)
	}
	response, err := NewTransport(s, nil).Do(ctx, transport.Request{URL: server.URL + "/protocol"})
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("background protocol request failed: %v", err)
	}
	value, err = s.Evaluate(ctx, "document.visibilityState+':'+window.fixtureHiddenEvents")
	if err != nil || value != "visible:0" {
		t.Fatalf("protocol request interrupted verification page: %v value=%v", err, value)
	}
}
