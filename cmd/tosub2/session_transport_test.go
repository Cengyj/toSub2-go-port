package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"tosub2-go-port/internal/config"
)

func TestPrepareFingerprintHTTPRejectionDoesNotRotateProxy(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		var originCalls, proxyCalls atomic.Int32
		identities := make(chan string, 16)
		origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { originCalls.Add(1); w.WriteHeader(status) }))
		proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			proxyCalls.Add(1)
			identities <- r.Header.Get("Proxy-Authorization")
			if r.Method != http.MethodConnect {
				http.Error(w, "expected CONNECT", 500)
				return
			}
			upstream, err := net.DialTimeout("tcp", strings.TrimPrefix(origin.URL, "http://"), time.Second)
			if err != nil {
				http.Error(w, "fixture connect failed", 500)
				return
			}
			client, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				upstream.Close()
				return
			}
			defer client.Close()
			defer upstream.Close()
			fmt.Fprint(client, "HTTP/1.1 200 Connection Established\r\n\r\n")
			done := make(chan struct{})
			go func() { io.Copy(upstream, client); upstream.Close(); close(done) }()
			io.Copy(client, upstream)
			client.Close()
			<-done
		}))
		cfg := config.Default()
		cfg.ChatGPTBase = origin.URL
		cfg.ProxyURLs = nil
		cfg.ProxyMaxAttempts = 10
		cfg.SameProxyRetries = 3
		cfg.SameProxyRetryDelayMS = 1
		cfg.RequestTimeoutMS = 1000
		selected := "http://user-session-original:fixture-password@" + strings.TrimPrefix(proxy.URL, "http://")
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		client, _, effective, err := prepareFingerprint(ctx, cfg, selected)
		cancel()
		if client != nil {
			client.Close()
		}
		proxy.Close()
		origin.Close()
		if err != nil {
			t.Fatalf("HTTP %d incorrectly treated as connectivity failure: %v", status, err)
		}
		// A fresh automatic profile probe may start four candidates together.
		// A refusal must retain one proxy identity and must not launch a retry
		// batch looking for a different security result.
		if originCalls.Load() < 1 || proxyCalls.Load() < originCalls.Load() || proxyCalls.Load() > 4 || effective == "" {
			t.Fatalf("HTTP %d caused %d origin requests/%d proxy connections", status, originCalls.Load(), proxyCalls.Load())
		}
		close(identities)
		unique := map[string]bool{}
		for identity := range identities {
			unique[identity] = true
		}
		if len(unique) != 1 || unique[""] {
			t.Fatal("HTTP rejection changed the proxy session identity")
		}
	}
}
