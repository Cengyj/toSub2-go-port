// Package browser drives an installed Chromium directly through CDP. It does not
// download or launch a Node driver, and each Session owns a fresh browser profile.
package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

var ErrClosed = errors.New("Chromium session closed")
var ErrCapacity = errors.New("browser concurrency limit reached")

type Identity struct {
	UserAgent      string
	BrowserVersion string
	Language       string
	Platform       string
	// Chromium always uses its own TLS stack; a Go ClientHello profile cannot be
	// installed into Chromium. Callers must not assert TLS-session equivalence.
	TLSProfile       string
	RequestedProfile string
}

type Session struct {
	bindingMu                                               sync.Mutex
	deviceScriptID                                          string
	bridge                                                  *proxyBridge
	Proxy, UserAgent, Profile, DeviceID, Language, Platform string
	ChromePath, Dir                                         string
	identity                                                Identity
	cmd                                                     *exec.Cmd
	tree                                                    *processTree
	cancel                                                  context.CancelFunc
	owner                                                   *Manager
	ws                                                      *websocket.Conn
	wsMu                                                    sync.Mutex
	pendingMu                                               sync.Mutex
	pending                                                 map[int64]chan cdpResponse
	readErr                                                 error
	seq                                                     atomic.Int64
	closed                                                  atomic.Bool
	closeOnce                                               sync.Once
	closeErr                                                error
	processDone                                             chan struct{}
	authMu                                                  sync.Mutex
	authAttempted                                           map[string]bool
	proxyUser, proxyPass                                    string
	pageEndpoint                                            string
	headless                                                bool
	targetMu                                                sync.Mutex
	targetSessions                                          map[string]struct{}
	eventMu                                                 sync.Mutex
	onPaused                                                func(json.RawMessage)
	extraMu                                                 sync.Mutex
	extraInfo                                               map[string]http.Header
	diagnosticMu                                            sync.Mutex
	diagnosticEntries                                       map[string]resourceDiagnostic
	diagnosticOrder                                         []string
	diagnosticDocument                                      resourceDiagnostic
	diagnosticDocumentRequest                               string
}

type BrowserSession interface {
	Navigate(context.Context, string) error
	Cookies() ([]map[string]any, error)
	SetCookies([]map[string]any) error
	Evaluate(context.Context, string) (any, error)
	EvaluateFrame(context.Context, string, string) (any, error)
	HTML(context.Context) (string, error)
	Close() error
}

type Manager struct {
	ChromePath      string
	Max             int
	mu              sync.Mutex
	active          map[*Session]struct{}
	starting        int
	closed          bool
	starts          sync.WaitGroup
	ctx             context.Context
	cancel          context.CancelFunc
	capacityChanged chan struct{}
	closeOnce       sync.Once
	closeErr        error
}

func NewManager(path string, max int) *Manager {
	if max <= 0 {
		max = 4
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Manager{ChromePath: path, Max: max, active: map[*Session]struct{}{}, ctx: ctx, cancel: cancel, capacityChanged: make(chan struct{})}
}

// StartWait keeps a protocol task pending while the shared Chromium pool is
// full. Waiting never holds a process slot and responds to both cancellation
// and manager shutdown.
func (m *Manager) StartWait(ctx context.Context, proxy, ua, profile, device string) (*Session, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		m.mu.Lock()
		changed := m.capacityChanged
		m.mu.Unlock()
		s, err := m.Start(ctx, proxy, ua, profile, device)
		if !errors.Is(err, ErrCapacity) {
			return s, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-m.ctx.Done():
			return nil, ErrClosed
		case <-changed:
		}
	}
}

func (m *Manager) signalCapacityLocked() {
	close(m.capacityChanged)
	m.capacityChanged = make(chan struct{})
}

