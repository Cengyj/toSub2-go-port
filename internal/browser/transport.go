package browser

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/net/http/httpguts"
	"tosub2-go-port/internal/transport"
)

// Transport performs browser-bound HTTP requests with CDP interception. Response
// redirects are captured before Chromium follows them, preserving raw Location
// and every Set-Cookie. Each request uses a hidden, windowless target in the same
// browser profile; the main tab keeps the live Sentinel SDK and its iframe session.
// Browser and normal Go transports must not be used concurrently on the same jar.
type Transport struct {
	Session  *Session
	Jar      *transport.CookieJar
	mu       sync.Mutex
	imported bool
}

// Unsupported or silently ignored hidden targets must never turn each protocol
// request into a visible tab. The caller can update Chromium and retry explicitly.
var ErrHiddenTargetUnsupported = errors.New("BROWSER_HIDDEN_TARGET_UNSUPPORTED: update Chrome to a version supporting verified hidden request targets; visible request-tab fallback is disabled")

// ErrNetworkRequest is a sanitized Chromium network failure, not a rejected
// HTTP response. Only a fresh, pre-authentication connectivity probe may use it
// to select another proxy session.
var ErrNetworkRequest = errors.New("Chromium network request failed")

// ErrResponseStarted accompanies an incomplete response after Chromium has
// received HTTP headers. Its cookies/session may already be committed, so a
// caller must not treat it as a connection failure that permits rotation.
var ErrResponseStarted = errors.New("Chromium received HTTP response headers before the request completed")

func NewTransport(s *Session, jar *transport.CookieJar) *Transport {
	if jar == nil {
		jar = transport.NewCookieJar()
	}
	return &Transport{Session: s, Jar: jar}
}
func (t *Transport) SetJar(jar *transport.CookieJar) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if jar != nil && jar != t.Jar {
		t.Jar = jar
		t.imported = false
	}
}

