// Package legacyruntime owns native Go HTTP/TLS for dynamically supplied
// authentication scripts executed by the same Go executable.
package legacyruntime

import (
	"bytes"
	"context"
	"crypto/x509"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	tlsclient "github.com/bogdanfinn/tls-client"

	"tosub2-go-port/internal/transport"
)

var (
	ErrClosed      = errors.New("LEGACY_RUNTIME_CLOSED: transport session is closed; start a new login")
	ErrProtocol    = errors.New("LEGACY_RUNTIME_PROTOCOL: invalid script worker response")
	ErrUnavailable = errors.New("LEGACY_RUNTIME_UNAVAILABLE: Go authentication script worker is unavailable")
)

type Options struct {
	ProxyURL, Profile, SDKURL string
	WorkerPath                string
	Timeout, SolverTimeout    time.Duration
	ConnectionRetries         int
	RetryDelay                time.Duration
	OnRetry                   func(transport.ReadRetryEvent)
}

type Client struct {
	opts       Options
	ctx        context.Context
	cancel     context.CancelFunc
	http       *transport.FingerprintClient
	gate       chan struct{}
	closeOnce  sync.Once
	mu         sync.RWMutex
	jar        *transport.CookieJar
	profile    string
	closed     bool
	cleanupErr error
	challenges map[string][]byte // serialized by the operation gate
}

// New selects a supported Chrome TLS identity without probing, replacing
// a bound profile or contacting the account service. Chrome146 uses the explicit
// curl-compatible variant. The Go script worker is started only when Sentinel
// or an authentication security challenge runs its scripts.
func New(owner context.Context, options Options) (*Client, error) {
	return newClient(owner, options, nil)
}

// NewWithRootCAs uses explicitly trusted private CAs while retaining hostname
// and certificate verification. New uses the operating system's trusted CAs.
func NewWithRootCAs(owner context.Context, options Options, roots *x509.CertPool) (*Client, error) {
	return newClient(owner, options, roots)
}

func newClient(owner context.Context, options Options, roots *x509.CertPool) (*Client, error) {
	if owner == nil {
		owner = context.Background()
	}
	if err := owner.Err(); err != nil {
		return nil, err
	}
	if options.ConnectionRetries < 0 || options.ConnectionRetries > 20 || options.RetryDelay < 0 {
		return nil, errors.New("LEGACY_RUNTIME_RETRY: invalid read retry configuration")
	}
	if options.Timeout <= 0 {
		options.Timeout = 30 * time.Second
	}
	if options.SolverTimeout <= 0 {
		options.SolverTimeout = 120 * time.Second
	}
	options.Profile = strings.ToLower(strings.TrimSpace(options.Profile))
	if options.Profile == "" || options.Profile == "auto" {
		options.Profile = "chrome146"
	}
	if !profilePattern.MatchString(options.Profile) {
		return nil, errors.New("LEGACY_RUNTIME_PROFILE: expected an explicitly supported Chrome profile such as chrome146")
	}
	jar := transport.NewCookieJar()
	var tlsOptions []tlsclient.HttpClientOption
	if roots != nil {
		tlsOptions = append(tlsOptions, tlsclient.WithTransportOptions(&tlsclient.TransportOptions{RootCAs: roots}))
	}
	httpClient, err := transport.NewCurlCompatibleFingerprintClientExact(options.Profile, options.ProxyURL, jar, tlsOptions...)
	if err != nil {
		return nil, err
	}
	options.Profile = httpClient.Profile()
	ctx, cancel := context.WithCancel(owner)
	return &Client{opts: options, ctx: ctx, cancel: cancel, http: httpClient, gate: make(chan struct{}, 1), jar: jar, profile: options.Profile, challenges: map[string][]byte{}}, nil
}

var profilePattern = regexp.MustCompile(`^chrome_?[0-9]+$`)

// Close cancels the owner and waits for the active HTTP/script operation's deferred
// cleanup before returning. Waiting callers cannot start another operation.
func (c *Client) Close() error {
	if c == nil {
		return nil
	}
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.closed = true
		c.cancel()
		c.mu.Unlock()
	})
	c.gate <- struct{}{}
	defer c.release()
	c.http.Close()
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.cleanupErr
}

func (c *Client) closedError() error {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.closed {
		return ErrClosed
	}
	if err := c.ctx.Err(); err != nil {
		return err
	}
	return ErrClosed
}

func (c *Client) acquire(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.ctx.Done():
		return c.closedError()
	case c.gate <- struct{}{}:
		if c.ctx.Err() != nil {
			c.release()
			return c.closedError()
		}
		if err := ctx.Err(); err != nil {
			c.release()
			return err
		}
		return nil
	}
}
func (c *Client) release() { <-c.gate }

// operationContext joins caller cancellation/deadlines with the transport owner
// so Close also stops a script worker or a request blocked in TLS/body I/O.
func (c *Client) operationContext(caller context.Context) (context.Context, context.CancelFunc) {
	if caller == nil {
		caller = context.Background()
	}
	ctx, cancel := context.WithCancel(caller)
	stop := context.AfterFunc(c.ctx, cancel)
	if c.ctx.Err() != nil {
		cancel()
	}
	return ctx, func() { stop(); cancel() }
}

