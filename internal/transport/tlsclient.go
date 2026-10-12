package transport

// FingerprintClient is the in-process TLS profile client used when a request
// must retain a Chrome-like ClientHello. It intentionally exposes the same
// manual redirect and raw response fields as Client, while keeping the
// bogdanfinn fork isolated from the rest of the protocol packages.
import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	fhttptrace "github.com/bogdanfinn/fhttp/httptrace"
	tlsclient "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
	"golang.org/x/net/proxy"
)

type FingerprintClient struct {
	client         tlsclient.HttpClient
	profile        string
	jar            *CookieJar
	mu             sync.RWMutex
	requestGate    chan struct{}
	dialer         *fingerprintDialer
	curlCompatible bool
}

// SetJar replaces the protocol-visible cookie jar.
func (c *FingerprintClient) SetJar(jar *CookieJar) {
	if jar != nil {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.jar = jar
	}
}

func NewFingerprintClient(profile, proxy string, jar *CookieJar) (*FingerprintClient, error) {
	if jar == nil {
		jar = NewCookieJar()
	}
	name := ""
	for _, candidate := range ProfileCandidates(profile) {
		if _, ok := profiles.MappedTLSClients[candidate]; ok {
			name = candidate
			break
		}
	}
	if name == "" {
		return nil, &TransportError{kind: failureProfile}
	}
	return newFingerprintClientExact(name, proxy, jar)
}

// NewFingerprintClientExact retains an explicitly selected TLS identity. It
// never probes or substitutes another profile, including for bound sessions.
func NewFingerprintClientExact(profile, proxy string, jar *CookieJar, extraOptions ...tlsclient.HttpClientOption) (*FingerprintClient, error) {
	return newFingerprintClientExact(normalizeProfile(profile), proxy, jar, extraOptions...)
}
func newFingerprintClientExact(name, proxyURL string, jar *CookieJar, extraOptions ...tlsclient.HttpClientOption) (*FingerprintClient, error) {
	return newFingerprintClientWithMode(name, proxyURL, jar, false, extraOptions...)
}

func newFingerprintClientWithMode(name, proxyURL string, jar *CookieJar, curlCompatible bool, extraOptions ...tlsclient.HttpClientOption) (*FingerprintClient, error) {
	if jar == nil {
		jar = NewCookieJar()
	}
	p, ok := profiles.MappedTLSClients[name]
	if !ok {
		return nil, &TransportError{kind: failureProfile}
	}
	// Only the shared CookieJar manages cookies. A second implicit jar would
	// append duplicate Cookie headers and drift from checkpoint/browser state.
	// Every Do/probe owns a context deadline. tls-client's global 30s default
	// would otherwise override longer request budgets configured by the runner.
	opts := []tlsclient.HttpClientOption{tlsclient.WithClientProfile(p), tlsclient.WithNotFollowRedirects(), tlsclient.WithTimeoutSeconds(0)}
	// Compatibility is deliberately scoped to the profile compared with the
	// original curl_cffi transport. Other bound profiles retain their identity.
	curlCompatible = curlCompatible && name == "chrome_146"
	if curlCompatible {
		opts[0] = tlsclient.WithClientProfile(curlCompatibleChrome146(p))
		opts = append(opts, tlsclient.WithRandomTLSExtensionOrder())
	}
	opts = append(opts, extraOptions...)
	u, e := ParseProxy(proxyURL)
	if e != nil {
		return nil, &TransportError{kind: failureProxyConfig}
	}
	dialer := &fingerprintDialer{}
	// The custom dialer below implements HTTP(S) CONNECT and SOCKS itself;
	// newer tls-client versions reject a simultaneous built-in proxy URL.
	opts = append(opts, tlsclient.WithProxyDialerFactory(func(_ string, t time.Duration, addr *net.TCPAddr, _ fhttp.Header, _ tlsclient.Logger) (proxy.ContextDialer, error) {
		var base proxy.ContextDialer
		switch {
		case u == nil:
			base = &net.Dialer{Timeout: t, KeepAlive: 30 * time.Second, LocalAddr: addr}
		case u.Scheme == "socks5" || u.Scheme == "socks5h":
			var err error
			base, err = newSOCKSDialer(u, t, addr)
			if err != nil {
				return nil, err
			}
		default:
			base = &connectProxyDialer{proxyURL: u, timeout: t, localAddr: addr}
		}
		dialer.base = base
		return dialer, nil
	}))
	c, e := tlsclient.NewHttpClient(nil, opts...)
	if e != nil {
		return nil, &TransportError{kind: failureInit}
	}
	return &FingerprintClient{client: c, profile: name, jar: jar, requestGate: make(chan struct{}, 1), dialer: dialer, curlCompatible: curlCompatible}, nil
}
func (c *FingerprintClient) Profile() string { return c.profile }
func (c *FingerprintClient) Close() {
	if c != nil && c.client != nil {
		c.client.CloseIdleConnections()
	}
}
func (c *FingerprintClient) Do(ctx context.Context, req Request) (*Response, error) {
	return c.do(ctx, req, false)
}

