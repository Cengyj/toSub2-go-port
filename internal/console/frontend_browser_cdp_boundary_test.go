package console

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	http "github.com/bogdanfinn/fhttp"
	"github.com/bogdanfinn/fhttp/httptest"
	"github.com/bogdanfinn/websocket"
)

type frontendBrowserCDPFixture struct {
	client   *frontendBrowserCDP
	socket   *websocket.Conn
	requests chan frontendBrowserCDPMessage
	ctx      context.Context
}

func newFrontendBrowserCDPFixture(t *testing.T) *frontendBrowserCDPFixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	fixture := &frontendBrowserCDPFixture{ctx: ctx, requests: make(chan frontendBrowserCDPMessage, 16)}
	connected := make(chan *websocket.Conn, 1)
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/devtools/browser/test-owned-page" {
			http.NotFound(w, r)
			return
		}
		connection, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer connection.Close()
		connected <- connection
		for {
			var message frontendBrowserCDPMessage
			if connection.ReadJSON(&message) != nil {
				return
			}
			select {
			case fixture.requests <- message:
			case <-ctx.Done():
				return
			}
		}
	}))
	profile := t.TempDir()
	address, _ := url.Parse(server.URL)
	if err := os.WriteFile(filepath.Join(profile, "DevToolsActivePort"), []byte(address.Port()+"\n/devtools/browser/test-owned-page\n"), 0600); err != nil {
		server.Close()
		cancel()
		t.Fatal(err)
	}
	client, err := connectFrontendBrowserCDP(ctx, profile)
	if err != nil {
		server.Close()
		cancel()
		t.Fatal(err)
	}
	fixture.client, fixture.socket = client, <-connected
	t.Cleanup(func() { cancel(); client.close(); server.Close() })
	return fixture
}

func (fixture *frontendBrowserCDPFixture) next(t *testing.T, method string) frontendBrowserCDPMessage {
	t.Helper()
	select {
	case message := <-fixture.requests:
		if message.Method != method || message.ID <= 0 {
			t.Fatalf("unexpected fixture command %q; want %q with an ID", message.Method, method)
		}
		return message
	case <-fixture.ctx.Done():
		t.Fatal("synthetic debugger command was not dispatched")
		return frontendBrowserCDPMessage{}
	}
}

func (fixture *frontendBrowserCDPFixture) send(t *testing.T, message any) {
	t.Helper()
	if err := fixture.socket.WriteJSON(message); err != nil {
		t.Fatal("synthetic debugger response failed")
	}
}

func (fixture *frontendBrowserCDPFixture) reply(t *testing.T, request frontendBrowserCDPMessage, result any) {
	t.Helper()
	fixture.send(t, map[string]any{"id": request.ID, "sessionId": request.SessionID, "result": result})
}

func TestFrontendBrowserCDPEndpointAndFreshProfileBoundaries(t *testing.T) {
	const private = "SYNTHETIC_PRIVATE_ENDPOINT"
	for _, raw := range []string{"", "0\n/devtools/browser/a", "65536\n/devtools/browser/a", "0123\n/devtools/browser/a", "123\n/devtools/page/a", "123\n/devtools/browser/", "123\n/devtools/browser/../" + private, "123\n/devtools/browser/a?host=" + private, "123\n/devtools/browser/a\n" + private, strings.Repeat(private, 100)} {
		if address, err := frontendBrowserCDPEndpoint([]byte(raw)); err == nil || address != "" || strings.Contains(err.Error(), private) {
			t.Fatal("invalid endpoint was accepted or leaked into diagnostics")
		}
	}
	if address, err := frontendBrowserCDPEndpoint([]byte("12345\n/devtools/browser/123-abc\n")); err != nil || address != "ws://127.0.0.1:12345/devtools/browser/123-abc" {
		t.Fatal("private loopback endpoint was rejected")
	}
	profile := t.TempDir()
	options := frontendBrowserPacedOptions{Executable: filepath.Join(profile, "synthetic-browser"), Profile: profile, URL: "http://127.0.0.1:12345/", Terminal: make(chan struct{})}
	if err := validateFrontendBrowserPacedOptions(options); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"https://127.0.0.1:12345/", "http://localhost:12345/", "http://192.0.2.1:12345/", "http://user@127.0.0.1:12345/", "http://127.0.0.1:12345/#foreign", "http://127.0.0.1:12345"} {
		altered := options
		altered.URL = value
		if err := validateFrontendBrowserPacedOptions(altered); err == nil {
			t.Fatal("fixture accepted a noncanonical or non-loopback destination")
		}
	}
	if err := os.WriteFile(filepath.Join(profile, "existing-profile"), []byte(private), 0600); err != nil {
		t.Fatal(err)
	}
	if err := validateFrontendBrowserPacedOptions(options); err == nil {
		t.Fatal("fixture reused a nonempty browser profile")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := connectFrontendBrowserCDP(ctx, t.TempDir()); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled readiness wait continued looking for a browser")
	}
}

