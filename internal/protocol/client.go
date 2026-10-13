package protocol

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"tosub2-go-port/internal/transport"
)

// Client is the protocol-facing wrapper around the manual redirect transport.
// It centralises browser-like headers and cookie persistence without making
// callers parse stdout or JavaScript output.
type Doer interface {
	Do(context.Context, transport.Request) (*transport.Response, error)
}

var ErrBrowserChallenge = errors.New("AUTH_CHALLENGE_REQUIRED: security check could not complete in the Go authentication runtime")

// jarSetter is implemented by transports that share the protocol cookie jar.
type jarSetter interface {
	SetJar(*transport.CookieJar)
}

type sessionResetter interface {
	ResetSession(context.Context) error
}

type Client struct {
	Transport         Doer
	Jar               *transport.CookieJar
	UserAgent         string
	AcceptLanguage    string
	Timeout           time.Duration
	Challenge         func(context.Context, string) error
	ConnectionRetries int
	RetryDelay        time.Duration
	OnRetry           func(transport.ReadRetryEvent)
}
type RequestOptions struct {
	// StopAt captures a registered callback before requesting its one-use code.
	StopAt func(string) bool
	// SingleAttempt forbids connection retries and challenge replay for account
	// security changes whose remote outcome may become uncertain.
	SingleAttempt bool
	// RetryReadOnly explicitly marks a query (including a POST query) as safe
	// to repeat. RetryBeforeSend permits retries only before HTTP dispatch.
	RetryReadOnly, RetryBeforeSend bool
	// DisableChallengeReplay allows safe reads to retry transport failures while
	// preserving the account-security boundary against challenge-driven replay.
	DisableChallengeReplay bool
	DisableDefaultHeaders  bool
	Headers                http.Header
	JSON                   any
	Form                   url.Values
	Body                   []byte
	Referer                string
	Origin                 string
	Timeout                time.Duration
}

func NewClient(tc Doer, jar *transport.CookieJar, profile string) *Client {
	if jar == nil {
		jar = transport.NewCookieJar()
	}
	if tc == nil {
		tc = transport.NewClient(jar)
	} else if setter, ok := tc.(jarSetter); ok {
		setter.SetJar(jar)
	}
	p := transport.ProfileFor(profile)
	return &Client{Transport: tc, Jar: jar, UserAgent: p.UserAgent, AcceptLanguage: p.AcceptLanguage, Timeout: 30 * time.Second, RetryDelay: 200 * time.Millisecond}
}

// ResetSession discards expired auth cookies while retaining the device and
// existing proxy, connection profile and browser context. Browser transports
// must also clear their authoritative cookie store before the next request.
func (c *Client) ResetSession(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if resetter, ok := c.Transport.(sessionResetter); ok {
		return resetter.ResetSession(ctx)
	}
	for _, cookie := range c.Jar.Cookies() {
		if cookie.Name != "oai-did" {
			cookie.Expires = 1
			c.Jar.Set(cookie)
		}
	}
	return nil
}

// Do lets OAuth helpers use the same session-aware request path as login.
// The underlying transport continues to expose redirects and raw Set-Cookie;
// POSTs are replayed only after an explicit challenge was handled successfully.
func (c *Client) Do(ctx context.Context, request transport.Request) (*transport.Response, error) {
	method := request.Method
	if method == "" {
		method = http.MethodGet
	}
	return c.Request(ctx, method, request.URL, RequestOptions{Headers: request.Headers, Body: request.Body, Timeout: request.Timeout, SingleAttempt: request.SingleAttempt, RetryReadOnly: request.RetryReadOnly, RetryBeforeSend: request.RetryBeforeSend})
}