func (m *Manager) Start(ctx context.Context, proxy, ua, profile, device string) (*Session, error) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, ErrClosed
	}
	if len(m.active)+m.starting >= m.Max {
		m.mu.Unlock()
		return nil, ErrCapacity
	}
	m.starting++
	m.starts.Add(1)
	m.mu.Unlock()
	defer func() { m.mu.Lock(); m.starting--; m.signalCapacityLocked(); m.mu.Unlock(); m.starts.Done() }()
	path := m.ChromePath
	if path == "" {
		path = os.Getenv("CHROME_PATH")
	}
	if path == "" {
		path = DefaultChrome()
	}
	if path == "" {
		return nil, errors.New("Chromium not found; set CHROME_PATH")
	}
	proxyArg, user, pass, err := parseProxy(proxy)
	if err != nil {
		return nil, err
	}
	dir, err := makeProfileDirectory()
	if err != nil {
		return nil, err
	}
	runCtx, cancel := context.WithCancel(ctx)
	stopManager := context.AfterFunc(m.ctx, cancel)
	var bridge *proxyBridge
	if parsed, e := url.Parse(proxy); e == nil && (parsed.Scheme == "socks5" || parsed.Scheme == "socks5h") {
		bridge, err = newProxyBridge(runCtx, proxy)
		if err != nil {
			stopManager()
			cancel()
			return nil, errors.Join(err, removeProfileDirectory(dir))
		}
		proxyArg, user, pass = bridge.endpoint, bridge.username, bridge.password
	}

	s := &Session{bridge: bridge, Proxy: proxy, Profile: profile, DeviceID: device, ChromePath: path, Dir: dir, cancel: cancel, owner: m, pending: map[int64]chan cdpResponse{}, authAttempted: map[string]bool{}, proxyUser: user, proxyPass: pass, processDone: make(chan struct{})}
	args := []string{"--no-first-run", "--no-default-browser-check", "--remote-debugging-address=127.0.0.1", "--remote-debugging-port=0", "--user-data-dir=" + dir, "--lang=zh-CN"}
	s.headless = os.Getenv("TOSUB2_BROWSER_HEADLESS") != "0"
	if s.headless {
		args = append(args, "--headless=new")
	}
	// Chromium's sandbox remains enabled by default. Root-only containers can opt
	// out explicitly; ordinary macOS/Windows/Linux users do not need this flag.
	if os.Getenv("TOSUB2_BROWSER_NO_SANDBOX") == "1" {
		args = append(args, "--no-sandbox")
	}
	if proxyArg != "" {
		args = append(args, "--proxy-server="+proxyArg, "--proxy-bypass-list=<-loopback>")
	}
	args = append(args, "about:blank")
	s.cmd = exec.CommandContext(runCtx, path, args...)
	s.tree, err = prepareProcessTree(s.cmd)
	if err != nil {
		stopManager()
		cancel()
		if bridge != nil {
			bridge.Close()
		}
		return nil, errors.Join(err, removeProfileDirectory(dir))
	}
	if err = s.cmd.Start(); err != nil {
		stopManager()
		cancel()
		if bridge != nil {
			bridge.Close()
		}
		return nil, errors.Join(fmt.Errorf("start Chromium: %w", err), s.tree.terminate(nil), removeProfileDirectory(dir))
	}
	if err = s.tree.attach(s.cmd.Process); err != nil {
		stopManager()
		cancel()
		killErr := s.tree.terminate(s.cmd.Process)
		_ = s.cmd.Wait()
		if bridge != nil {
			bridge.Close()
		}
		return nil, errors.Join(fmt.Errorf("isolate Chromium process: %w", err), killErr, removeProfileDirectory(dir))
	}
	go func() { _ = s.cmd.Wait(); close(s.processDone); stopManager(); _ = s.Close() }()
	if err = s.connect(runCtx); err != nil {
		return nil, errors.Join(err, s.Close())
	}
	if err = s.configure(runCtx, ua); err != nil {
		return nil, errors.Join(err, s.Close())
	}
	m.mu.Lock()
	if m.closed || s.closed.Load() {
		m.mu.Unlock()
		return nil, errors.Join(ErrClosed, s.Close())
	}
	m.active[s] = struct{}{}
	m.mu.Unlock()
	return s, nil
}