func TestFrontendBrowserCDPWaitsForCompletePrivateRecord(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	attempted := make(chan struct{}, 1)
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempted <- struct{}{}
		connection, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer connection.Close()
		_, _, _ = connection.ReadMessage()
	}))
	defer server.Close()
	profile := t.TempDir()
	path := filepath.Join(profile, "DevToolsActivePort")
	address, _ := url.Parse(server.URL)
	if err := os.WriteFile(path, []byte(address.Port()+"\n/devtools/browser/"), 0600); err != nil {
		t.Fatal(err)
	}
	type outcome struct {
		client *frontendBrowserCDP
		err    error
	}
	completed := make(chan outcome, 1)
	go func() {
		client, err := connectFrontendBrowserCDP(ctx, profile)
		completed <- outcome{client, err}
	}()
	select {
	case <-attempted:
		t.Fatal("partial record was used as a debugger endpoint")
	case result := <-completed:
		if result.client != nil {
			result.client.close()
		}
		t.Fatal("partial record ended the bounded readiness wait")
	case <-time.After(25 * time.Millisecond):
	}
	if err := os.WriteFile(path, []byte(address.Port()+"\n/devtools/browser/owned-complete-record\n"), 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-completed:
		if result.err != nil || result.client == nil {
			t.Fatal("completed private record was not accepted")
		}
		result.client.close()
	case <-ctx.Done():
		t.Fatal("readiness failed to observe a completed private record")
	}
}

func TestFrontendBrowserVirtualClockBudgetAndNoCatchup(t *testing.T) {
	started := time.Now()
	var clock frontendBrowserVirtualClock
	if err := clock.grant(started); err != nil {
		t.Fatal(err)
	}
	if err := clock.grant(started.Add(time.Second)); err == nil || clock.GrantedMS != 25 {
		t.Fatal("a new CDP policy replaced an unexpired budget")
	}
	if err := clock.expire(); err != nil {
		t.Fatal(err)
	}
	if clock.delay(started.Add(24*time.Millisecond)) != time.Millisecond || clock.grant(started.Add(24*time.Millisecond)) == nil {
		t.Fatal("virtual time was granted before its real interval elapsed")
	}
	for clock.GrantedMS < frontendBrowserVirtualBudgetMS {
		when := started.Add(time.Duration(clock.GrantedMS) * time.Millisecond)
		if err := clock.grant(when); err != nil {
			t.Fatal(err)
		}
		if err := clock.expire(); err != nil {
			t.Fatal(err)
		}
	}
	if clock.GrantedMS != 16000 || clock.ExpiredMS != 16000 || !errors.Is(clock.grant(started.Add(time.Hour)), errFrontendBrowserBudget) {
		t.Fatal("virtual clock exceeded its original total budget")
	}
	if err := clock.expire(); err == nil {
		t.Fatal("an unowned completion changed budget accounting")
	}
	var delayed frontendBrowserVirtualClock
	if delayed.grant(started) != nil || delayed.expire() != nil || delayed.grant(started.Add(time.Hour)) != nil {
		t.Fatal("clock setup failed")
	}
	if delayed.expire() != nil || delayed.delay(started.Add(time.Hour+time.Millisecond)) != 24*time.Millisecond {
		t.Fatal("time spent waiting was banked into a fast-forward burst")
	}
}

