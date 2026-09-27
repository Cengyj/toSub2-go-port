package console

import (
	"net/http/httptest"
	"testing"

	"tosub2-go-port/internal/config"
)

func TestConsoleHostOriginAndBootstrapProtection(t *testing.T) {
	srv, _ := newMonitorTestServer(t)
	srv.cfg.Host = "127.0.0.1"
	srv.cfg.Port = 4399
	cases := []struct {
		name, host, origin, fetch string
		allowed                   bool
	}{
		{"local", "127.0.0.1:4399", "", "", true},
		{"localhost", "localhost:4399", "http://localhost:4399", "same-origin", true},
		{"ipv6 loopback", "[::1]:4399", "http://[::1]:4399", "", true},
		{"rebinding", "attacker.example:4399", "", "", false},
		{"evil origin", "127.0.0.1:4399", "https://evil.example", "", false},
		{"other localhost origin", "127.0.0.1:4399", "http://127.0.0.1:8080", "", false},
		{"opaque origin", "127.0.0.1:4399", "null", "", false},
		{"cross site", "127.0.0.1:4399", "", "cross-site", false},
		{"wrong port", "localhost:8080", "", "", false},
		{"userinfo", "evil@localhost:4399", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/api/bootstrap", nil)
			r.Host = tc.host
			if tc.origin != "" {
				r.Header.Set("Origin", tc.origin)
			}
			if tc.fetch != "" {
				r.Header.Set("Sec-Fetch-Site", tc.fetch)
			}
			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, r)
			if (w.Code == 200) != tc.allowed {
				t.Fatalf("status %d", w.Code)
			}
		})
	}
}
func TestExplicitLANAndHostname(t *testing.T) {
	for _, tc := range []struct {
		bind, host string
		allowed    bool
	}{
		{"192.168.1.4", "192.168.1.4", true}, {"192.168.1.4", "evil.test", false}, {"my-console.internal", "my-console.internal", true}, {"my-console.internal", "evil.test", false}, {"0.0.0.0", "evil.test", false}, {"0.0.0.0", "127.0.0.1", true},
	} {
		s := &Server{cfg: config.Config{Host: tc.bind}}
		if s.allowedHost(tc.host) != tc.allowed {
			t.Fatal(tc)
		}
	}
}
