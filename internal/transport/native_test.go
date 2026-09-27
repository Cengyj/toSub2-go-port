package transport

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"
)

func TestNativeTransportExplicitIdentityAndManualCookies(t *testing.T) {
	var redirected atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/unexpected" {
			redirected.Add(1)
			return
		}
		if r.UserAgent() != NativeUserAgent || r.Header.Get("Sec-CH-UA") != "" {
			t.Error("native Go claimed Chromium transport identity")
		}
		w.Header().Add("Set-Cookie", "first=fixture; Path=/; HttpOnly")
		w.Header().Add("Set-Cookie", "second=fixture; Path=/")
		w.Header().Set("Location", "/unexpected")
		w.WriteHeader(302)
	}))
	defer server.Close()
	jar := NewCookieJar()
	client, err := NewNativeClient("", jar)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	response, err := client.Do(context.Background(), Request{URL: server.URL, Headers: http.Header{"User-Agent": {"Chrome/999"}, "Sec-Ch-Ua": {"not-native"}}})
	if err != nil || response.StatusCode != 302 || len(response.RawSetCookie) != 2 || !jar.Has("first") || !jar.Has("second") || redirected.Load() != 0 {
		t.Fatalf("native manual transport: %v", err)
	}
	if client.Profile() != NativeProfile {
		t.Fatal("explicit native transport profile missing")
	}
}

func TestNativeHTTPAndHTTPSProxyAuthentication(t *testing.T) {
	for _, secure := range []bool{false, true} {
		t.Run(fmt.Sprintf("https-%v", secure), func(t *testing.T) {
			expected := "Basic " + base64.StdEncoding.EncodeToString([]byte("fixture-user:fixture-password"))
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Proxy-Authorization") != expected || r.URL.Host != "authorized.invalid" || r.UserAgent() != NativeUserAgent {
					t.Error("native proxy request lost routing, authentication or identity")
				}
				fmt.Fprint(w, "native-proxy-success")
			})
			var server *httptest.Server
			if secure {
				server = httptest.NewTLSServer(handler)
			} else {
				server = httptest.NewServer(handler)
			}
			defer server.Close()
			u, _ := url.Parse(server.URL)
			u.User = url.UserPassword("fixture-user", "fixture-password")
			client, err := NewNativeClient(u.String(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			if secure {
				if _, err = client.Do(context.Background(), Request{URL: "http://authorized.invalid/fixture"}); err == nil {
					t.Fatal("untrusted proxy TLS certificate accepted")
				}
				roots := x509.NewCertPool()
				roots.AddCert(server.Certificate())
				client.HTTP.Transport.(*http.Transport).TLSClientConfig.RootCAs = roots
			}
			response, err := client.Do(context.Background(), Request{URL: "http://authorized.invalid/fixture"})
			if err != nil || string(response.Body) != "native-proxy-success" {
				t.Fatalf("native proxy: %v", err)
			}
		})
	}
}

func TestNativeRequestContextTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer server.Close()
	client, err := NewNativeClient("", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	_, err = client.Do(context.Background(), Request{URL: server.URL, Timeout: 30 * time.Millisecond})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("native timeout: %v", err)
	}
}
