package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"tosub2-go-port/internal/config"
	"tosub2-go-port/internal/transport"
)

func TestGoAuthenticationPreflightTimeoutRetryLogsAndBudget(t *testing.T) {
	t.Setenv("CHATGPT_SAME_PROXY_CONNECTION_RETRIES", "")
	for _, recoverAfter := range []int32{2, 99} {
		scenario := "exhausted"
		if recoverAfter == 2 {
			scenario = "recovers"
		}
		t.Run(scenario, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if calls.Add(1) <= recoverAfter {
					<-r.Context().Done()
					return
				}
				w.WriteHeader(http.StatusOK)
			}))
			defer server.Close()
			cfg := config.Default()
			cfg.ChatGPTBase, cfg.AuthTLSProfile, cfg.RequestTimeoutMS = server.URL, "chrome146", 30
			cfg.ProxyURL, cfg.ProxyURLs = "", nil
			cfg.SameProxyRetryDelaySet, cfg.SameProxyRetryDelayMS = true, 0
			if cfg.SameProxyRetries != 2 {
				t.Fatal("default preflight must allow exactly three total requests")
			}
			var events []transport.ReadRetryEvent
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			client, _, _, err := prepareLegacyAuthentication(ctx, cfg, "", func(e transport.ReadRetryEvent) { events = append(events, e) })
			if recoverAfter == 2 {
				if err != nil || calls.Load() != 3 || client == nil || len(events) != 2 {
					t.Fatalf("preflight failed before retry recovery: calls=%d events=%v err=%v", calls.Load(), events, err)
				}
				if err := client.Close(); err != nil {
					t.Fatal(err)
				}
			} else if !errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil || calls.Load() != 3 || len(events) != 3 || !events[2].Exhausted || !strings.Contains(err.Error(), "共 3 次请求") {
				t.Fatalf("preflight exceeded or hid retry budget: calls=%d events=%v err=%v", calls.Load(), events, err)
			}
			for i, event := range events {
				if event.MaxRetries != 2 || (!event.Exhausted && event.Retry != i+1) || !strings.Contains(event.Message(), "重试") {
					t.Fatalf("preflight retry log incorrect: %+v", event)
				}
			}
		})
	}
}

func TestAuthenticationRuntimeStartsWithFreshCookieJar(t *testing.T) {
	jar := transport.NewCookieJar()
	client, err := newLegacyAuthentication(context.Background(), config.Config{}, "", jar)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if len(jar.Cookies()) != 0 {
		t.Fatal("new runtime imported cookies")
	}
}
