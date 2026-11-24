package console

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bogdanfinn/websocket"
)

const (
	frontendBrowserVirtualBudgetMS = 16000
	frontendBrowserVirtualSliceMS  = 25
)

var (
	errFrontendBrowserCDPClosed   = errors.New("fixture debugger disconnected")
	errFrontendBrowserCDPIdentity = errors.New("fixture debugger target identity changed")
	errFrontendBrowserBudget      = errors.New("fixture exhausted its existing virtual time budget")
)

type frontendBrowserCDPMessage struct {
	ID        int             `json:"id,omitempty"`
	SessionID string          `json:"sessionId,omitempty"`
	Method    string          `json:"method,omitempty"`
	Params    json.RawMessage `json:"params,omitempty"`
	Result    json.RawMessage `json:"result,omitempty"`
	Error     *struct {
		Code int `json:"code"`
	} `json:"error,omitempty"`
}

type frontendBrowserCDPPending struct {
	reply chan frontendBrowserCDPMessage
}

type frontendBrowserCDP struct {
	connection *websocket.Conn
	write      chan struct{}
	mu         sync.Mutex
	next       int
	pending    map[int]frontendBrowserCDPPending
	events     chan frontendBrowserCDPMessage
	closed     chan struct{}
	stop       func() bool
}

// The endpoint must originate in the new private test profile. Neither the
// browser list nor an existing user's debugging session is consulted.
func frontendBrowserCDPEndpoint(raw []byte) (string, error) {
	invalid := errors.New("invalid private fixture debugger endpoint")
	if len(raw) == 0 || len(raw) > 1024 {
		return "", invalid
	}
	parts := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(parts) != 2 {
		return "", invalid
	}
	port, err := strconv.Atoi(parts[0])
	if err != nil || port < 1 || port > 65535 || strconv.Itoa(port) != parts[0] {
		return "", invalid
	}
	const prefix = "/devtools/browser/"
	if !strings.HasPrefix(parts[1], prefix) || len(parts[1]) == len(prefix) {
		return "", invalid
	}
	for _, c := range strings.TrimPrefix(parts[1], prefix) {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
			return "", invalid
		}
	}
	return "ws://127.0.0.1:" + parts[0] + parts[1], nil
}

func connectFrontendBrowserCDP(ctx context.Context, profile string) (*frontendBrowserCDP, error) {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	var address string
	for address == "" {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		path := filepath.Join(profile, "DevToolsActivePort")
		info, err := os.Lstat(path)
		if err == nil {
			if !info.Mode().IsRegular() || info.Size() > 1024 {
				return nil, errors.New("invalid private fixture debugger record")
			}
			file, err := os.Open(path)
			if err != nil {
				return nil, errors.New("cannot read private fixture debugger record")
			}
			raw, readErr := io.ReadAll(io.LimitReader(file, 1025))
			closeErr := file.Close()
			if readErr != nil || closeErr != nil {
				return nil, errors.New("cannot read private fixture debugger record")
			}
			// Visibility does not prove the browser has finished writing this
			// record. Re-read within the original context until it validates;
			// an empty, partial or malformed endpoint is never dialled.
			address, _ = frontendBrowserCDPEndpoint(raw)
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, errors.New("cannot inspect private fixture debugger record")
		}
		if address == "" {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-ticker.C:
			}
		}
	}
	// An explicit nil proxy prevents environment proxy settings from redirecting
	// this private loopback websocket. The URL cannot contain an alternate host.
	connection, _, err := (&websocket.Dialer{HandshakeTimeout: 3 * time.Second}).DialContext(ctx, address, nil)
	if err != nil {
		return nil, errors.New("cannot connect private fixture debugger")
	}
	connection.SetReadLimit(1024 * 1024)
	client := &frontendBrowserCDP{connection: connection, write: make(chan struct{}, 1), pending: make(map[int]frontendBrowserCDPPending), events: make(chan frontendBrowserCDPMessage, 16), closed: make(chan struct{})}
	client.stop = context.AfterFunc(ctx, func() { _ = connection.Close() })
	go client.read()
	return client, nil
}

func (client *frontendBrowserCDP) close() {
	client.stop()
	_ = client.connection.Close()
	<-client.closed
}

