package transport

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Request describes one protocol request. Redirects are deliberately disabled;
// callers use Follow so every 30x Location and Set-Cookie is observable.
type Request struct {
	Method  string
	URL     string
	Headers http.Header
	Body    []byte
	Timeout time.Duration
}

type Response struct {
	StatusCode   int
	Header       http.Header
	Body         []byte
	URL          *url.URL
	Cookies      []Cookie
	RawSetCookie []string
}

func (r *Response) Status() int {
	if r == nil {
		return 0
	}
	return r.StatusCode
}
func (r *Response) Text() string {
	if r == nil {
		return ""
	}
	return string(r.Body)
}
func (r *Response) Location(base string) *url.URL {
	if r == nil {
		return nil
	}
	raw := r.Header.Get("Location")
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil
	}
	if !u.IsAbs() {
		b, _ := url.Parse(base)
		if b != nil {
			u = b.ResolveReference(u)
		}
	}
	return u
}

type Client struct {
	HTTP           *http.Client
	Jar            *CookieJar
	Timeout        time.Duration
	mu             sync.RWMutex
	nativeIdentity bool
}

// SetJar replaces the cookie jar shared with the protocol layer.
func (c *Client) SetJar(jar *CookieJar) {
	if jar != nil {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.Jar = jar
	}
}

const NativeProfile = "go-native"
const NativeUserAgent = "toSub2-Go/1.0"

// NewNativeClient is an explicit compatibility mode, never an automatic TLS
// profile fallback. Its UA identifies the Go program instead of claiming a
// Chromium ClientHello. Proxy parsing, SOCKS DNS behavior, context deadlines,
// raw cookies and manual redirects match the other protocol transports.
func NewNativeClient(proxyURL string, jar *CookieJar) (*Client, error) {
	// Do supplies a context deadline for every request. A second client-wide
	// timeout would silently truncate caller budgets longer than 30 seconds.
	httpClient, err := newHTTPClient(proxyURL, 0)
	if err != nil {
		return nil, err
	}
	c := NewClient(jar)
	c.HTTP, c.nativeIdentity = httpClient, true
	return c, nil
}
func (c *Client) Close() {
	if c != nil && c.HTTP != nil {
		c.HTTP.CloseIdleConnections()
	}
}
func (c *Client) Profile() string {
	if c != nil && c.nativeIdentity {
		return NativeProfile
	}
	return ""
}