// ResetSession drops expired authentication state in the same browser context.
// The device cookies, actual browser identity and proxy remain unchanged. The
// old SDK page is unloaded so it cannot repopulate stale authentication cookies.
func (t *Transport) ResetSession(ctx context.Context) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.Session == nil {
		return errors.New("Chromium session required")
	}
	if err := t.exportCookies(ctx); err != nil {
		return err
	}
	var deviceCookies []transport.Cookie
	for _, cookie := range t.Jar.Cookies() {
		if cookie.Name == "oai-did" {
			deviceCookies = append(deviceCookies, cookie)
		}
	}
	if err := t.Session.DiscardRuntime(ctx); err != nil {
		return err
	}
	if _, err := t.Session.call(ctx, "Network.clearBrowserCookies", nil); err != nil {
		return err
	}
	for _, cookie := range t.Jar.Cookies() {
		cookie.Expires = 1
		t.Jar.Set(cookie)
	}
	t.Jar.Merge(deviceCookies)
	t.imported = false
	return t.importCookies(ctx)
}
func (t *Transport) ImportCookies(ctx context.Context) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.importCookies(ctx)
}
func (t *Transport) importCookies(ctx context.Context) error {
	if t.imported {
		return nil
	}
	if t.Session == nil {
		return errors.New("Chromium session required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	var cookies []map[string]any
	device := t.Session.BoundDeviceID()
	for _, c := range t.Jar.Cookies() {
		if c.Expires > 0 && c.Expires <= time.Now().UnixMilli() {
			continue
		}
		if c.PartitionKeyOpaque || (c.Partitioned && c.PartitionKey == nil) {
			return errors.New("BROWSER_COOKIE_PARTITION_REQUIRED: checkpoint contains a cookie without a restorable top-level site; start a fresh browser login")
		}
		if c.Name == "oai-did" && device != "" && c.Value != device {
			return errors.New("BROWSER_DEVICE_MISMATCH: checkpoint cookie and browser device differ")
		}
		cookie := map[string]any{"name": c.Name, "value": c.Value, "path": c.Path, "secure": c.Secure, "httpOnly": c.HTTPOnly}
		if c.PartitionKey != nil {
			cookie["partitionKey"] = map[string]any{"topLevelSite": c.PartitionKey.TopLevelSite, "hasCrossSiteAncestor": c.PartitionKey.HasCrossSiteAncestor}
		}
		if c.SameSite != "" {
			cookie["sameSite"] = c.SameSite
		}
		if c.HostOnly {
			scheme := "http"
			if c.Secure {
				scheme = "https"
			}
			cookie["url"] = scheme + "://" + c.Domain + c.Path
		} else {
			cookie["domain"] = "." + strings.TrimPrefix(c.Domain, ".")
		}
		if c.Expires > 0 {
			cookie["expires"] = float64(c.Expires) / 1000
		}
		cookies = append(cookies, cookie)
	}
	if len(cookies) > 0 {
		if err := t.Session.SetCookiesContext(ctx, cookies); err != nil {
			return err
		}
	}
	t.imported = true
	return nil
}
func (t *Transport) ExportCookies(ctx context.Context) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.exportCookies(ctx)
}
func (t *Transport) exportCookies(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	cookies, err := t.Session.CookiesContext(ctx)
	if err != nil {
		return err
	}
	device := t.Session.BoundDeviceID()
	for _, cookie := range cookies {
		if cookie["name"] == "oai-did" && device != "" && cookie["value"] != device {
			return errors.New("BROWSER_DEVICE_MISMATCH: provider changed the bound browser device; a fresh login is required")
		}
	}
	// The browser is authoritative after handoff. Remove missing cookies as well
	// as merging current values so deleted auth cookies never reappear on resume.
	for _, old := range t.Jar.Cookies() {
		old.Expires = 1
		t.Jar.Set(old)
	}
	for _, value := range cookies {
		c := transport.Cookie{}
		c.Name, _ = value["name"].(string)
		c.Value, _ = value["value"].(string)
		c.Domain, _ = value["domain"].(string)
		c.Path, _ = value["path"].(string)
		c.Secure, _ = value["secure"].(bool)
		c.HTTPOnly, _ = value["httpOnly"].(bool)
		c.SameSite, _ = value["sameSite"].(string)
		c.PartitionKeyOpaque, _ = value["partitionKeyOpaque"].(bool)
		if partition, ok := value["partitionKey"].(map[string]any); ok {
			site, _ := partition["topLevelSite"].(string)
			cross, _ := partition["hasCrossSiteAncestor"].(bool)
			c.PartitionKey = &transport.CookiePartitionKey{TopLevelSite: site, HasCrossSiteAncestor: cross}
		}
		c.Partitioned = c.PartitionKey != nil || c.PartitionKeyOpaque
		c.HostOnly = !strings.HasPrefix(c.Domain, ".")
		if expires, ok := value["expires"].(float64); ok && expires > 0 {
			c.Expires = int64(expires * 1000)
		}
		t.Jar.Set(c)
	}
	return nil
}

type intercepted struct {
	response *transport.Response
	err      error
}
type pausedRequest struct {
	NetworkID string `json:"networkId"`
	ID        string `json:"requestId"`
	Request   struct {
		URL     string         `json:"url"`
		Headers map[string]any `json:"headers"`
	} `json:"request"`
	ResourceType  string        `json:"resourceType"`
	Status        int           `json:"responseStatusCode"`
	ResponseError string        `json:"responseErrorReason"`
	Headers       []headerEntry `json:"responseHeaders"`
}
type headerEntry struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

func (t *Transport) Do(ctx context.Context, req transport.Request) (response *transport.Response, requestErr error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.Session == nil {
		return nil, errors.New("Chromium session required")
	}
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	u, err := url.Parse(req.URL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, errors.New("invalid browser request URL")
	}
	if supplied := req.Headers.Get("User-Agent"); supplied != "" && supplied != t.Session.UserAgent {
		return nil, errors.New("BROWSER_IDENTITY_MISMATCH: request User-Agent differs from Chromium")
	}
	for key, values := range req.Headers {
		if !httpguts.ValidHeaderFieldName(key) {
			return nil, errors.New("invalid browser request header name")
		}
		for _, value := range values {
			if !httpguts.ValidHeaderFieldValue(value) {
				return nil, errors.New("invalid browser request header value")
			}
		}
	}
	// Use Chromium's own WHATWG serialization, including host case, default
	// ports, dot segments, escapes and omitted fragments. A partial Go URL
	// normalization can miss the paused request and let a redirect escape.
	u, err = canonicalRequestURL(ctx, t.Session, req.URL)
	if err != nil {
		return nil, err
	}
	req.URL = u.String()
	if err = t.importCookies(ctx); err != nil {
		return nil, err
	}
	page, id, err := t.Session.newPage(ctx)
	if err != nil {
		return nil, err
	}
	var responseStarted atomic.Bool
	defer func() {
		requestErr = errors.Join(requestErr, t.Session.closeRequestPage(page, id))
		if requestErr != nil && !t.Session.closed.Load() {
			// Chrome may finish committing accepted cookies as the interrupted
			// network request is stopped. Read its authoritative store after target
			// closure, using a bounded cleanup context independent of the expired
			// request. This keeps a resumable checkpoint from resurrecting cookies.
			cleanup, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if err := t.exportCookies(cleanup); err != nil {
				requestErr = errors.Join(requestErr, errors.New("synchronize Chromium cookies after interrupted request"), err)
			}
		}
		if requestErr != nil && responseStarted.Load() {
			requestErr = errors.Join(requestErr, ErrResponseStarted)
		}
	}()
	result := make(chan intercepted, 1)
	var once sync.Once
	finish := func(r *transport.Response, e error) { once.Do(func() { result <- intercepted{r, e} }) }
	page.eventMu.Lock()
	page.onPaused = func(raw json.RawMessage) {
		var p pausedRequest
		if json.Unmarshal(raw, &p) != nil {
			return
		}
		if p.ResourceType != "Document" || p.Request.URL != req.URL {
			go page.notify("Fetch.continueRequest", map[string]any{"requestId": p.ID})
			return
		}
		if p.Status > 0 {
			responseStarted.Store(true)
		}
		// Only the observation above runs on the websocket reader. Response
		// processing issues CDP requests and must not hold up their replies.
		go func() {
			if p.ResponseError != "" {
				finish(nil, ErrNetworkRequest)
				page.notify("Fetch.failRequest", map[string]any{"requestId": p.ID, "errorReason": "Aborted"})
				return
			}
			if p.Status == 0 {
				h := http.Header{}
				for k, v := range p.Request.Headers {
					if text, ok := v.(string); ok {
						h.Set(k, text)
					}
				}
				for k, values := range req.Headers {
					if strings.EqualFold(k, "Cookie") || strings.EqualFold(k, "Host") || strings.EqualFold(k, "Content-Length") {
						continue
					}
					// Browser TLS, navigator, client hints and language form one identity.
					// Protocol headers from the previous Go profile cannot override it.
					if strings.HasPrefix(strings.ToLower(k), "sec-ch-ua") || strings.EqualFold(k, "Accept-Language") {
						continue
					}
					h.Del(k)
					for _, v := range values {
						h.Add(k, v)
					}
				}
				h.Set("User-Agent", t.Session.UserAgent)
				headers := []headerEntry{}
				for k, values := range h {
					for _, v := range values {
						headers = append(headers, headerEntry{k, v})
					}
				}
				method := req.Method
				if method == "" {
					method = http.MethodGet
				}
				params := map[string]any{"requestId": p.ID, "method": method, "headers": headers}
				if req.Body != nil {
					params["postData"] = base64.StdEncoding.EncodeToString(req.Body)
				}
				if _, e := page.call(ctx, "Fetch.continueRequest", params); e != nil {
					abortCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
					_, _ = page.call(abortCtx, "Fetch.failRequest", map[string]any{"requestId": p.ID, "errorReason": "Aborted"})
					cancel()
					finish(nil, e)
				}
				return
			}
			headers := http.Header{}
			for _, h := range p.Headers {
				headers.Add(h.Name, h.Value)
			}
			// Fetch omits Set-Cookie on current Chromium versions. ExtraInfo
			// contains the unfiltered network headers, including repeated values.
			if p.NetworkID != "" {
				deadline := time.Now().Add(2 * time.Second)
				for {
					page.extraMu.Lock()
					extra, ok := page.extraInfo[p.NetworkID]
					page.extraMu.Unlock()
					if ok {
						for k, values := range extra {
							headers.Del(k)
							for _, value := range values {
								headers.Add(k, value)
							}
						}
						break
					}
					if time.Now().After(deadline) {
						finish(nil, errors.New("Chromium response headers unavailable"))
						return
					}
					select {
					case <-ctx.Done():
						finish(nil, ctx.Err())
						return
					case <-time.After(5 * time.Millisecond):
					}
				}
			}
			var body []byte
			// CDP explicitly disallows getResponseBody on redirect responses. Callers
			// receive status/Location/Set-Cookie and manually request the next location.
			if !transport.IsRedirect(p.Status) && p.Status != 204 && p.Status != 304 && req.Method != http.MethodHead {
				raw, e := page.call(ctx, "Fetch.getResponseBody", map[string]any{"requestId": p.ID})
				if e != nil {
					finish(nil, e)
					return
				}
				var b struct {
					Body    string `json:"body"`
					Encoded bool   `json:"base64Encoded"`
				}
				if e = json.Unmarshal(raw, &b); e != nil {
					finish(nil, e)
					return
				}
				body = []byte(b.Body)
				if b.Encoded {
					body, e = base64.StdEncoding.DecodeString(b.Body)
					if e != nil {
						finish(nil, e)
						return
					}
				}
			}
			// Complete with an inert page. This prevents redirects, downloads and API
			// response scripts from running, while Chrome processes the actual cookies.
			fulfilled := []headerEntry{{"Content-Type", "text/html; charset=utf-8"}}
			for _, value := range headers.Values("Set-Cookie") {
				fulfilled = append(fulfilled, headerEntry{"Set-Cookie", value})
			}
			_, e := page.call(ctx, "Fetch.fulfillRequest", map[string]any{"requestId": p.ID, "responseCode": 200, "responseHeaders": fulfilled, "body": base64.StdEncoding.EncodeToString([]byte("<!doctype html><title>Protocol response</title>"))})
			if e != nil {
				finish(nil, e)
				return
			}
			finish(&transport.Response{StatusCode: p.Status, Header: headers, Body: body, URL: u, RawSetCookie: headers.Values("Set-Cookie")}, nil)
		}()
	}
	page.eventMu.Unlock()
	patterns := []map[string]any{{"urlPattern": "*", "requestStage": "Request"}, {"urlPattern": "*", "requestStage": "Response"}}
	if _, err = page.call(ctx, "Fetch.enable", map[string]any{"patterns": patterns, "handleAuthRequests": page.proxyUser != ""}); err != nil {
		return nil, err
	}
	// Page.navigate does not finish until the paused response is fulfilled, so
	// dispatch it independently of reading the captured response.
	go func() {
		raw, e := page.call(ctx, "Page.navigate", map[string]any{"url": req.URL})
		if e != nil {
			finish(nil, e)
			return
		}
		var out struct {
			Error string `json:"errorText"`
		}
		if json.Unmarshal(raw, &out) == nil && out.Error != "" {
			finish(nil, ErrNetworkRequest)
		}
	}()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case r := <-result:
		if r.err != nil {
			return nil, r.err
		}
		if err = t.exportCookies(ctx); err != nil {
			return nil, err
		}
		r.response.Cookies = t.Jar.Cookies()
		return r.response, nil
	}
}