func parseProxy(raw string) (server, user, pass string, err error) {
	if strings.TrimSpace(raw) == "" {
		return "", "", "", nil
	}
	u, e := url.Parse(raw)
	if e != nil || u.Hostname() == "" {
		return "", "", "", errors.New("invalid Chromium proxy URL")
	}
	switch u.Scheme {
	case "http", "https", "socks5", "socks5h":
	default:
		return "", "", "", errors.New("unsupported Chromium proxy scheme")
	}
	if u.User != nil {
		user = u.User.Username()
		pass, _ = u.User.Password()
	}
	if u.Scheme == "socks5h" {
		u.Scheme = "socks5"
	} // Chromium resolves SOCKS5 names at the proxy.
	u.User = nil
	u.Path = ""
	u.RawQuery = ""
	u.Fragment = ""
	return u.String(), user, pass, nil
}

func (s *Session) connect(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	// Chrome chooses its own debug port, eliminating the bind-close-start race.
	var address string
	for address == "" {
		b, e := os.ReadFile(filepath.Join(s.Dir, "DevToolsActivePort"))
		if e == nil {
			lines := strings.Split(string(b), "\n")
			if len(lines) > 0 && strings.TrimSpace(lines[0]) != "" {
				address = "http://127.0.0.1:" + strings.TrimSpace(lines[0]) + "/json/list"
			}
		}
		if address != "" {
			break
		}
		select {
		case <-ctx.Done():
			return errors.New("Chromium DevTools endpoint not ready")
		case <-s.processDone:
			return errors.New("Chromium exited before DevTools was ready")
		case <-time.After(40 * time.Millisecond):
		}
	}
	client := &http.Client{Timeout: time.Second, Transport: &http.Transport{Proxy: nil}}
	defer client.CloseIdleConnections()
	var wsURL string
	for wsURL == "" {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
		resp, e := client.Do(req)
		if e == nil {
			var pages []struct {
				Type string `json:"type"`
				URL  string `json:"webSocketDebuggerUrl"`
			}
			_ = json.NewDecoder(resp.Body).Decode(&pages)
			_ = resp.Body.Close()
			for _, p := range pages {
				if p.Type == "page" {
					wsURL = p.URL
					break
				}
			}
		}
		if wsURL != "" {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(40 * time.Millisecond):
		}
	}
	dialer := websocket.Dialer{HandshakeTimeout: 5 * time.Second, Proxy: nil}
	c, _, err := dialer.DialContext(ctx, wsURL, nil)
	if err != nil {
		return errors.New("connect Chromium DevTools failed")
	}
	s.wsMu.Lock()
	if s.closed.Load() {
		s.wsMu.Unlock()
		_ = c.Close()
		return ErrClosed
	}
	s.ws = c
	s.pageEndpoint = wsURL
	s.wsMu.Unlock()
	go s.readLoop(c)
	return nil
}

func (s *Session) configure(ctx context.Context, requestedUA string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	for _, method := range []string{"Page.enable", "Runtime.enable", "Network.enable"} {
		if _, e := s.call(ctx, method, nil); e != nil {
			return e
		}
	}
	// A cross-site iframe has its own renderer/CDP target. Attach recursively
	// rather than assuming the top page can create that frame's execution world.
	if _, e := s.call(ctx, "Target.setAutoAttach", map[string]any{"autoAttach": true, "waitForDebuggerOnStart": false, "flatten": true}); e != nil {
		return e
	}
	raw, e := s.call(ctx, "Browser.getVersion", nil)
	if e != nil {
		return e
	}
	var v struct {
		UserAgent string `json:"userAgent"`
		Product   string `json:"product"`
	}
	if e = json.Unmarshal(raw, &v); e != nil {
		return e
	}
	// Preserve Chromium's actual UA/platform/client hints. A caller requesting a
	// different browser fingerprint must fail explicitly instead of silently
	// transferring a challenge token between mismatched identities.
	if requestedUA != "" && requestedUA != v.UserAgent {
		return errors.New("BROWSER_IDENTITY_MISMATCH: requested User-Agent differs from installed Chromium; start with an empty User-Agent and use Session.Identity for browser-bound requests")
	}
	s.UserAgent = v.UserAgent
	s.Language = "zh-CN"
	if p, e := s.Evaluate(ctx, "({platform:navigator.platform,language:navigator.language})"); e == nil {
		if identity, ok := p.(map[string]any); ok {
			s.Platform, _ = identity["platform"].(string)
			s.Language, _ = identity["language"].(string)
		}
	}
	s.identity = Identity{UserAgent: s.UserAgent, BrowserVersion: v.Product, Language: s.Language, Platform: s.Platform, TLSProfile: "chromium-native", RequestedProfile: s.Profile}
	if s.proxyUser != "" {
		if _, e = s.call(ctx, "Fetch.enable", map[string]any{"handleAuthRequests": true}); e != nil {
			return e
		}
	}
	if s.DeviceID != "" {
		if err := s.UpdateDeviceID(ctx, s.DeviceID); err != nil {
			return err
		}
	}
	return nil
}
func (s *Session) Identity() Identity { return s.identity }