func TestFrontendBrowserCDPCorrelatesResponsesAndCancellation(t *testing.T) {
	fixture := newFrontendBrowserCDPFixture(t)
	type outcome struct {
		session string
		value   string
		err     error
	}
	results := make(chan outcome, 2)
	for _, session := range []string{"first", "second"} {
		go func() {
			var result struct{ Value string }
			err := fixture.client.call(fixture.ctx, session, "Fixture.read", struct{}{}, &result)
			results <- outcome{session, result.Value, err}
		}()
	}
	one, two := fixture.next(t, "Fixture.read"), fixture.next(t, "Fixture.read")
	fixture.reply(t, two, map[string]string{"Value": two.SessionID})
	fixture.reply(t, one, map[string]string{"Value": one.SessionID})
	for range 2 {
		result := <-results
		if result.err != nil || result.value != result.session {
			t.Fatal("out-of-order debugger results crossed requests or sessions")
		}
	}
	cancelCtx, cancel := context.WithCancel(fixture.ctx)
	cancelled := make(chan error, 1)
	go func() { cancelled <- fixture.client.call(cancelCtx, "old", "Fixture.cancel", struct{}{}, nil) }()
	stale := fixture.next(t, "Fixture.cancel")
	cancel()
	if err := <-cancelled; !errors.Is(err, context.Canceled) {
		t.Fatal("debugger call ignored cancellation")
	}
	fixture.reply(t, stale, map[string]bool{"ok": true})
	go func() { cancelled <- fixture.client.call(fixture.ctx, "current", "Fixture.read", struct{}{}, nil) }()
	current := fixture.next(t, "Fixture.read")
	fixture.send(t, map[string]any{"id": current.ID, "sessionId": "old", "result": map[string]bool{"ok": true}})
	if err := <-cancelled; !errors.Is(err, errFrontendBrowserCDPIdentity) {
		t.Fatal("a foreign session acknowledged a current request")
	}
	go func() { cancelled <- fixture.client.call(fixture.ctx, "current", "Fixture.read", struct{}{}, nil) }()
	current = fixture.next(t, "Fixture.read")
	fixture.send(t, map[string]any{"id": current.ID, "sessionId": current.SessionID, "error": map[string]any{"code": -1, "message": "SYNTHETIC_PRIVATE_TOKEN"}})
	if err := <-cancelled; err == nil || strings.Contains(err.Error(), "SYNTHETIC_PRIVATE") {
		t.Fatal("debugger error was ignored or leaked its private message")
	}
	fixture.client.mu.Lock()
	pending := len(fixture.client.pending)
	fixture.client.mu.Unlock()
	if pending != 0 {
		t.Fatal("completed or cancelled debugger requests retained their waiters")
	}
}

func TestFrontendBrowserClockRejectsForeignEventsAndCancels(t *testing.T) {
	fixture := newFrontendBrowserCDPFixture(t)
	ctx, cancel := context.WithCancel(fixture.ctx)
	defer cancel()
	terminal := make(chan struct{})
	var clock frontendBrowserVirtualClock
	finished := make(chan error, 1)
	go func() {
		finished <- advanceFrontendBrowserClock(ctx, fixture.client, "owned-session", "owned-target", terminal, &clock)
	}()
	first := fixture.next(t, "Emulation.setVirtualTimePolicy")
	var policy struct {
		Policy string `json:"policy"`
		Budget int    `json:"budget"`
	}
	if json.Unmarshal(first.Params, &policy) != nil || policy.Policy != "pauseIfNetworkFetchesPending" || policy.Budget != 25 {
		t.Fatal("driver changed the virtual slice or network readiness policy")
	}
	fixture.reply(t, first, struct{}{})
	fixture.send(t, map[string]any{"sessionId": "foreign-session", "method": "Emulation.virtualTimeBudgetExpired", "params": struct{}{}})
	select {
	case <-fixture.requests:
		t.Fatal("foreign completion advanced the owned fixture clock")
	case <-time.After(2 * frontendBrowserVirtualSliceMS * time.Millisecond):
	}
	fixture.send(t, map[string]any{"sessionId": "owned-session", "method": "Emulation.virtualTimeBudgetExpired", "params": struct{}{}})
	second := fixture.next(t, "Emulation.setVirtualTimePolicy")
	fixture.reply(t, second, struct{}{})
	cancel()
	if err := <-finished; !errors.Is(err, context.Canceled) || clock.GrantedMS != 50 || clock.ExpiredMS != 25 {
		t.Fatal("cancelled clock lost dispatch accounting or continued after cancellation")
	}
}

func TestFrontendBrowserClockStopsWhenItsTargetDetaches(t *testing.T) {
	fixture := newFrontendBrowserCDPFixture(t)
	var clock frontendBrowserVirtualClock
	finished := make(chan error, 1)
	go func() {
		finished <- advanceFrontendBrowserClock(fixture.ctx, fixture.client, "owned-session", "owned-target", make(chan struct{}), &clock)
	}()
	request := fixture.next(t, "Emulation.setVirtualTimePolicy")
	fixture.reply(t, request, struct{}{})
	fixture.send(t, map[string]any{"method": "Target.detachedFromTarget", "params": map[string]string{"sessionId": "foreign-session", "targetId": "foreign-target"}})
	fixture.send(t, map[string]any{"method": "Target.detachedFromTarget", "params": map[string]string{"sessionId": "owned-session", "targetId": "owned-target"}})
	if err := <-finished; !errors.Is(err, errFrontendBrowserCDPIdentity) || clock.GrantedMS != 25 || clock.ExpiredMS != 0 {
		t.Fatal("detached page continued or inherited another target's clock")
	}
}