func canonicalRequestURL(ctx context.Context, session *Session, raw string) (*url.URL, error) {
	value, err := session.Evaluate(ctx, `(()=>{const u=new URL(`+jsString(raw)+`);u.hash='';return u.href})()`)
	if err != nil {
		return nil, err
	}
	text, ok := value.(string)
	if !ok {
		return nil, errors.New("Chromium could not normalize request URL")
	}
	u, err := url.Parse(text)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, errors.New("invalid browser request URL")
	}
	return u, nil
}

func (s *Session) newPage(ctx context.Context) (requestPage *Session, targetID string, requestErr error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	// Background alone still creates a visible tab-strip entry. A hidden target
	// has no window but keeps this exact browser profile, proxy and network stack.
	// The manually operated SDK/challenge page is never reused for protocol I/O.
	raw, e := s.call(ctx, "Target.createTarget", map[string]any{"url": "about:blank", "background": true, "hidden": true})
	if e != nil {
		var protocolError *cdpError
		if errors.As(e, &protocolError) {
			if protocolError.Code == -32601 || protocolError.Code == -32602 {
				return nil, "", errors.Join(ErrHiddenTargetUnsupported, e)
			}
			return nil, "", e
		}
		// Cancellation or a lost connection can race a successful createTarget:
		// the late response is discarded and its target ID is no longer known.
		// End this owned session so the unidentifiable request cannot survive.
		return nil, "", errors.Join(e, errors.New("Chromium request target creation was interrupted; owned browser session terminated"), s.Close())
	}
	var out struct {
		ID string `json:"targetId"`
	}
	if e = json.Unmarshal(raw, &out); e != nil || out.ID == "" {
		// A created but unidentified target cannot safely be left behind.
		return nil, "", errors.Join(errors.New("Chromium returned no request target identifier"), s.Close())
	}
	var page *Session
	ready := false
	defer func() {
		if !ready {
			requestErr = errors.Join(requestErr, s.closeRequestPage(page, out.ID))
		}
	}()
	if e = s.requireHiddenRequestTarget(ctx, out.ID); e != nil {
		return nil, "", e
	}
	endpoint := s.pageEndpoint
	at := strings.LastIndex(endpoint, "/")
	if at < 0 {
		return nil, "", errors.New("invalid DevTools endpoint")
	}
	endpoint = endpoint[:at+1] + out.ID
	dialer := websocket.Dialer{HandshakeTimeout: 5 * time.Second, Proxy: nil}
	c, _, e := dialer.DialContext(ctx, endpoint, nil)
	if e != nil {
		return nil, "", errors.New("connect browser request tab failed")
	}
	page = &Session{UserAgent: s.UserAgent, Language: s.Language, Platform: s.Platform, proxyUser: s.proxyUser, proxyPass: s.proxyPass, ws: c, pending: map[int64]chan cdpResponse{}, authAttempted: map[string]bool{}, pageEndpoint: endpoint}
	go page.readLoop(c)
	if _, e = page.call(ctx, "Network.enable", nil); e != nil {
		return nil, "", e
	}
	// A protocol request must retain the direct HTTP behavior of the Go
	// transport: no browser response cache and no navigation-to-app-shell
	// rewrite by a PWA service worker. These settings apply only to this
	// temporary request target. The live SDK/challenge page keeps the normal
	// browser cache, service workers, frames and event loop.
	for method, params := range map[string]any{
		"Network.setCacheDisabled":       map[string]any{"cacheDisabled": true},
		"Network.setBypassServiceWorker": map[string]any{"bypass": true},
	} {
		if _, e = page.call(ctx, method, params); e != nil {
			return nil, "", e
		}
	}
	if _, e = page.call(ctx, "Page.enable", nil); e != nil {
		return nil, "", e
	}
	ready = true
	return page, out.ID, nil
}

