package browser

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"tosub2-go-port/internal/transport"
)

func TestBrowserTransportNormalizesNavigationURL(t *testing.T) {
	session, ctx := integrationSession(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("fixture-response"))
	}))
	defer srv.Close()
	client := NewTransport(session, nil)
	for _, target := range []string{srv.URL, srv.URL + "/endpoint#resume", srv.URL + "/contains space?q=one", srv.URL + "/parent/../endpoint", strings.Replace(srv.URL, "127.0.0.1", "LOCALHOST", 1) + "/endpoint"} {
		t.Run(target, func(t *testing.T) {
			resp, err := client.Do(ctx, transport.Request{URL: target, Timeout: 3 * time.Second})
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != http.StatusOK || string(resp.Body) != "fixture-response" {
				t.Fatalf("response not intercepted: status=%d", resp.StatusCode)
			}
		})
	}
	for _, input := range []string{"http://EXAMPLE.COM:80/a/../b#fragment", "http://example.com/b"} {
		u, err := canonicalRequestURL(ctx, session, input)
		if err != nil || u.String() != "http://example.com/b" {
			t.Fatalf("canonical URL mismatch: %v %v", u, err)
		}
	}
}
