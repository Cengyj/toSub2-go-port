package transport

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync/atomic"
	"time"
)

type ProxySession struct {
	Template string
	Current  atomic.Value
	Counter  atomic.Uint64
}

func NewProxySession(template string) *ProxySession {
	p := &ProxySession{Template: strings.TrimSpace(template)}
	p.Current.Store("")
	return p
}

var sessionMarker = regexp.MustCompile(`(?i)(^|[-_])(sid|session)[-_]([A-Za-z0-9]+)`)
var numericSession = regexp.MustCompile(`(?i)^(.*-[A-Za-z]{2})-([0-9]{8})-([0-9]+m)$`)

func randomSession() string {
	b := make([]byte, 6)
	if _, e := rand.Read(b); e != nil {
		return ""
	}
	return hex.EncodeToString(b)
}
func replaceSession(value, id string) string {
	return sessionMarker.ReplaceAllStringFunc(value, func(match string) string {
		parts := sessionMarker.FindStringSubmatch(match)
		prefix := match[:len(match)-len(parts[3])]
		return prefix + id
	})
}
func (p *ProxySession) Rotate() string {
	p.Counter.Add(1)
	v := p.Template
	if v == "" {
		p.Current.Store("")
		return ""
	}
	id := randomSession()
	if id == "" {
		return p.URL()
	}
	v = strings.ReplaceAll(strings.ReplaceAll(v, "{sid}", id), "{session}", id)
	if u, e := url.Parse(v); e == nil && u.Host != "" {
		if u.User != nil {
			user := replaceSession(u.User.Username(), id)
			password, hasPassword := u.User.Password()
			password = replaceSession(password, id)
			if m := numericSession.FindStringSubmatch(password); len(m) > 0 {
				n, e := rand.Int(rand.Reader, big.NewInt(90000000))
				if e == nil {
					password = fmt.Sprintf("%s-%08d-%s", m[1], n.Int64()+10000000, m[3])
				}
			}
			if hasPassword {
				u.User = url.UserPassword(user, password)
			} else {
				u.User = url.User(user)
			}
		}
		q := u.Query()
		for _, key := range []string{"sid", "session"} {
			if q.Has(key) {
				q.Set(key, id)
			}
		}
		u.RawQuery = q.Encode()
		v = u.String()
	}
	p.Current.Store(v)
	return v
}
func (p *ProxySession) URL() string {
	if v, ok := p.Current.Load().(string); ok && v != "" {
		return v
	}
	return p.Template
}
func ProxySupportsSessionRotation(raw string) bool {
	if strings.Contains(raw, "{sid}") || strings.Contains(raw, "{session}") {
		return true
	}
	u, e := url.Parse(raw)
	if e != nil {
		return false
	}
	if u.Query().Has("sid") || u.Query().Has("session") {
		return true
	}
	if u.User != nil {
		pass, _ := u.User.Password()
		return sessionMarker.MatchString(u.User.Username()) || sessionMarker.MatchString(pass) || numericSession.MatchString(pass)
	}
	return false
}
func ParseProxy(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	u, e := url.Parse(raw)
	if e != nil || u.Host == "" {
		return nil, errors.New("invalid proxy URL")
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https", "socks5", "socks5h":
	default:
		return nil, fmt.Errorf("unsupported proxy scheme %q", u.Scheme)
	}
	return u, nil
}
func NewHTTPClient(proxyURL string, timeout time.Duration) (*http.Client, error) {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return newHTTPClient(proxyURL, timeout)
}

// A zero timeout is used only by the protocol Client, whose Do always supplies
// a bounded context. Other callers keep NewHTTPClient's explicit/default limit.
func newHTTPClient(proxyURL string, timeout time.Duration) (*http.Client, error) {
	u, e := ParseProxy(proxyURL)
	if e != nil {
		return nil, e
	}
	tr := &http.Transport{Proxy: http.ProxyURL(u), TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}, DialContext: (&net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}).DialContext}
	if u != nil && (u.Scheme == "socks5" || u.Scheme == "socks5h") {
		d, err := newSOCKSDialer(u, timeout, nil)
		if err != nil {
			return nil, err
		}
		tr.Proxy = nil
		tr.DialContext = d.DialContext
	}
	preserveNativeDialScope(tr)
	return &http.Client{Transport: tr, Timeout: timeout, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}, nil
}
func DoWithRetry(ctx context.Context, c *Client, req Request, max int) (*Response, error) {
	if max < 1 {
		max = 1
	}
	var last error
	for i := 0; i < max; i++ {
		r, e := c.Do(ctx, req)
		if e == nil && r.StatusCode < 500 {
			return r, nil
		}
		last = e
		if r != nil {
			last = fmt.Errorf("HTTP %d", r.StatusCode)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Duration(i+1) * 200 * time.Millisecond):
		}
	}
	if last == nil {
		last = errors.New("request failed")
	}
	return nil, last
}
