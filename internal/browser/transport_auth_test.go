package browser

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
	"tosub2-go-port/internal/transport"
)

func TestBrowserTransportCompleteAuthenticationHeaders(t *testing.T) {
	s, ctx := integrationSession(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.URL.Path == "/auth" && (r.Method != "POST" || string(body) != `{"code":"fixture-code"}` || r.Header.Get("OpenAI-Sentinel-Token") == "") {
			t.Error("unmodified navigation escaped instead of intended authenticated POST")
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"accepted":true}`)
	}))
	defer server.Close()
	h := http.Header{"Content-Type": {"application/json"}, "Accept": {"application/json"}, "Referer": {server.URL + "/email-verification"}, "Origin": {server.URL}, "Sec-Fetch-Site": {"same-origin"}, "Sec-Fetch-Mode": {"cors"}, "Sec-Fetch-Dest": {"empty"}, "Oai-Device-Id": {"fixture-device"}, "Openai-Sentinel-Token": {`{"p":"fixture"}`}, "Openai-Sentinel-So-Token": {`{"so":"fixture"}`}}
	_, err := NewTransport(s, nil).Do(ctx, transport.Request{Method: "POST", URL: server.URL + "/auth", Headers: h, Body: []byte(`{"code":"fixture-code"}`)})
	if err != nil {
		var diagnostic *cdpError
		if errors.As(err, &diagnostic) {
			t.Fatalf("fixture CDP error: %s: %s", err, diagnostic.Message)
		}
		t.Fatal(err)
	}
}

func TestBrowserTransportMalformedHeadersNeverSendOriginalNavigation(t *testing.T) {
	s, ctx := integrationSession(t)
	var received atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { received.Add(1); w.WriteHeader(204) }))
	defer server.Close()
	_, err := NewTransport(s, nil).Do(ctx, transport.Request{Method: http.MethodPost, URL: server.URL + "/account-write", Headers: http.Header{"Openai-Sentinel-Token": {"invalid\nvalue"}}, Body: []byte(`{"fixture":true}`)})
	if err == nil {
		t.Fatal("invalid request header accepted")
	}
	time.Sleep(100 * time.Millisecond)
	if received.Load() != 0 {
		t.Fatal("original navigation escaped after intercepted request failed")
	}
}

func TestBrowserTransportRejectedCDPRewriteDoesNotReleaseNavigation(t *testing.T) {
	s, ctx := integrationSession(t)
	var received atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { received.Add(1); w.WriteHeader(204) }))
	defer server.Close()
	_, err := NewTransport(s, nil).Do(ctx, transport.Request{Method: "INVALID METHOD", URL: server.URL + "/account-write", Body: []byte(`{"fixture":true}`)})
	if err == nil {
		t.Fatal("CDP accepted an invalid HTTP method")
	}
	time.Sleep(100 * time.Millisecond)
	if received.Load() != 0 {
		t.Fatal("original GET escaped after Fetch.continueRequest rejected the rewrite")
	}
	if value, err := s.Evaluate(ctx, "1"); err != nil || value != float64(1) {
		t.Fatal("request tab cleanup destroyed SDK page")
	}
}
