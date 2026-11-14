// Package transport contains the HTTP plumbing used by the protocol client.
package transport

import (
	"errors"
	"math"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/publicsuffix"
)

// Cookie is a transient HTTP cookie. Expires is Unix time in milliseconds;
// zero means no explicit expiry.
type Cookie struct {
	Name     string `json:"-"`
	Value    string `json:"-"`
	Domain   string `json:"-"`
	Path     string `json:"-"`
	Secure   bool   `json:"-"`
	Expires  int64  `json:"-"`
	HostOnly bool   `json:"-"`
	HTTPOnly bool   `json:"-"`
	SameSite string `json:"-"`
	// Partitioned cookies belong to a top-level site, not just name/domain/path.
	// Keep their key only in memory and never send them as ordinary
	// unpartitioned cookies through a transport with no top-level site context.
	Partitioned        bool                `json:"-"`
	PartitionKey       *CookiePartitionKey `json:"-"`
	PartitionKeyOpaque bool                `json:"-"`
}

type CookiePartitionKey struct {
	TopLevelSite         string `json:"-"`
	HasCrossSiteAncestor bool   `json:"-"`
}

// Cookies are transient transport data and cannot be imported or exported.
func (Cookie) MarshalJSON() ([]byte, error) {
	return nil, errors.New("cookie persistence is not supported")
}
func (*Cookie) UnmarshalJSON([]byte) error { return errors.New("cookie persistence is not supported") }

// CookieJar is a small RFC6265-compatible jar. It intentionally stores host
// and path information for requests within the current operation.
type CookieJar struct {
	mu      sync.RWMutex
	cookies []Cookie
}

func NewCookieJar(cookies ...Cookie) *CookieJar { j := &CookieJar{}; j.Merge(cookies); return j }

func (j *CookieJar) Cookies() []Cookie {
	j.mu.RLock()
	defer j.mu.RUnlock()
	var out []Cookie
	now := time.Now().UnixMilli()
	for _, c := range j.cookies {
		if c.Expires <= 0 || c.Expires > now {
			out = append(out, copyCookie(c))
		}
	}
	return out
}

// SetFromResponse consumes all Set-Cookie values from h for requestURL.
func (j *CookieJar) SetFromResponse(requestURL string, h http.Header) {
	u, err := url.Parse(requestURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return
	}
	for _, line := range GetSetCookie(h) {
		if c, ok := ParseSetCookie(line, requestURL); ok {
			j.mu.Lock()
			if u.Scheme == "https" || !j.shadowsSecureCookieLocked(c) {
				j.setLocked(c)
			}
			j.mu.Unlock()
		}
	}
}

// An insecure origin cannot replace, delete, or place a more specific cookie
// ahead of an existing Secure cookie. Apply the same rule to HTTP and script
// writes; the check and update must share the jar lock.
func (j *CookieJar) shadowsSecureCookieLocked(c Cookie) bool {
	now := time.Now().UnixMilli()
	for _, old := range j.cookies {
		if old.Name == c.Name && old.Secure && (old.Expires <= 0 || old.Expires > now) && sameCookiePartition(old, c) &&
			(c.Domain == old.Domain || strings.HasSuffix(c.Domain, "."+old.Domain) || strings.HasSuffix(old.Domain, "."+c.Domain)) &&
			(c.Path == old.Path || strings.HasPrefix(c.Path, strings.TrimSuffix(old.Path, "/")+"/")) {
			return true
		}
	}
	return false
}

// GetSetCookie returns raw Set-Cookie lines without collapsing commas in an
// Expires attribute. It also handles test servers and older Go transports that
// expose one combined header value.
func GetSetCookie(h http.Header) []string {
	values := h.Values("Set-Cookie")
	var out []string
	for _, value := range values {
		parts := strings.FieldsFunc(value, func(r rune) bool { return r == ',' })
		// A comma inside Expires= is part of the same cookie; only split when the
		// following text looks like a cookie name/value pair.
		if len(parts) > 1 {
			buf := ""
			for _, part := range parts {
				part = strings.TrimSpace(part)
				if buf == "" {
					buf = part
					continue
				}
				eq := strings.IndexByte(part, '=')
				name := ""
				if eq > 0 {
					name = strings.TrimSpace(part[:eq])
				}
				cookieStart := eq > 0 && name != "" && !strings.ContainsAny(name, "; \t")
				if cookieStart {
					out = append(out, buf)
					buf = part
				} else {
					buf += ", " + part
				}
			}
			if buf != "" {
				out = append(out, buf)
			}
		} else if strings.TrimSpace(value) != "" {
			out = append(out, strings.TrimSpace(value))
		}
	}
	return out
}