func (c *Client) Request(ctx context.Context, method, rawURL string, opt RequestOptions) (*transport.Response, error) {
	if c == nil || c.Transport == nil {
		return nil, fmt.Errorf("nil protocol client")
	}
	h := make(http.Header)
	for k, v := range opt.Headers {
		for _, x := range v {
			h.Add(k, x)
		}
	}
	if !opt.DisableDefaultHeaders && h.Get("Accept") == "" {
		h.Set("Accept", "application/json, text/plain, */*")
	}
	if c.UserAgent != "" && h.Get("User-Agent") == "" {
		h.Set("User-Agent", c.UserAgent)
	}
	if c.AcceptLanguage != "" && h.Get("Accept-Language") == "" {
		h.Set("Accept-Language", c.AcceptLanguage)
	}
	if opt.Referer != "" {
		h.Set("Referer", opt.Referer)
	}
	if opt.Origin != "" {
		h.Set("Origin", opt.Origin)
	}
	u, parseErr := url.Parse(rawURL)
	accountWrite := method != http.MethodGet && parseErr == nil && (strings.EqualFold(u.Hostname(), "auth.openai.com") || strings.HasPrefix(u.Path, "/api/accounts/"))
	if !opt.DisableDefaultHeaders && accountWrite {
		// One invocation identifies one logical auth operation, including its
		// explicit browser challenge replay. Subsequent writes get fresh IDs.
		h.Set("X-Access-Flow-Invocation-Id", randomUUID())
		h.Set("Sec-Fetch-Site", "same-origin")
		h.Set("Sec-Fetch-Mode", "cors")
		h.Set("Sec-Fetch-Dest", "empty")
		h.Set("Priority", "u=1, i")
	} else if !opt.DisableDefaultHeaders && method != http.MethodGet && h.Get("X-Access-Flow-Invocation-Id") == "" {
		h.Set("X-Access-Flow-Invocation-Id", randomUUID())
	}
	body := opt.Body
	if opt.JSON != nil {
		b, e := json.Marshal(opt.JSON)
		if e != nil {
			return nil, e
		}
		body = b
		h.Set("Content-Type", "application/json")
		if !opt.DisableDefaultHeaders {
			h.Set("Accept", "application/json")
		}
	}
	if opt.Form != nil {
		body = []byte(opt.Form.Encode())
		h.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	timeout := opt.Timeout
	if timeout <= 0 {
		timeout = c.Timeout
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	oneTimeNavigation := (method == http.MethodGet || method == "") && isOneTimeAuthNavigation(u)
	beforeSend := opt.RetryBeforeSend || oneTimeNavigation || method != http.MethodGet && method != http.MethodHead && method != ""
	request := transport.Request{Method: method, URL: rawURL, Headers: h, Body: body, Timeout: timeout, SingleAttempt: opt.SingleAttempt, RetryReadOnly: opt.RetryReadOnly && !oneTimeNavigation, RetryBeforeSend: beforeSend}
	if opt.SingleAttempt {
		return c.Transport.Do(ctx, request)
	}
	response, err := c.requestWithRetry(ctx, request)
	if err == nil && !opt.DisableChallengeReplay && !oneTimeNavigation && c.Challenge != nil && response != nil && isBrowserChallenge(response) {
		if err = c.Challenge(ctx, rawURL); err != nil {
			return nil, err
		}
		request.Headers.Set("User-Agent", c.UserAgent)
		if c.AcceptLanguage != "" {
			request.Headers.Set("Accept-Language", c.AcceptLanguage)
		}
		response, err = c.requestWithRetry(ctx, request)
	}
	if err == nil && isBrowserChallenge(response) {
		return response, ErrBrowserChallenge
	}
	return response, err
}

// Some GET navigations consume a one-time authorization code or establish a
// session. Losing their response does not make them safe to replay.
func isOneTimeAuthNavigation(u *url.URL) bool {
	if u == nil {
		return false
	}
	if strings.HasPrefix(strings.ToLower(u.Path), "/api/auth/callback/") {
		return true
	}
	for _, key := range []string{"code", "token", "access_token", "id_token", "ticket", "otp"} {
		if u.Query().Get(key) != "" {
			return true
		}
	}
	return false
}

func (c *Client) Follow(ctx context.Context, rawURL string, opt RequestOptions) (*transport.FollowResult, error) {
	opt.Headers = opt.Headers.Clone()
	if opt.Headers == nil {
		opt.Headers = make(http.Header)
	}
	if opt.Headers.Get("Accept") == "" {
		opt.Headers.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8")
	}
	current := rawURL
	var history []*transport.Response
	for redirects := 0; redirects < 12; redirects++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if transport.IsLocalCallback(current) || opt.StopAt != nil && opt.StopAt(current) {
			return &transport.FollowResult{FinalURL: current, History: history}, nil
		}
		r, err := c.Request(ctx, http.MethodGet, current, opt)
		if err != nil {
			return nil, err
		}
		history = append(history, r)
		if r.StatusCode >= 400 {
			return nil, newHTTPError(r.StatusCode, r.Body)
		}
		if !transport.IsRedirect(r.StatusCode) || r.Location(current) == nil {
			return &transport.FollowResult{FinalURL: current, Response: r, History: history}, nil
		}
		next := r.Location(current)
		previous, _ := url.Parse(current)
		if !sameOrigin(previous, next) {
			// Explicit headers belong to the origin that accepted them. The
			// next request gets its own domain-scoped cookies from the jar.
			opt.Headers = opt.Headers.Clone()
			for key := range opt.Headers {
				switch strings.ToLower(key) {
				case "authorization", "proxy-authorization", "cookie", "openai-sentinel-token", "openai-sentinel-so-token", "oai-device-id", "oai-session-id", "x-access-flow-invocation-id", "origin", "referer":
					delete(opt.Headers, key)
				}
			}
			opt.Origin = ""
			opt.Referer = ""
			if previous != nil && !(previous.Scheme == "https" && next.Scheme != "https") {
				opt.Referer = previous.Scheme + "://" + previous.Host + "/"
			}
		}
		current = next.String()
		if transport.IsLocalCallback(current) || opt.StopAt != nil && opt.StopAt(current) {
			return &transport.FollowResult{FinalURL: current, Response: r, History: history}, nil
		}
	}
	return nil, fmt.Errorf("too many redirects")
}

func sameOrigin(a, b *url.URL) bool {
	if a == nil || b == nil || !strings.EqualFold(a.Scheme, b.Scheme) || !strings.EqualFold(a.Hostname(), b.Hostname()) {
		return false
	}
	port := func(u *url.URL) string {
		if p := u.Port(); p != "" {
			return p
		}
		if strings.EqualFold(u.Scheme, "https") {
			return "443"
		}
		if strings.EqualFold(u.Scheme, "http") {
			return "80"
		}
		return ""
	}
	return port(a) == port(b)
}
func (c *Client) JSON(ctx context.Context, method, rawURL string, body any, opt RequestOptions, out any) (*transport.Response, error) {
	opt.JSON = body
	r, e := c.Request(ctx, method, rawURL, opt)
	if e != nil {
		return nil, e
	}
	if r.StatusCode < 200 || r.StatusCode >= 300 {
		return r, newHTTPError(r.StatusCode, r.Body)
	}
	if out == nil {
		// getJson in the legacy protocol validates JSON even when its caller
		// ignores the value (for example, the login providers preflight).
		var ignored any
		out = &ignored
	}
	if e = json.Unmarshal(r.Body, out); e != nil {
		return r, fmt.Errorf("HTTP %d returned invalid JSON: %w", r.StatusCode, e)
	}
	return r, nil
}
func randomHex(n int) string {
	b := make([]byte, n)
	if _, e := rand.Read(b); e != nil {
		return ""
	}
	return hex.EncodeToString(b)
}

func isBrowserChallenge(r *transport.Response) bool {
	if r == nil {
		return false
	}
	for _, key := range []string{"Cf-Mitigated", "X-Cf-Mitigated"} {
		if strings.EqualFold(strings.TrimSpace(r.Header.Get(key)), "challenge") {
			return true
		}
	}
	// A business denial or rate limit is not an invitation to launch a browser
	// and replay the operation. JSON error messages can mention challenge URLs;
	// only an explicit mitigation header overrides their ordinary HTTP status.
	contentType := strings.ToLower(r.Header.Get("Content-Type"))
	if strings.Contains(contentType, "json") || json.Valid(r.Body) {
		return false
	}
	body := strings.ToLower(string(r.Body))
	htmlBody := strings.Contains(contentType, "text/html") || strings.Contains(body, "<!doctype html") || strings.Contains(body, "<html")
	// Generic Cloudflare assets (for example /cdn-cgi/styles) also occur on
	// access-denied pages. Require the actual challenge script/runtime markers,
	// or the challenge page title, instead of treating all HTML 403s as gates.
	marker := strings.Contains(body, "/cdn-cgi/challenge-platform/") || strings.Contains(body, "_cf_chl_opt") || strings.Contains(body, "cf-challenge") || strings.Contains(body, "cf-chl-") || (htmlBody && strings.Contains(body, "<title>just a moment"))
	switch r.StatusCode {
	case 403:
		return marker
	case 400, 409:
		return htmlBody && marker
	case 503:
		return marker
	}
	return false
}

func randomUUID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return ""
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// Only idempotent reads are retried on connection failure. A write might have
// reached the server before its connection failed; its checkpoint is retained
// for an explicit resume instead of replaying OTP/token exchange blindly.
func (c *Client) requestWithRetry(ctx context.Context, request transport.Request) (*transport.Response, error) {
	return transport.DoReadWithRetry(ctx, request, c.ConnectionRetries, c.RetryDelay, c.Transport.Do, c.OnRetry)
}