func (client *frontendBrowserCDP) read() {
	defer close(client.closed)
	defer client.connection.Close()
	for {
		var message frontendBrowserCDPMessage
		if client.connection.ReadJSON(&message) != nil {
			return
		}
		if message.ID != 0 {
			client.mu.Lock()
			pending, found := client.pending[message.ID]
			delete(client.pending, message.ID)
			client.mu.Unlock()
			if found {
				pending.reply <- message
			}
			continue
		}
		switch message.Method {
		case "Emulation.virtualTimeBudgetExpired", "Target.detachedFromTarget", "Target.targetDestroyed", "Inspector.detached":
			select {
			case client.events <- message:
			default:
				// Losing a clock or target event cannot become a successful run.
				return
			}
		}
	}
}

func (client *frontendBrowserCDP) call(ctx context.Context, session, method string, params any, result any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	client.mu.Lock()
	client.next++
	id := client.next
	reply := make(chan frontendBrowserCDPMessage, 1)
	client.pending[id] = frontendBrowserCDPPending{reply: reply}
	client.mu.Unlock()
	defer func() { client.mu.Lock(); delete(client.pending, id); client.mu.Unlock() }()
	select {
	case client.write <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	case <-client.closed:
		return errFrontendBrowserCDPClosed
	}
	deadline, _ := ctx.Deadline()
	_ = client.connection.SetWriteDeadline(deadline)
	err := client.connection.WriteJSON(struct {
		ID        int    `json:"id"`
		SessionID string `json:"sessionId,omitempty"`
		Method    string `json:"method"`
		Params    any    `json:"params"`
	}{id, session, method, params})
	<-client.write
	if err != nil {
		return errors.New("fixture debugger command write failed")
	}
	select {
	case message := <-reply:
		if message.SessionID != session {
			return errFrontendBrowserCDPIdentity
		}
		if message.Error != nil {
			// Upstream messages, URLs, arguments and exception stacks are private.
			return fmt.Errorf("fixture debugger command rejected with code %d", message.Error.Code)
		}
		if result != nil && json.Unmarshal(message.Result, result) != nil {
			return errors.New("fixture debugger returned an invalid result")
		}
		return nil
	case <-client.closed:
		return errFrontendBrowserCDPClosed
	case <-ctx.Done():
		return ctx.Err()
	}
}

type frontendBrowserVirtualClock struct {
	GrantedMS int
	ExpiredMS int
	lastGrant time.Time
	inFlight  bool
}

func (clock *frontendBrowserVirtualClock) delay(now time.Time) time.Duration {
	if clock.lastGrant.IsZero() {
		return 0
	}
	return max(0, clock.lastGrant.Add(frontendBrowserVirtualSliceMS*time.Millisecond).Sub(now))
}

func (clock *frontendBrowserVirtualClock) grant(now time.Time) error {
	if clock.inFlight || clock.delay(now) > 0 {
		return errors.New("fixture attempted to replace an active virtual time budget")
	}
	if clock.GrantedMS+frontendBrowserVirtualSliceMS > frontendBrowserVirtualBudgetMS {
		return errFrontendBrowserBudget
	}
	// Count dispatch, including a lost acknowledgement or a final in-flight
	// slice. Do not bank wall time spent waiting for network or renderer work.
	clock.GrantedMS += frontendBrowserVirtualSliceMS
	clock.lastGrant, clock.inFlight = now, true
	return nil
}

func (clock *frontendBrowserVirtualClock) expire() error {
	if !clock.inFlight {
		return errors.New("fixture received an unowned virtual time completion")
	}
	clock.ExpiredMS += frontendBrowserVirtualSliceMS
	clock.inFlight = false
	return nil
}