// A TLS probe is complete when HTTP headers arrive. A slow challenge body must
// not turn a valid HTTP response into a reason to select a different identity.
func (c *FingerprintClient) probeHTTP(ctx context.Context, req Request) error {
	_, err := c.do(ctx, req, true)
	return err
}

func (c *FingerprintClient) do(ctx context.Context, req Request, headersOnly bool) (*Response, error) {
	if c == nil || c.client == nil {
		return nil, errors.New("nil fingerprint client")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	select {
	case c.requestGate <- struct{}{}:
	case <-ctx.Done():
		failure := classifyRequestError(nil, ctx.Err())
		failure.requestNotSent = true
		return nil, failure
	}
	scope := newRequestDialScope(ctx)
	c.dialer.bind(scope)
	defer func() {
		scope.finish(false)
		c.dialer.bind(nil)
		<-c.requestGate
	}()
	ctx = context.WithValue(ctx, requestDialScopeKey{}, scope)
	// GotConn fires only after TLS has completed and HTTP can be sent. Merely
	// not observing WroteHeaders does not establish that a write was unsent.
	var gotConn, wrote atomic.Bool
	ctx = fhttptrace.WithClientTrace(ctx, &fhttptrace.ClientTrace{
		WroteHeaders: func() { wrote.Store(true) },
		GotConn: func(info fhttptrace.GotConnInfo) {
			repeated := gotConn.Swap(true)
			if req.SingleAttempt && (repeated || wrote.Load()) {
				cancel()
				_ = info.Conn.Close()
			}
		},
	})
	c.mu.RLock()
	jar := c.jar
	c.mu.RUnlock()
	method := req.Method
	if method == "" {
		method = http.MethodGet
	}
	body := bytes.NewReader(req.Body)
	r, e := fhttp.NewRequestWithContext(ctx, method, req.URL, body)
	if e != nil {
		return nil, &TransportError{kind: failureInvalidRequest}
	}
	if req.SingleAttempt || method != http.MethodGet && method != http.MethodHead {
		// Passwords, OTPs and OAuth exchanges are single attempts. A fresh
		// connection plus a non-rewindable body prevents fhttp from replaying a
		// write after an idle-connection failure or HTTP/2 retry signal.
		c.client.CloseIdleConnections()
		r.GetBody = nil
		r.Close = true
	}
	for k, vs := range req.Headers {
		// fhttp's HTTP/2 encoder checks the lowercase map key, while Header.Add
		// canonicalizes it. Copying Content-Length would therefore emit it twice.
		// NewRequest already derives the authoritative length from req.Body.
		if strings.EqualFold(k, "Content-Length") {
			continue
		}
		for _, v := range vs {
			r.Header.Add(k, v)
		}
	}
	identity := ProfileFor(c.profile)
	r.Header.Set("User-Agent", identity.UserAgent)
	r.Header.Set("Sec-CH-UA", identity.ClientHints)
	r.Header.Set("Sec-CH-UA-Mobile", "?0")
	r.Header.Set("Sec-CH-UA-Platform", `"`+identity.Platform+`"`)
	if r.Header.Get("Accept-Language") == "" {
		r.Header.Set("Accept-Language", identity.AcceptLanguage)
	}
	if ck := jar.HeaderFor(req.URL); ck != "" && r.Header.Get("Cookie") == "" {
		r.Header.Set("Cookie", ck)
	}
	if c.curlCompatible {
		applyCurlCompatibleHeaders(r.Header)
	}
	resp, e := c.client.Do(r)
	if e != nil {
		failure := classifyRequestError(e, ctx.Err())
		failure.requestNotSent = !gotConn.Load()
		return nil, failure
	}
	defer resp.Body.Close()
	hdr := http.Header{}
	for k, vs := range resp.Header {
		for _, v := range vs {
			hdr.Add(k, v)
		}
	}
	// Session cookies take effect at response headers, not at body EOF. A
	// canceled/truncated body must not resurrect deleted authentication state.
	jar.SetFromResponse(req.URL, hdr)
	var b []byte
	if !headersOnly {
		b, e = io.ReadAll(resp.Body)
	}
	response := &Response{StatusCode: resp.StatusCode, Header: hdr, Body: b, URL: resp.Request.URL, Cookies: jar.Cookies(), RawSetCookie: GetSetCookie(hdr)}
	// A clean EOF does not override a cancellation observed before completion.
	if cause := ctx.Err(); cause != nil {
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return response, classifyRequestError(e, cause)
		}
		return nil, classifyRequestError(e, cause)
	}
	if e != nil {
		failure := classifyRequestError(e, nil)
		if IsRetryableReadError(failure) {
			failure.kind = failureBody
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return response, failure
		}
		return nil, failure
	}
	return response, nil
}