type cdpMessage struct {
	ID        int64           `json:"id,omitempty"`
	Method    string          `json:"method,omitempty"`
	Params    json.RawMessage `json:"params,omitempty"`
	Result    json.RawMessage `json:"result,omitempty"`
	Error     *cdpError       `json:"error,omitempty"`
	SessionID string          `json:"sessionId,omitempty"`
}
type cdpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *cdpError) Error() string { return fmt.Sprintf("CDP command failed (%d)", e.Code) }

type cdpResponse struct {
	result json.RawMessage
	err    error
}

func jsString(v string) string { b, _ := json.Marshal(v); return string(b) }
func (s *Session) readLoop(c *websocket.Conn) {
	for {
		_, b, e := c.ReadMessage()
		if e != nil {
			s.failPending(ErrClosed)
			return
		}
		var m cdpMessage
		if json.Unmarshal(b, &m) != nil {
			continue
		}
		if m.ID == 0 {
			s.event(m)
			continue
		}
		s.pendingMu.Lock()
		ch := s.pending[m.ID]
		delete(s.pending, m.ID)
		s.pendingMu.Unlock()
		if ch != nil {
			if m.Error != nil {
				ch <- cdpResponse{err: m.Error}
			} else {
				ch <- cdpResponse{result: m.Result}
			}
		}
	}
}
func (s *Session) event(m cdpMessage) {
	s.recordResourceDiagnostic(m)
	switch m.Method {
	case "Target.attachedToTarget":
		var p struct {
			SessionID  string `json:"sessionId"`
			TargetInfo struct {
				Type string `json:"type"`
			} `json:"targetInfo"`
		}
		if json.Unmarshal(m.Params, &p) != nil || p.SessionID == "" || p.TargetInfo.Type != "iframe" {
			return
		}
		s.targetMu.Lock()
		if s.targetSessions == nil {
			s.targetSessions = map[string]struct{}{}
		}
		s.targetSessions[p.SessionID] = struct{}{}
		s.targetMu.Unlock()
		go s.configureFrameTarget(p.SessionID)
	case "Target.detachedFromTarget":
		var p struct {
			SessionID string `json:"sessionId"`
		}
		if json.Unmarshal(m.Params, &p) == nil {
			s.targetMu.Lock()
			delete(s.targetSessions, p.SessionID)
			s.targetMu.Unlock()
		}
	case "Network.responseReceivedExtraInfo":
		var p struct {
			ID      string         `json:"requestId"`
			Headers map[string]any `json:"headers"`
		}
		if json.Unmarshal(m.Params, &p) == nil {
			headers := http.Header{}
			for k, value := range p.Headers {
				if text, ok := value.(string); ok {
					for _, line := range strings.Split(text, "\n") {
						headers.Add(k, line)
					}
				}
			}
			s.extraMu.Lock()
			if s.extraInfo == nil {
				s.extraInfo = map[string]http.Header{}
			}
			s.extraInfo[p.ID] = headers
			s.extraMu.Unlock()
		}
	case "Fetch.requestPaused":
		s.eventMu.Lock()
		handler := s.onPaused
		s.eventMu.Unlock()
		if handler != nil {
			// Observers must record the response boundary before a deadline can
			// race their goroutine dispatch. The handler starts any CDP calls in
			// its own goroutine and must not block this reader.
			handler(m.Params)
			return
		}
		var p struct {
			ID string `json:"requestId"`
		}
		if json.Unmarshal(m.Params, &p) == nil && p.ID != "" {
			go s.notifyTarget(m.SessionID, "Fetch.continueRequest", map[string]any{"requestId": p.ID})
		}
	case "Fetch.authRequired":
		var p struct {
			ID        string `json:"requestId"`
			Challenge struct {
				Source string `json:"source"`
			} `json:"authChallenge"`
		}
		if json.Unmarshal(m.Params, &p) != nil || p.ID == "" {
			return
		}
		response := map[string]any{"response": "Default"}
		// Never leak proxy credentials to an origin's HTTP authentication challenge.
		if p.Challenge.Source == "Proxy" && s.proxyUser != "" {
			s.authMu.Lock()
			key := m.SessionID + "/" + p.ID
			tried := s.authAttempted[key]
			s.authAttempted[key] = true
			s.authMu.Unlock()
			if tried {
				response["response"] = "CancelAuth"
			} else {
				response = map[string]any{"response": "ProvideCredentials", "username": s.proxyUser, "password": s.proxyPass}
			}
		}
		go s.notifyTarget(m.SessionID, "Fetch.continueWithAuth", map[string]any{"requestId": p.ID, "authChallengeResponse": response})
	}
}
func (s *Session) notify(method string, params any) {
	s.notifyTarget("", method, params)
}
func (s *Session) notifyTarget(sessionID, method string, params any) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = s.callTarget(ctx, sessionID, method, params)
}
func (s *Session) failPending(err error) {
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()
	s.readErr = err
	for id, ch := range s.pending {
		delete(s.pending, id)
		ch <- cdpResponse{err: err}
	}
}
func (s *Session) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	return s.callTarget(ctx, "", method, params)
}
func (s *Session) callTarget(ctx context.Context, sessionID, method string, params any) (json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.closed.Load() {
		return nil, ErrClosed
	}
	id := s.seq.Add(1)
	message := map[string]any{"id": id, "method": method, "params": params}
	if sessionID != "" {
		message["sessionId"] = sessionID
	}
	b, e := json.Marshal(message)
	if e != nil {
		return nil, e
	}
	ch := make(chan cdpResponse, 1)
	s.pendingMu.Lock()
	if s.readErr != nil {
		e = s.readErr
		s.pendingMu.Unlock()
		return nil, e
	}
	s.pending[id] = ch
	s.pendingMu.Unlock()
	defer func() { s.pendingMu.Lock(); delete(s.pending, id); s.pendingMu.Unlock() }()
	s.wsMu.Lock()
	if s.ws == nil {
		e = ErrClosed
	} else {
		deadline := time.Now().Add(10 * time.Second)
		if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
			deadline = d
		}
		_ = s.ws.SetWriteDeadline(deadline)
		e = s.ws.WriteMessage(websocket.TextMessage, b)
	}
	s.wsMu.Unlock()
	if e != nil {
		return nil, ErrClosed
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case r := <-ch:
		if r.err != nil {
			return nil, fmt.Errorf("%s: %w", method, r.err)
		}
		return r.result, nil
	}
}