func (j *CookieJar) Set(c Cookie) {
	if c.Name == "" || c.Domain == "" {
		return
	}
	c.Domain = strings.ToLower(strings.TrimPrefix(c.Domain, "."))
	if strings.HasPrefix(c.Name, "__Host-") {
		c.HostOnly = true
	}
	c.SameSite = normalizeSameSite(c.SameSite)
	c = copyCookie(c)
	c.Partitioned = c.Partitioned || c.PartitionKey != nil || c.PartitionKeyOpaque
	if c.Path == "" {
		c.Path = "/"
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	j.setLocked(c)
}

func (j *CookieJar) setLocked(c Cookie) {
	for i, old := range j.cookies {
		if old.Name == c.Name && strings.EqualFold(old.Domain, c.Domain) && old.Path == c.Path && sameCookiePartition(old, c) {
			if c.Expires > 0 && c.Expires <= time.Now().UnixMilli() {
				j.cookies = append(j.cookies[:i], j.cookies[i+1:]...)
			} else {
				j.cookies[i] = c
			}
			return
		}
	}
	if c.Expires <= 0 || c.Expires > time.Now().UnixMilli() {
		j.cookies = append(j.cookies, c)
	}
}

func (j *CookieJar) Merge(cookies []Cookie) {
	for _, c := range cookies {
		j.Set(c)
	}
}

func (j *CookieJar) HeaderFor(rawURL string) string {
	return j.headerFor(rawURL, false)
}

// DocumentCookie exposes only cookies visible to JavaScript at this document.
// The network and script host use the same authoritative jar.
func (j *CookieJar) DocumentCookie(rawURL string) string {
	return j.headerFor(rawURL, true)
}

// SetDocumentCookie applies the non-HTTP cookie API. Scripts cannot create or
// replace HttpOnly cookies or overwrite a secure cookie from an insecure page.
func (j *CookieJar) SetDocumentCookie(rawURL, value string) {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return
	}
	c, ok := ParseSetCookie(value, rawURL)
	if !ok || c.HTTPOnly || c.Partitioned || c.Secure && u.Scheme != "https" {
		return
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if u.Scheme != "https" && j.shadowsSecureCookieLocked(c) {
		return
	}
	for _, old := range j.cookies {
		if old.Name != c.Name || old.Expires > 0 && old.Expires <= time.Now().UnixMilli() {
			continue
		}
		if old.HTTPOnly && old.Domain == c.Domain && old.Path == c.Path && sameCookiePartition(old, c) {
			return
		}
	}
	j.setLocked(c)
}

func (j *CookieJar) headerFor(rawURL string, document bool) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	now := time.Now().UnixMilli()
	host := strings.ToLower(u.Hostname())
	secure := strings.EqualFold(u.Scheme, "https")
	j.mu.Lock()
	defer j.mu.Unlock()
	filtered := j.cookies[:0]
	matched := make([]Cookie, 0, len(j.cookies))
	for _, c := range j.cookies {
		if c.Expires > 0 && c.Expires <= now {
			continue
		}
		filtered = append(filtered, c)
		if (!document || !c.HTTPOnly) && cookieMatches(c, host, u.EscapedPath(), secure) {
			matched = append(matched, c)
		}
	}
	j.cookies = filtered
	// RFC6265 puts the most specific path first; stable ordering preserves
	// creation order for equal paths and compatibility with existing jars.
	sort.SliceStable(matched, func(i, k int) bool { return len(matched[i].Path) > len(matched[k].Path) })
	vals := make([]string, 0, len(matched))
	for _, c := range matched {
		vals = append(vals, c.Name+"="+c.Value)
	}
	return strings.Join(vals, "; ")
}

func (j *CookieJar) NamesFor(rawURL string) []string {
	h, _ := url.Parse(rawURL)
	if h == nil {
		return nil
	}
	jar := j.HeaderFor(rawURL)
	if jar == "" {
		return nil
	}
	parts := strings.Split(jar, "; ")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		out = append(out, strings.TrimSpace(strings.SplitN(p, "=", 2)[0]))
	}
	return out
}
func (j *CookieJar) Has(name string) bool {
	j.mu.RLock()
	defer j.mu.RUnlock()
	for _, c := range j.cookies {
		if c.Name == name && (c.Expires <= 0 || c.Expires > time.Now().UnixMilli()) {
			return true
		}
	}
	return false
}
func (j *CookieJar) Value(name, rawURL string) string {
	h, _ := url.Parse(rawURL)
	if h == nil {
		return ""
	}
	j.mu.RLock()
	defer j.mu.RUnlock()
	value, longestPath := "", -1
	now := time.Now().UnixMilli()
	for _, c := range j.cookies {
		if c.Name == name && (c.Expires <= 0 || c.Expires > now) && len(c.Path) > longestPath && cookieMatches(c, strings.ToLower(h.Hostname()), h.EscapedPath(), strings.EqualFold(h.Scheme, "https")) {
			value, longestPath = c.Value, len(c.Path)
		}
	}
	return value
}