func (s *Session) requireHiddenRequestTarget(ctx context.Context, targetID string) error {
	_, err := s.call(ctx, "Browser.getWindowForTarget", map[string]any{"targetId": targetID})
	if err != nil {
		var protocolError *cdpError
		if errors.As(err, &protocolError) && protocolError.Code == -32000 {
			message := strings.ToLower(protocolError.Message)
			if message == "browser window not found" || strings.Contains(message, "no window for target") {
				return nil
			}
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if s.closed.Load() {
			return ErrClosed
		}
	}
	// Old CDP versions may ignore an unknown hidden parameter. Verify the
	// absence of a window before navigating or sending any sensitive request.
	return errors.Join(ErrHiddenTargetUnsupported, err)
}

func (s *Session) closeRequestPage(page *Session, targetID string) (cleanupErr error) {
	defer func() {
		if page != nil {
			cleanupErr = errors.Join(cleanupErr, page.Close())
		}
	}()
	if targetID == "" || s.closed.Load() {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	// Detaching CDP resumes paused requests automatically. Close the target
	// first, using a cleanup context independent of the canceled request.
	raw, err := s.call(ctx, "Target.closeTarget", map[string]any{"targetId": targetID})
	if err == nil {
		var result struct {
			Success bool `json:"success"`
		}
		if json.Unmarshal(raw, &result) == nil && result.Success {
			return nil
		}
		err = errors.New("Chromium did not confirm request target closure")
	}
	// A target can already be gone because Chromium or the caller closed it.
	// Do not confuse that with losing control of a still-live request target.
	if s.closed.Load() || s.requestTargetAbsent(targetID) {
		return nil
	}
	if page != nil {
		stopCtx, stop := context.WithTimeout(context.Background(), time.Second)
		_, _ = page.call(stopCtx, "Page.stopLoading", nil)
		stop()
	}
	// Stopping loading or detaching CDP does not close a tab. If closure cannot
	// be proved, terminate this task's owned session; never touch other Chrome
	// processes or report a successful request while leaving its target behind.
	return errors.Join(errors.New("Chromium request target cleanup failed; owned browser session terminated"), err, s.Close())
}

func (s *Session) requestTargetAbsent(targetID string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	raw, err := s.call(ctx, "Target.getTargets", nil)
	if err != nil {
		return false
	}
	var result struct {
		Targets []struct {
			ID string `json:"targetId"`
		} `json:"targetInfos"`
	}
	if json.Unmarshal(raw, &result) != nil || result.Targets == nil {
		return false
	}
	for _, target := range result.Targets {
		if target.ID == targetID {
			return false
		}
	}
	return true
}