func advanceFrontendBrowserClock(ctx context.Context, client *frontendBrowserCDP, session, target string, terminal <-chan struct{}, clock *frontendBrowserVirtualClock) error {
	for {
		select {
		case <-terminal:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		case <-client.closed:
			return errFrontendBrowserCDPClosed
		default:
		}
		if clock.GrantedMS == frontendBrowserVirtualBudgetMS {
			return errFrontendBrowserBudget
		}
		if delay := clock.delay(time.Now()); delay > 0 {
			timer := time.NewTimer(delay)
			select {
			case <-timer.C:
			case <-terminal:
				timer.Stop()
				return nil
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-client.closed:
				timer.Stop()
				return errFrontendBrowserCDPClosed
			}
		}
		select {
		case <-terminal:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if err := clock.grant(time.Now()); err != nil {
			return err
		}
		if err := client.call(ctx, session, "Emulation.setVirtualTimePolicy", map[string]any{"policy": "pauseIfNetworkFetchesPending", "budget": frontendBrowserVirtualSliceMS, "maxVirtualTimeTaskStarvationCount": 1000}, nil); err != nil {
			return err
		}
		waiting := true
		for waiting {
			select {
			case <-terminal:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			case <-client.closed:
				return errFrontendBrowserCDPClosed
			case event := <-client.events:
				if event.Method == "Emulation.virtualTimeBudgetExpired" && event.SessionID == session {
					if err := clock.expire(); err != nil {
						return err
					}
					waiting = false
				}
				var detached struct {
					Session string `json:"sessionId"`
					Target  string `json:"targetId"`
				}
				if event.Method == "Inspector.detached" && event.SessionID == session || (event.Method == "Target.detachedFromTarget" || event.Method == "Target.targetDestroyed") && json.Unmarshal(event.Params, &detached) == nil && (detached.Session == session || detached.Target == target) {
					return errFrontendBrowserCDPIdentity
				}
			}
		}
	}
}

type frontendBrowserPacedOptions struct {
	Executable string
	Profile    string
	URL        string
	Terminal   <-chan struct{}
	Stdout     io.Writer
	Stderr     io.Writer
}

type frontendBrowserPacedResult struct {
	DOM       []byte
	GrantedMS int
	ExpiredMS int
	WallMS    int64
}

func validateFrontendBrowserPacedOptions(options frontendBrowserPacedOptions) error {
	u, err := url.Parse(options.URL)
	port := 0
	if err == nil {
		port, _ = strconv.Atoi(u.Port())
	}
	if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || port < 1 || port > 65535 || u.User != nil || u.Fragment != "" || u.Path == "" {
		return errors.New("fixture navigation must use an explicit loopback URL")
	}
	if !filepath.IsAbs(options.Executable) || !filepath.IsAbs(options.Profile) || options.Terminal == nil {
		return errors.New("fixture requires an explicit executable, fresh profile and terminal signal")
	}
	info, err := os.Lstat(options.Profile)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("fixture profile must be a fresh private directory")
	}
	entries, err := os.ReadDir(options.Profile)
	if err != nil || len(entries) != 0 {
		return errors.New("fixture profile must be empty before browser startup")
	}
	return nil
}

