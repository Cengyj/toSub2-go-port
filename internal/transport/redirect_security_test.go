package transport

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFollowPreservesHEADAcrossAllRedirectCodes(t *testing.T) {
	for _, status := range []int{301, 302, 303, 307, 308} {
		destinationMethod := ""
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/start" {
				w.Header().Set("Location", "/destination")
				w.WriteHeader(status)
				return
			}
			destinationMethod = r.Method
			w.WriteHeader(204)
		}))
		result, err := NewClient(nil).Follow(context.Background(), Request{Method: http.MethodHead, URL: server.URL + "/start"}, 3)
		server.Close()
		if err != nil || result.Response.StatusCode != 204 || destinationMethod != http.MethodHead {
			t.Fatalf("HEAD status=%d method=%s err=%v", status, destinationMethod, err)
		}
	}
}

func TestFollowDropsCredentialsAcrossOrigins(t *testing.T) {
	var leaked bool
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, key := range []string{"Authorization", "Proxy-Authorization", "Cookie", "OpenAI-Sentinel-Token", "OpenAI-Sentinel-SO-Token", "X-API-Key"} {
			leaked = leaked || r.Header.Get(key) != ""
		}
		w.WriteHeader(204)
	}))
	defer destination.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", destination.URL)
		w.WriteHeader(307)
	}))
	defer source.Close()
	headers := http.Header{}
	for _, key := range []string{"Authorization", "Proxy-Authorization", "Cookie", "OpenAI-Sentinel-Token", "OpenAI-Sentinel-SO-Token", "X-API-Key"} {
		headers.Set(key, "private-value")
		headers[strings.ToLower(key)] = []string{"lowercase-private-value"}
	}
	if _, err := NewClient(nil).Follow(context.Background(), Request{Method: http.MethodPost, URL: source.URL, Headers: headers, Body: []byte("fixture")}, 3); err != nil {
		t.Fatal(err)
	}
	if leaked {
		t.Fatal("authentication headers crossed origins")
	}
	if headers.Get("Authorization") == "" {
		t.Fatal("caller request headers were mutated")
	}
}

func TestHTTPErrorsNeverIncludeURLCredentials(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	server.Close()
	_, err := NewClient(nil).Do(context.Background(), Request{URL: server.URL + "/?code=fixture-secret"})
	if err == nil || strings.Contains(err.Error(), "fixture-secret") {
		t.Fatal("transport error includes credential query")
	}
}

func TestLoopbackCallbackRecognition(t *testing.T) {
	for _, raw := range []string{"http://localhost:1455/auth/callback?code=fixture", "http://127.0.0.1:1455/auth/callback", "http://[::1]:4444/auth/callback"} {
		if !IsLocalCallback(raw) {
			t.Errorf("loopback callback not recognized: %s", raw)
		}
	}
	for _, raw := range []string{"http://localhost.attacker.test:1455/auth/callback", "http://example.com:1455/auth/callback", "http://localhost:1234/auth/callback-fake", "file://localhost/auth/callback"} {
		if IsLocalCallback(raw) {
			t.Errorf("non callback accepted: %s", raw)
		}
	}
}