func cookieMatches(c Cookie, host, reqPath string, secure bool) bool {
	if c.Partitioned || c.PartitionKey != nil || c.PartitionKeyOpaque {
		return false
	}
	domain := strings.ToLower(strings.TrimPrefix(c.Domain, "."))
	if c.HostOnly {
		if host != domain {
			return false
		}
	} else if host != domain && !strings.HasSuffix(host, "."+domain) {
		return false
	}
	if c.Secure && !secure {
		return false
	}
	if reqPath == "" {
		reqPath = "/"
	}
	p := c.Path
	if p == "" {
		p = "/"
	}
	return reqPath == p || strings.HasPrefix(reqPath, strings.TrimSuffix(p, "/")+"/") || (p == "/" && strings.HasPrefix(reqPath, "/"))
}

// ParseSetCookie parses a single Set-Cookie header. Invalid/nameless cookies
// are ignored by returning ok=false.
func ParseSetCookie(line, requestURL string) (Cookie, bool) {
	u, err := url.Parse(requestURL)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return Cookie{}, false
	}
	parsed := (&http.Response{Header: http.Header{"Set-Cookie": {line}}}).Cookies()
	if len(parsed) != 1 {
		return Cookie{}, false
	}
	p := parsed[0]
	host := strings.ToLower(u.Hostname())
	c := Cookie{Name: p.Name, Value: p.Value, Domain: host, Path: p.Path, HostOnly: true, Secure: p.Secure, HTTPOnly: p.HttpOnly}
	if c.Secure && u.Scheme != "https" {
		return Cookie{}, false
	}
	for _, attribute := range strings.Split(line, ";")[1:] {
		if strings.EqualFold(strings.TrimSpace(attribute), "Partitioned") {
			if !c.Secure {
				return Cookie{}, false
			}
			// Set-Cookie alone does not disclose the embedding top-level site. Keep
			// this marker instead of inventing a key or dropping the partition.
			c.Partitioned = true
		}
	}
	if p.Domain != "" {
		domain := strings.ToLower(strings.TrimPrefix(p.Domain, "."))
		if domain == "" || strings.HasSuffix(domain, ".") || (host != domain && (net.ParseIP(host) != nil || !strings.HasSuffix(host, "."+domain))) {
			return Cookie{}, false
		}
		suffix, _ := publicsuffix.PublicSuffix(domain)
		if suffix == domain && host != domain {
			return Cookie{}, false
		}
		c.Domain, c.HostOnly = domain, suffix == domain || net.ParseIP(host) != nil
	}
	if c.Path == "" || c.Path[0] != '/' {
		c.Path = "/"
		if path := u.EscapedPath(); strings.HasPrefix(path, "/") {
			if at := strings.LastIndexByte(path, '/'); at > 0 {
				c.Path = path[:at]
			}
		}
	}
	if strings.HasPrefix(c.Name, "__Secure-") || strings.HasPrefix(c.Name, "__Host-") {
		if !c.Secure || !strings.EqualFold(u.Scheme, "https") {
			return Cookie{}, false
		}
		if strings.HasPrefix(c.Name, "__Host-") && (p.Domain != "" || p.Path != "/") {
			return Cookie{}, false
		}
	}
	switch p.SameSite {
	case http.SameSiteLaxMode:
		c.SameSite = "Lax"
	case http.SameSiteStrictMode:
		c.SameSite = "Strict"
	case http.SameSiteNoneMode:
		c.SameSite = "None"
	}
	if !p.Expires.IsZero() {
		c.Expires = p.Expires.UnixMilli()
		if c.Expires <= 0 {
			c.Expires = 1
		}
	}
	// Max-Age takes precedence regardless of attribute order. Bound arithmetic
	// so a very large server-provided lifetime cannot overflow into a session.
	if p.MaxAge < 0 {
		c.Expires = 1
	} else if p.MaxAge > 0 {
		now := time.Now().UnixMilli()
		if int64(p.MaxAge) > (math.MaxInt64-now)/1000 {
			c.Expires = math.MaxInt64
		} else {
			c.Expires = now + int64(p.MaxAge)*1000
		}
	}
	return c, true
}

func normalizeSameSite(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "lax":
		return "Lax"
	case "strict":
		return "Strict"
	case "none":
		return "None"
	default:
		return ""
	}
}

func copyCookie(c Cookie) Cookie {
	if c.PartitionKey != nil {
		key := *c.PartitionKey
		c.PartitionKey = &key
	}
	return c
}
func sameCookiePartition(a, b Cookie) bool {
	if a.Partitioned != b.Partitioned || a.PartitionKeyOpaque != b.PartitionKeyOpaque {
		return false
	}
	if a.PartitionKey == nil || b.PartitionKey == nil {
		return a.PartitionKey == nil && b.PartitionKey == nil
	}
	return *a.PartitionKey == *b.PartitionKey
}