func runFrontendBrowserPaced(ctx context.Context, options frontendBrowserPacedOptions) (result frontendBrowserPacedResult, runErr error) {
	if err := validateFrontendBrowserPacedOptions(options); err != nil {
		return result, err
	}
	started := time.Now()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(ctx, options.Executable, "--headless", "--no-first-run", "--no-default-browser-check", "--disable-sync", "--disable-extensions", "--disable-background-networking", "--disable-component-update", "--password-store=basic", "--use-mock-keychain", "--proxy-server=http://127.0.0.1:9", "--proxy-bypass-list=127.0.0.1", "--host-resolver-rules=MAP * ~NOTFOUND, EXCLUDE 127.0.0.1, EXCLUDE localhost", "--user-data-dir="+options.Profile, "--window-size=1600,1000", "--remote-debugging-address=127.0.0.1", "--remote-debugging-port=0", "about:blank")
	cmd.Stdout, cmd.Stderr = options.Stdout, options.Stderr
	done := make(chan struct{})
	var processErr error
	go func() {
		processErr = runFrontendBrowserCommand(cmd)
		close(done)
		cancel()
	}()
	defer func() {
		cancel()
		<-done // The shared process-tree runner owns cancellation and pipe draining.
		result.WallMS = time.Since(started).Milliseconds()
		if processErr != nil {
			if errors.Is(processErr, errFrontendBrowserCleanup) {
				runErr = errors.Join(runErr, errFrontendBrowserCleanup)
			} else {
				runErr = errors.Join(runErr, errors.New("isolated fixture browser process failed"))
			}
		}
	}()
	client, err := connectFrontendBrowserCDP(ctx, options.Profile)
	if err != nil {
		return result, err
	}
	defer client.close()
	// A newly created target has a specific identity. Endpoint availability is
	// not page readiness, and the first page in Target.getTargets is not trusted.
	var created struct {
		Target string `json:"targetId"`
	}
	if err := client.call(ctx, "", "Target.createTarget", map[string]any{"url": "about:blank"}, &created); err != nil || created.Target == "" {
		return result, errors.Join(err, errors.New("cannot create owned fixture page"))
	}
	var attached struct {
		Session string `json:"sessionId"`
	}
	if err := client.call(ctx, "", "Target.attachToTarget", map[string]any{"targetId": created.Target, "flatten": true}, &attached); err != nil || attached.Session == "" {
		return result, errors.Join(err, errors.New("cannot attach owned fixture page"))
	}
	session := attached.Session
	if err := client.call(ctx, session, "Page.enable", struct{}{}, nil); err != nil {
		return result, err
	}
	if err := client.call(ctx, session, "Emulation.setVirtualTimePolicy", map[string]any{"policy": "pause"}, nil); err != nil {
		return result, err
	}
	var navigation struct {
		Frame string `json:"frameId"`
		Error string `json:"errorText"`
	}
	if err := client.call(ctx, session, "Page.navigate", map[string]any{"url": options.URL}, &navigation); err != nil || navigation.Frame == "" || navigation.Error != "" {
		return result, errors.Join(err, errors.New("owned fixture page navigation failed"))
	}
	var clock frontendBrowserVirtualClock
	runErr = advanceFrontendBrowserClock(ctx, client, session, created.Target, options.Terminal, &clock)
	result.GrantedMS, result.ExpiredMS = clock.GrantedMS, clock.ExpiredMS
	if ctx.Err() == nil {
		pauseErr := client.call(ctx, session, "Emulation.setVirtualTimePolicy", map[string]any{"policy": "pause"}, nil)
		var snapshotErr error
		result.DOM, snapshotErr = frontendBrowserDOMSnapshot(ctx, client, session, navigation.Frame, options.URL)
		runErr = errors.Join(runErr, pauseErr, snapshotErr)
		// Closing the browser may close its websocket before acknowledging the
		// command. Only a confirmed clean process exit can accept that case.
		_ = client.call(ctx, "", "Browser.close", struct{}{}, nil)
	}
	select {
	case <-done:
		return result, runErr
	case <-ctx.Done():
		select {
		case <-done:
			return result, runErr
		default:
			return result, errors.Join(runErr, ctx.Err())
		}
	}
}

func frontendBrowserDOMSnapshot(ctx context.Context, client *frontendBrowserCDP, session, frame, fixtureURL string) ([]byte, error) {
	var tree struct {
		Tree struct {
			Frame struct {
				ID     string `json:"id"`
				Parent string `json:"parentId"`
				URL    string `json:"url"`
			} `json:"frame"`
		} `json:"frameTree"`
	}
	if err := client.call(ctx, session, "Page.getFrameTree", struct{}{}, &tree); err != nil {
		return nil, err
	}
	if tree.Tree.Frame.ID != frame || tree.Tree.Frame.Parent != "" || tree.Tree.Frame.URL != fixtureURL {
		return nil, errFrontendBrowserCDPIdentity
	}
	var result struct {
		Result struct {
			Type  string `json:"type"`
			Value struct {
				URL string `json:"url"`
				DOM string `json:"dom"`
			} `json:"value"`
		} `json:"result"`
		Exception json.RawMessage `json:"exceptionDetails"`
	}
	// This only reads the top fixture document. Opaque preview HTML and its
	// execution context are never evaluated or promoted to trusted content.
	if err := client.call(ctx, session, "Runtime.evaluate", map[string]any{"expression": "({url:location.href,dom:document.documentElement.outerHTML.slice(0,65536)})", "returnByValue": true}, &result); err != nil {
		return nil, err
	}
	if len(result.Exception) != 0 || result.Result.Type != "object" || result.Result.Value.URL != fixtureURL {
		return nil, errFrontendBrowserCDPIdentity
	}
	dom := []byte(result.Result.Value.DOM)
	return dom[:min(len(dom), 64*1024)], nil
}
