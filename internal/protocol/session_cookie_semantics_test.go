package protocol

import (
	"net/http"
	"net/http/httptest"
	"net/url"

	"testing"

	"tosub2-go-port/internal/transport"
)

func opaqueSessionCookie(name string) transport.Cookie {
	return transport.Cookie{Name: name, Value: "synthetic-private-" + name, Domain: "chat.example", Path: "/", HostOnly: true, Secure: true, HTTPOnly: true}
}

func TestOpaqueCookiesPreserveOriginalWireNames(t *testing.T) {
	names := []string{"renamed-session.alpha", "renamed-session.2", "__Secure-next-auth.session-token.01"}
	seen := false
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = true
		if len(r.Cookies()) != len(names) {
			t.Error("transport fabricated or merged opaque cookie names")
		}
		for _, name := range names {
			c, err := r.Cookie(name)
			if err != nil || c.Value != "synthetic-private-"+name {
				t.Error("opaque cookie changed on wire")
			}
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	u, _ := url.Parse(server.URL)
	var cookies []transport.Cookie
	for _, name := range names {
		c := opaqueSessionCookie(name)
		c.Domain = u.Hostname()
		cookies = append(cookies, c)
	}
	jar := transport.NewCookieJar(cookies...)
	client := transport.NewClient(jar)
	client.HTTP = server.Client()
	if _, err := client.Do(t.Context(), transport.Request{URL: server.URL}); err != nil || !seen {
		t.Fatal("opaque original-cookie request failed", err)
	}
}