func NewClient(jar *CookieJar) *Client {
	if jar == nil {
		jar = NewCookieJar()
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	preserveNativeDialScope(tr)
	return &Client{HTTP: &http.Client{Transport: tr, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}, Jar: jar, Timeout: 30 * time.Second}
}

func (c *Client) Do(ctx context.Context, req Request) (*Response, error) {
	if c == nil {
		return nil, errors.New("nil transport client")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = c.Timeout
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	scope := newRequestDialScope(ctx)
	scope.native = true
	defer scope.finish(true)
	ctx = context.WithValue(ctx, requestDialScopeKey{}, scope)
	ctx = httptrace.WithClientTrace(ctx, scope.nativeTrace())
	c.mu.RLock()
	jar := c.Jar
	c.mu.RUnlock()
	method := req.Method
	if method == "" {
		method = http.MethodGet
	}
	hreq, err := http.NewRequestWithContext(ctx, method, req.URL, bytes.NewReader(req.Body))
	if err != nil {
		return nil, errors.New("invalid HTTP request URL")
	}
	if hreq.URL.Scheme == "https" {
		scope.tlsStages = 1
	}
	for key, vals := range req.Headers {
		if c.nativeIdentity && strings.HasPrefix(strings.ToLower(key), "sec-ch-ua") {
			continue
		}
		for _, val := range vals {
			hreq.Header.Add(key, val)
		}
	}
	if c.nativeIdentity {
		hreq.Header.Set("User-Agent", NativeUserAgent)
	}
	if cookie := jar.HeaderFor(req.URL); cookie != "" && hreq.Header.Get("Cookie") == "" {
		hreq.Header.Set("Cookie", cookie)
	}
	httpClient := c.HTTP
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	resp, err := httpClient.Do(hreq)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		// net/http errors include the full request URL, which may contain an
		// OAuth code or mailbox API key. Callers receive a safe category instead.
		return nil, errors.New("HTTP transport request failed")
	}
	defer resp.Body.Close()
	// Headers are an accepted server response even when its body subsequently
	// stalls or is truncated. Preserve additions/deletions for a safe resume.
	jar.SetFromResponse(req.URL, resp.Header)
	body, err := io.ReadAll(resp.Body)
	// Cancellation and a peer's clean EOF can arrive together. ReadAll may
	// therefore succeed after the request ended; it is not a successful Do.
	if cause := ctx.Err(); cause != nil {
		return nil, cause
	}
	if err != nil {
		return nil, errors.New("HTTP response body read failed")
	}
	return &Response{StatusCode: resp.StatusCode, Header: resp.Header.Clone(), Body: body, URL: resp.Request.URL, Cookies: jar.Cookies(), RawSetCookie: GetSetCookie(resp.Header)}, nil
}

// Follow manually follows HTTP redirects, preserving method semantics (303
// becomes GET, while 307/308 retain method and body). It returns the final
// response and every intermediate response for callers that need checkpoint
// or cookie inspection.
type FollowResult struct {
	FinalURL string
	Response *Response
	History  []*Response
}

func (c *Client) Follow(ctx context.Context, req Request, max int) (*FollowResult, error) {
	if max <= 0 {
		max = 12
	}
	current := req.URL
	currentReq := req
	history := make([]*Response, 0, max)
	for i := 0; i <= max; i++ {
		currentReq.URL = current
		r, err := c.Do(ctx, currentReq)
		if err != nil {
			return nil, err
		}
		history = append(history, r)
		if !IsRedirect(r.StatusCode) {
			return &FollowResult{FinalURL: current, Response: r, History: history}, nil
		}
		loc := r.Header.Get("Location")
		if loc == "" {
			return &FollowResult{FinalURL: current, Response: r, History: history}, nil
		}
		u, e := url.Parse(loc)
		if e != nil {
			return nil, errors.New("invalid redirect location")
		}
		base, _ := url.Parse(current)
		if !u.IsAbs() {
			u = base.ResolveReference(u)
		}
		if u.User != nil || (u.Scheme != "http" && u.Scheme != "https") {
			return nil, errors.New("unsupported redirect destination")
		}
		if !sameOrigin(base, u) {
			currentReq.Headers = currentReq.Headers.Clone()
			for key := range currentReq.Headers {
				switch strings.ToLower(key) {
				case "authorization", "proxy-authorization", "cookie", "openai-sentinel-token", "openai-sentinel-so-token", "x-api-key":
					delete(currentReq.Headers, key)
				case "referer":
					if base.Scheme == "https" && u.Scheme == "http" {
						delete(currentReq.Headers, key)
					}
				}
			}
		}
		current = u.String()
		if IsLocalCallback(current) {
			return &FollowResult{FinalURL: current, Response: r, History: history}, nil
		}
		method := currentReq.Method
		if method == "" {
			method = http.MethodGet
		}
		if (r.StatusCode == 301 || r.StatusCode == 302 || r.StatusCode == 303) && method != http.MethodGet && method != http.MethodHead {
			currentReq.Method = http.MethodGet
			currentReq.Body = nil
			currentReq.Headers = cloneHeadersWithoutEntity(currentReq.Headers)
		}
	}
	return nil, fmt.Errorf("too many redirects (>%d)", max)
}
func cloneHeadersWithoutEntity(in http.Header) http.Header {
	out := in.Clone()
	for key := range out {
		if strings.EqualFold(key, "Content-Length") || strings.EqualFold(key, "Content-Type") {
			delete(out, key)
		}
	}
	return out
}

func IsRedirect(code int) bool {
	return code == 301 || code == 302 || code == 303 || code == 307 || code == 308
}
func IsLocalCallback(raw string) bool {
	u, e := url.Parse(raw)
	if e != nil || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	loopback := host == "localhost"
	if ip := net.ParseIP(host); ip != nil {
		loopback = ip.IsLoopback()
	}
	return loopback && (u.Port() == "1455" || u.Path == "/auth/callback" || strings.HasPrefix(u.Path, "/auth/callback/"))
}

func sameOrigin(a, b *url.URL) bool {
	if a == nil || b == nil {
		return false
	}
	port := func(u *url.URL) string {
		if u.Port() != "" {
			return u.Port()
		}
		if u.Scheme == "https" {
			return "443"
		}
		return "80"
	}
	return strings.EqualFold(a.Scheme, b.Scheme) && strings.EqualFold(a.Hostname(), b.Hostname()) && port(a) == port(b)
}