func (c *Client) SetJar(jar *transport.CookieJar) {
	if jar == nil {
		return
	}
	if err := c.acquire(c.ctx); err != nil {
		return
	}
	defer c.release()
	c.mu.Lock()
	c.jar = jar
	c.http.SetJar(jar)
	c.mu.Unlock()
	c.challenges = map[string][]byte{}
}
func (c *Client) cookieJar() *transport.CookieJar { c.mu.RLock(); defer c.mu.RUnlock(); return c.jar }
func (c *Client) Profile() string                 { return c.profile }
func (c *Client) UserAgent() string               { return transport.ProfileFor(c.http.Profile()).UserAgent }
func (c *Client) AcceptLanguage() string {
	return transport.ProfileFor(c.http.Profile()).AcceptLanguage
}

func (c *Client) ResetSession(ctx context.Context) error {
	if err := c.acquire(ctx); err != nil {
		return err
	}
	defer c.release()
	ctx, cancel := c.operationContext(ctx)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return err
	}
	jar := c.cookieJar()
	for _, cookie := range jar.Cookies() {
		if cookie.Name != "oai-did" {
			cookie.Expires = 1
			jar.Set(cookie)
		}
	}
	c.challenges = map[string][]byte{}
	c.http.Close()
	return nil
}

func (c *Client) Do(ctx context.Context, request transport.Request) (*transport.Response, error) {
	if err := c.acquire(ctx); err != nil {
		return nil, err
	}
	defer c.release()
	return c.request(ctx, request)
}

// request assumes the caller owns the operation gate. The script resource bridge
// calls it directly so all script HTTP uses this same TLS identity and cookie
// jar without recursively taking the gate or creating a second HTTP session.
func (c *Client) request(ctx context.Context, request transport.Request) (*transport.Response, error) {
	ctx, cancel := c.operationContext(ctx)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	u, err := url.Parse(request.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		return nil, errors.New("LEGACY_RUNTIME_URL: invalid HTTP request URL")
	}
	headers := request.Headers.Clone()
	if headers == nil {
		headers = make(http.Header)
	}
	// Cookies always come from the shared authoritative jar; script-supplied
	// Cookie fields cannot resurrect deleted sessions or shadow restored state.
	headers.Del("Cookie")
	request.Headers = headers
	if request.Timeout <= 0 {
		request.Timeout = c.opts.Timeout
	}
	response, err := c.http.Do(ctx, request)
	if err != nil {
		return nil, err
	}
	// Only the challenge document is needed by the solver. Do not retain SDK
	// assets, mailbox/API responses or successful account/token payloads.
	delete(c.challenges, request.URL)
	contentType := strings.ToLower(response.Header.Get("Content-Type"))
	if bytes.Contains(response.Body, []byte("_cf_chl_opt")) &&
		(strings.Contains(contentType, "text/html") || strings.EqualFold(response.Header.Get("Cf-Mitigated"), "challenge") || strings.EqualFold(response.Header.Get("X-Cf-Mitigated"), "challenge")) {
		c.challenges[request.URL] = append([]byte(nil), response.Body...)
	}
	return response, nil
}

// runtimeRequest owns the script request retry budget. Mutations opt in only
// before dispatch; Do/request remain single attempts because protocol requests
// and connectivity probes own their budgets.
// The caller already owns the operation gate for this script operation.
func (c *Client) runtimeRequest(ctx context.Context, request transport.Request) (*transport.Response, error) {
	ctx, cancel := c.operationContext(ctx)
	defer cancel()
	return transport.DoReadWithRetry(ctx, request, c.opts.ConnectionRetries, c.opts.RetryDelay, c.request, c.opts.OnRetry)
}

// IsConnectionError is only for a fresh empty-session connectivity probe; HTTP
// status responses and script failures cannot authorize a proxy/profile change.
func IsConnectionError(err error) bool {
	return transport.IsConnectionError(err) || errors.Is(err, context.DeadlineExceeded)
}

func identity(profile string) map[string]any {
	actual := transport.ProfileFor(profile)
	osName, osVersion := "linux", "6.1"
	switch actual.Platform {
	case "macOS":
		osName, osVersion = "macos", "15.7.0"
	case "Windows":
		osName, osVersion = "windows", "10.0"
	}
	return map[string]any{"profile": profile, "browser": "chrome", "browserMajorVersion": actual.Major, "userAgent": actual.UserAgent, "secChUa": actual.ClientHints, "secChUaMobile": "?0", "secChUaPlatform": strconv.Quote(actual.Platform), "acceptLanguage": actual.AcceptLanguage, "locale": "zh-CN", "languages": []string{"zh-CN", "zh", "en"}, "timezoneId": "Asia/Shanghai", "os": osName, "osVersion": osVersion, "platform": actual.NavigatorPlatform}
}