func (s *Session) Navigate(ctx context.Context, target string) error {
	u, err := url.Parse(target)
	if err != nil || u.Scheme == "" {
		return errors.New("invalid navigation URL")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	raw, err := s.call(ctx, "Page.navigate", map[string]any{"url": target})
	if err != nil {
		return err
	}
	var result struct {
		ErrorText string `json:"errorText"`
		LoaderID  string `json:"loaderId"`
	}
	if err = json.Unmarshal(raw, &result); err != nil {
		return err
	}
	if result.ErrorText != "" {
		return errors.New("Chromium navigation failed")
	}
	// Navigation can acknowledge before committing the replacement document.
	// Do not treat the previous page's readyState as the new page being ready.
	if result.LoaderID != "" {
		for {
			tree, e := s.frameTree(ctx)
			if e == nil && tree.Frame.LoaderID == result.LoaderID {
				break
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(20 * time.Millisecond):
			}
		}
	}
	return s.waitDocumentReady(ctx)
}

// DiscardRuntime aborts outstanding page requests before unloading an abandoned
// SDK invocation. Context cancellation alone only stops the CDP caller waiting.
// The browser profile, cookie store, device and proxy remain bound to this session.
func (s *Session) DiscardRuntime(ctx context.Context) error {
	if _, err := s.call(ctx, "Page.stopLoading", nil); err != nil {
		return err
	}
	for _, target := range s.frameTargetIDs() {
		_, _ = s.callTarget(ctx, target, "Page.stopLoading", nil)
	}
	return s.Navigate(ctx, "about:blank")
}

// Reload discards the document's SDK/iframe runtime after a failed invocation.
// ignoreCache asks Chromium to revalidate the loader and versioned SDK using
// its own network stack, proxy and cookie policy.
func (s *Session) Reload(parent context.Context, ignoreCache bool) error {
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	marker := fmt.Sprint(s.seq.Add(1))
	if _, err := s.Evaluate(ctx, "document.__tosub2ReloadMarker="+jsString(marker)); err != nil {
		return err
	}
	if ignoreCache {
		// A loader injected after document load would otherwise reuse a cached
		// script even when Page.reload itself bypassed cache for the document.
		if _, err := s.call(ctx, "Network.clearBrowserCache", nil); err != nil {
			return err
		}
	}
	if _, err := s.call(ctx, "Page.reload", map[string]any{"ignoreCache": ignoreCache}); err != nil {
		return err
	}
	// Page.reload can acknowledge before navigation commits. A document marker
	// lets readiness polling distinguish the new document from the old one.
	for {
		v, err := s.Evaluate(ctx, "document.__tosub2ReloadMarker!=="+jsString(marker)+"&&document.readyState==='complete'")
		if err == nil && v == true {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(40 * time.Millisecond):
		}
	}
}

func (s *Session) waitDocumentReady(ctx context.Context) error {
	for {
		v, e := s.Evaluate(ctx, "document.readyState")
		if e == nil && v == "complete" {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(40 * time.Millisecond):
		}
	}
}
func (s *Session) Evaluate(ctx context.Context, expression string) (any, error) {
	return s.evaluate(ctx, 0, expression)
}

// InsertText sends native Chromium input to the focused element. It preserves
// the browser's input event behavior for authorized interactive flows and UI
// acceptance tests. The supplied text is never logged.
func (s *Session) InsertText(ctx context.Context, text string) error {
	_, err := s.call(ctx, "Input.insertText", map[string]any{"text": text})
	return err
}
func (s *Session) evaluate(ctx context.Context, id int64, expression string) (any, error) {
	return s.evaluateTarget(ctx, "", id, expression)
}
func (s *Session) evaluateTarget(ctx context.Context, sessionID string, id int64, expression string) (any, error) {
	params := map[string]any{"expression": expression, "returnByValue": true, "awaitPromise": true}
	if id != 0 {
		params["contextId"] = id
	}
	raw, e := s.callTarget(ctx, sessionID, "Runtime.evaluate", params)
	if e != nil {
		return nil, e
	}
	var out struct {
		Result struct {
			Value any `json:"value"`
		} `json:"result"`
		Exception json.RawMessage `json:"exceptionDetails"`
	}
	if e = json.Unmarshal(raw, &out); e != nil {
		return nil, e
	}
	if len(out.Exception) > 0 && string(out.Exception) != "null" {
		return nil, errors.New("Chromium JavaScript execution failed")
	}
	return out.Result.Value, nil
}

type frameTreeNode struct {
	Frame struct {
		ID       string `json:"id"`
		URL      string `json:"url"`
		LoaderID string `json:"loaderId"`
	} `json:"frame"`
	ChildFrames []frameTreeNode `json:"childFrames"`
}

func (s *Session) frameTree(ctx context.Context) (frameTreeNode, error) {
	return s.frameTreeTarget(ctx, "")
}
func (s *Session) frameTreeTarget(ctx context.Context, sessionID string) (frameTreeNode, error) {
	raw, e := s.callTarget(ctx, sessionID, "Page.getFrameTree", nil)
	if e != nil {
		return frameTreeNode{}, e
	}
	var t struct {
		Tree frameTreeNode `json:"frameTree"`
	}
	e = json.Unmarshal(raw, &t)
	return t.Tree, e
}
func findFrameID(n frameTreeNode, want string) string {
	if n.Frame.URL == want {
		return n.Frame.ID
	}
	for _, child := range n.ChildFrames {
		if id := findFrameID(child, want); id != "" {
			return id
		}
	}
	return ""
}
func (s *Session) FrameURLs(ctx context.Context) ([]string, error) {
	tree, e := s.frameTree(ctx)
	if e != nil {
		return nil, e
	}
	var urls []string
	seen := map[string]bool{}
	var walk func(frameTreeNode)
	walk = func(n frameTreeNode) {
		if n.Frame.URL != "" && !seen[n.Frame.URL] {
			seen[n.Frame.URL] = true
			urls = append(urls, n.Frame.URL)
		}
		for _, child := range n.ChildFrames {
			walk(child)
		}
	}
	walk(tree)
	for _, sessionID := range s.frameTargetIDs() {
		if tree, err := s.frameTreeTarget(ctx, sessionID); err == nil {
			walk(tree)
		}
	}
	return urls, nil
}

// EvaluateFrame reads an exact frame URL in an isolated world. DOM is shared,
// but page script globals deliberately are not; use normal postMessage/provider
// callbacks to obtain challenge results generated by an iframe SDK.
func (s *Session) EvaluateFrame(ctx context.Context, frameURL, expression string) (any, error) {
	// Prefer the owning iframe target. A parent frame tree may mention an OOPIF
	// even though its execution contexts only exist in the child renderer.
	ids := append(s.frameTargetIDs(), "")
	for _, sessionID := range ids {
		tree, err := s.frameTreeTarget(ctx, sessionID)
		if err != nil {
			continue
		}
		id := findFrameID(tree, frameURL)
		if id == "" {
			continue
		}
		raw, err := s.callTarget(ctx, sessionID, "Page.createIsolatedWorld", map[string]any{"frameId": id, "worldName": "tosub2-dom"})
		if err != nil {
			continue
		}
		var created struct {
			ID int64 `json:"executionContextId"`
		}
		if err = json.Unmarshal(raw, &created); err != nil {
			return nil, err
		}
		return s.evaluateTarget(ctx, sessionID, created.ID, expression)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return nil, errors.New("browser frame not found or detached")
}
func (s *Session) HTML(ctx context.Context) (string, error) {
	v, e := s.Evaluate(ctx, "document.documentElement ? document.documentElement.outerHTML : ''")
	if e != nil {
		return "", e
	}
	html, _ := v.(string)
	return html, nil
}
func (s *Session) Cookies() ([]map[string]any, error) {
	return s.CookiesContext(context.Background())
}
func (s *Session) CookiesContext(parent context.Context) ([]map[string]any, error) {
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	raw, e := s.call(ctx, "Network.getAllCookies", nil)
	if e != nil {
		return nil, e
	}
	var r struct {
		Cookies []map[string]any `json:"cookies"`
	}
	e = json.Unmarshal(raw, &r)
	return r.Cookies, e
}
func (s *Session) SetCookies(cookies []map[string]any) error {
	return s.SetCookiesContext(context.Background(), cookies)
}
func (s *Session) SetCookiesContext(parent context.Context, cookies []map[string]any) error {
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	// Read-only Cookie fields returned by Network.getAllCookies are not accepted
	// by CookieParam. Explicitly copy supported fields to permit safe roundtrips.
	clean := make([]map[string]any, 0, len(cookies))
	for _, cookie := range cookies {
		c := map[string]any{}
		for _, key := range []string{"name", "value", "url", "domain", "path", "secure", "httpOnly", "sameSite", "expires", "priority", "sameParty", "sourceScheme", "sourcePort", "partitionKey"} {
			if value, ok := cookie[key]; ok {
				c[key] = value
			}
		}
		clean = append(clean, c)
	}
	_, e := s.call(ctx, "Network.setCookies", map[string]any{"cookies": clean})
	return e
}
func (s *Session) Close() error {
	s.closeOnce.Do(func() {
		processRunning := false
		if s.processDone != nil {
			select {
			case <-s.processDone:
			default:
				processRunning = true
			}
		}
		s.wsMu.Lock()
		hasConnection := s.ws != nil
		s.wsMu.Unlock()
		if processRunning && hasConnection && !s.closed.Load() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			_, _ = s.call(ctx, "Browser.close", nil)
			cancel()
			select {
			case <-s.processDone:
			case <-time.After(time.Second):
			}
		}
		s.closed.Store(true)
		s.failPending(ErrClosed)
		s.wsMu.Lock()
		if s.ws != nil {
			_ = s.ws.Close()
		}
		s.wsMu.Unlock()
		if s.cancel != nil {
			s.cancel()
		}
		treeStopped := true
		if s.tree != nil {
			treeErr := s.tree.terminate(s.cmd.Process)
			treeStopped = treeErr == nil
			s.closeErr = errors.Join(s.closeErr, treeErr)
		}
		if s.bridge != nil {
			s.bridge.Close()
		}
		processStopped := treeStopped
		if s.processDone != nil {
			select {
			case <-s.processDone:
			case <-time.After(5 * time.Second):
				processStopped = false
				s.closeErr = errors.Join(s.closeErr, errors.New("Chromium did not exit; private profile retained because it may still be in use"))
			}
		}
		if processStopped {
			s.closeErr = errors.Join(s.closeErr, removeProfileDirectory(s.Dir))
		}
		if s.owner != nil {
			s.owner.mu.Lock()
			s.owner.closeErr = errors.Join(s.owner.closeErr, s.closeErr)
			delete(s.owner.active, s)
			s.owner.signalCapacityLocked()
			s.owner.mu.Unlock()
		}
	})
	return s.closeErr
}
func (m *Manager) Stop(s *Session) error {
	if s == nil {
		return nil
	}
	return s.Close()
}
func (m *Manager) Close() error {
	m.closeOnce.Do(func() {
		m.mu.Lock()
		m.closed = true
		m.cancel()
		m.mu.Unlock()
		m.starts.Wait()
		m.mu.Lock()
		var sessions []*Session
		for s := range m.active {
			sessions = append(sessions, s)
		}
		m.mu.Unlock()
		for _, s := range sessions {
			_ = s.Close() // Session.Close records its persistent result on the owner.
		}
	})
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.closeErr
}
func (m *Manager) TempRoot() string { return filepath.Join(os.TempDir(), "tosub2-browser") }
func DefaultChrome() string {
	paths := []string{"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome", "/Applications/Chromium.app/Contents/MacOS/Chromium", "/usr/bin/google-chrome", "/usr/bin/chromium", "/usr/bin/chromium-browser"}
	if runtime.GOOS == "windows" {
		for _, base := range []string{os.Getenv("ProgramFiles"), os.Getenv("ProgramFiles(x86)"), os.Getenv("LOCALAPPDATA")} {
			if base != "" {
				paths = append(paths, filepath.Join(base, "Google", "Chrome", "Application", "chrome.exe"))
			}
		}
	}
	for _, p := range paths {
		if st, e := os.Stat(p); e == nil && !st.IsDir() {
			return p
		}
	}
	for _, name := range []string{"google-chrome", "chromium", "chromium-browser"} {
		if p, e := exec.LookPath(name); e == nil {
			return p
		}
	}
	return ""
}

var _ BrowserSession = (*Session)(nil)