func TestFrontendBrowserTerminalFailureCannotBecomeLateSuccess(t *testing.T) {
	fixture := newFrontendBrowserCDPFixture(t)
	capture := newProtectedSettingsBrowserCapture()
	var clock frontendBrowserVirtualClock
	finished := make(chan error, 1)
	go func() {
		finished <- advanceFrontendBrowserClock(fixture.ctx, fixture.client, "owned-session", "owned-target", capture.done, &clock)
	}()
	request := fixture.next(t, "Emulation.setVirtualTimePolicy")
	fixture.reply(t, request, struct{}{})
	failed := protectedBrowserTestReport("preview_requested", "iframe_mounted", "script_started")
	raw, _ := json.Marshal(failed)
	capture.accept(protectedSettingsBrowserResult{OK: false, Error: "static wait timeout", Diagnostics: raw})
	capture.accept(protectedSettingsBrowserResult{OK: true, Diagnostics: json.RawMessage(`{"status":"passed"}`)})
	if err := <-finished; err != nil || clock.GrantedMS != 25 || clock.ExpiredMS != 0 {
		t.Fatal("terminal clock stop lost the final in-flight grant")
	}
	fixture.send(t, map[string]any{"sessionId": "owned-session", "method": "Emulation.virtualTimeBudgetExpired", "params": struct{}{}})
	if capture.result.OK || capture.result.Error != "static wait timeout" || string(capture.result.Diagnostics) != string(raw) {
		t.Fatal("late success replaced the terminal failure")
	}
	var stopped frontendBrowserVirtualClock
	if err := advanceFrontendBrowserClock(fixture.ctx, fixture.client, "owned-session", "owned-target", capture.done, &stopped); err != nil || stopped.GrantedMS != 0 {
		t.Fatal("a terminal receipt restarted the clock")
	}
}

func TestFrontendBrowserDOMSnapshotBindsRootFrameAndURL(t *testing.T) {
	for _, mode := range []string{"frame-changed", "subframe", "navigation-changed", "evaluation-changed", "script-exception", "valid"} {
		t.Run(mode, func(t *testing.T) {
			fixture := newFrontendBrowserCDPFixture(t)
			const fixtureURL = "http://127.0.0.1:12345/"
			type outcome struct {
				dom []byte
				err error
			}
			finished := make(chan outcome, 1)
			go func() {
				dom, err := frontendBrowserDOMSnapshot(fixture.ctx, fixture.client, "owned-session", "owned-root", fixtureURL)
				finished <- outcome{dom, err}
			}()
			request := fixture.next(t, "Page.getFrameTree")
			frame := map[string]string{"id": "owned-root", "url": fixtureURL}
			switch mode {
			case "frame-changed":
				frame["id"] = "replacement-root"
			case "subframe":
				frame["parentId"] = "another-root"
			case "navigation-changed":
				frame["url"] = "http://127.0.0.1:12345/replaced"
			}
			fixture.reply(t, request, map[string]any{"frameTree": map[string]any{"frame": frame}})
			if mode == "evaluation-changed" || mode == "script-exception" || mode == "valid" {
				request = fixture.next(t, "Runtime.evaluate")
				value := map[string]string{"url": fixtureURL, "dom": strings.Repeat("x", 65540)}
				if mode == "evaluation-changed" {
					value["url"] = fixtureURL + "replaced"
				}
				result := map[string]any{"result": map[string]any{"type": "object", "value": value}}
				if mode == "script-exception" {
					result["exceptionDetails"] = map[string]string{"text": "SYNTHETIC_PRIVATE_EXCEPTION"}
				}
				fixture.reply(t, request, result)
			}
			observed := <-finished
			if mode == "valid" {
				if observed.err != nil || len(observed.dom) != 64*1024 {
					t.Fatal("valid snapshot was lost or exceeded the original retention bound")
				}
			} else if !errors.Is(observed.err, errFrontendBrowserCDPIdentity) || len(observed.dom) != 0 || strings.Contains(observed.err.Error(), "SYNTHETIC_PRIVATE") {
				t.Fatal("foreign, replaced or failed document became accepted DOM evidence")
			}
		})
	}
}
