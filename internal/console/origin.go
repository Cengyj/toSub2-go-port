package console

import (
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Bootstrap contains the local startup token. Host validation prevents DNS
// rebinding, while Origin and Fetch Metadata checks reject cross-site access
// even before a browser has obtained the token.
func (s *Server) validRequestOrigin(r *http.Request) bool {
	host, port, ok := splitAuthority(r.Host)
	if !ok || !s.allowedHost(host) {
		return false
	}
	if s.cfg.Port > 0 {
		actual := port
		if actual == "" {
			actual = "80"
			if r.TLS != nil {
				actual = "443"
			}
		}
		if actual != strconv.Itoa(s.cfg.Port) {
			return false
		}
	}
	if strings.EqualFold(r.Header.Get("Sec-Fetch-Site"), "cross-site") {
		return false
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		u, err := url.Parse(origin)
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		if err != nil || u.Scheme != scheme || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
			return false
		}
		originHost, originPort, ok := splitAuthority(u.Host)
		if !ok || originHost != host {
			return false
		}
		defaultPort := "80"
		if scheme == "https" {
			defaultPort = "443"
		}
		if port == "" {
			port = defaultPort
		}
		if originPort == "" {
			originPort = defaultPort
		}
		if originPort != port {
			return false
		}
	}
	return true
}
func splitAuthority(authority string) (string, string, bool) {
	if authority == "" || strings.ContainsAny(authority, " /\\\t\r\n@?#") {
		return "", "", false
	}
	host, port, err := net.SplitHostPort(authority)
	if err != nil {
		host = authority
		port = ""
		if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
			host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
		}
		if strings.Contains(host, ":") && net.ParseIP(host) == nil {
			return "", "", false
		}
	}
	if port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return "", "", false
		}
	}
	host = strings.ToLower(host)
	if host == "" || strings.HasSuffix(host, ".") {
		return "", "", false
	}
	return host, port, true
}
func (s *Server) allowedHost(host string) bool {
	configured := strings.ToLower(strings.TrimSpace(s.cfg.Host))
	if configured == "" {
		configured = "127.0.0.1"
	}
	configured = strings.Trim(configured, "[]")
	ip := net.ParseIP(host)
	bindIP := net.ParseIP(configured)
	if configured == "localhost" || (bindIP != nil && bindIP.IsLoopback()) {
		return host == "localhost" || (ip != nil && ip.IsLoopback())
	}
	if bindIP != nil && bindIP.IsUnspecified() {
		if host == "localhost" || (ip != nil && ip.IsLoopback()) {
			return true
		}
		if ip == nil {
			return false
		}
		addrs, err := net.InterfaceAddrs()
		if err != nil {
			return false
		}
		for _, a := range addrs {
			if subnet, ok := a.(*net.IPNet); ok && subnet.IP.Equal(ip) {
				return true
			}
		}
		return false
	}
	if bindIP != nil {
		return ip != nil && bindIP.Equal(ip)
	}
	return host == configured
}
