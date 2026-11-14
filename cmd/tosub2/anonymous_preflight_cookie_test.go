package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"tosub2-go-port/internal/config"
	"tosub2-go-port/internal/transport"
)

func TestAnonymousAuthenticationConstructionDoesNotCreateCookiesOrSendRequests(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != http.MethodGet || r.URL.Path != "/" || r.Header.Get("Cookie") != "" {
			t.Error("fresh transport sent an unexpected request or cookie")
		}
		http.SetCookie(w, &http.Cookie{Name: "anonymous-visitor", Value: "synthetic-prelogin-cookie", Path: "/", HttpOnly: true})
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	jar := transport.NewCookieJar()
	if len(jar.Cookies()) != 0 {
		t.Fatal("new cookie jar already contained cookies")
	}
	cfg := config.Config{ChatGPTBase: server.URL, AuthBase: server.URL, SentinelSDKURL: server.URL + "/unexpected-sdk", AuthTLSProfile: "chrome146", RequestTimeoutMS: 1000}
	client, err := newLegacyAuthentication(t.Context(), cfg, "", jar)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if requests.Load() != 0 || len(jar.Cookies()) != 0 {
		t.Fatal("constructing the legacy client started networking or manufactured a cookie")
	}
	if _, err := client.Do(t.Context(), transport.Request{Method: http.MethodGet, URL: server.URL + "/", SingleAttempt: true}); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 1 || len(jar.Cookies()) != 1 || jar.Value("anonymous-visitor", server.URL+"/") != "synthetic-prelogin-cookie" {
		t.Fatal("the first server response did not become the first cookie")
	}
}

// Exercise the production legacy preflight, rather than injecting a cookie
// into a protocol fixture. Anonymous homepage responses may set cookies even
// while the same server explicitly reports that no user has logged in.
func TestAnonymousPreflightStoresServerCookiesWithoutLoginAndKeepsJarsSeparate(t *testing.T) {
	type requestObservation struct {
		path, cookie string
	}
	seen := make(chan requestObservation, 8)
	var homeRequests, totalRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		totalRequests.Add(1)
		seen <- requestObservation{path: r.URL.Path, cookie: r.Header.Get("Cookie")}
		if r.Method != http.MethodGet {
			t.Error("anonymous preflight unexpectedly sent a login or mutation")
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		switch r.URL.Path {
		case "/":
			index := homeRequests.Add(1)
			http.SetCookie(w, &http.Cookie{Name: "oai-did", Value: fmt.Sprintf("synthetic-device-%d", index), Path: "/", HttpOnly: true})
			http.SetCookie(w, &http.Cookie{Name: "anonymous-preflight", Value: fmt.Sprintf("synthetic-visitor-%d", index), Path: "/", HttpOnly: true})
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("<html>Anonymous homepage</html>"))
		case "/api/auth/session":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{}`))
		default:
			t.Errorf("preflight requested unexpected endpoint %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	cfg := config.Config{ChatGPTBase: server.URL, AuthBase: server.URL, SentinelSDKURL: server.URL + "/unexpected-sdk", AuthTLSProfile: "chrome146", RequestTimeoutMS: 1000, SameProxyRetries: 0, SameProxyRetryDelaySet: true}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var previous *transport.CookieJar
	for operation := 1; operation <= 2; operation++ {
		client, jar, _, err := prepareLegacyAuthentication(ctx, cfg, "")
		if err != nil {
			t.Fatal("local production preflight failed", err)
		}
		t.Cleanup(func() {
			if err := client.Close(); err != nil {
				t.Error(err)
			}
		})
		first := <-seen
		if first.path != "/" || first.cookie != "" {
			t.Fatal("new operation did not begin with a cookie-free homepage request")
		}
		if len(jar.Cookies()) != 2 || jar.Value("oai-did", server.URL+"/") != fmt.Sprintf("synthetic-device-%d", operation) || jar.Value("anonymous-preflight", server.URL+"/") != fmt.Sprintf("synthetic-visitor-%d", operation) {
			t.Fatal("homepage Set-Cookie was not retained by the returned jar")
		}
		result, err := client.Do(ctx, transport.Request{Method: http.MethodGet, URL: server.URL + "/api/auth/session", SingleAttempt: true})
		if err != nil || result.StatusCode != http.StatusOK || string(result.Body) != `{}` {
			t.Fatal("anonymous session response changed", err)
		}
		session := <-seen
		if session.path != "/api/auth/session" || session.cookie == "" || session.cookie != jar.HeaderFor(server.URL+"/api/auth/session") {
			t.Fatal("retained anonymous cookies were not reused by the same client")
		}
		if previous != nil && (previous == jar || previous.Value("oai-did", server.URL+"/") != "synthetic-device-1") {
			t.Fatal("separate operation reused or changed the previous cookie jar")
		}
		previous = jar
	}
	if homeRequests.Load() != 2 || totalRequests.Load() != 4 {
		t.Fatal("preflight performed requests beyond homepage and read-only identity checks")
	}
}
