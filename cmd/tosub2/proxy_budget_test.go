package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"tosub2-go-port/internal/config"
)

func TestZeroProxySessionBudgetRejectsRotationBeforeNetwork(t *testing.T) {
	var calls atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(200) }))
	defer proxy.Close()
	cfg := config.Config{ChatGPTBase: "http://fixture.test", TLSProfile: "auto", ProxyMaxAttempts: 0, ProxyMaxAttemptsSet: true, RequestTimeoutMS: 200}
	selected := "http://user-session-fixture:synthetic@" + strings.TrimPrefix(proxy.URL, "http://")
	for _, native := range []bool{false, true} {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		var err error
		if native {
			c, _, _, e := prepareNative(ctx, cfg, selected)
			err = e
			if c != nil {
				c.Close()
			}
		} else {
			c, _, _, e := prepareFingerprint(ctx, cfg, selected)
			err = e
			if c != nil {
				c.Close()
			}
		}
		cancel()
		if err == nil || !strings.Contains(err.Error(), "budget exhausted") || calls.Load() != 0 {
			t.Fatal("zero session budget was silently re-enabled", native, err)
		}
	}
	for _, template := range []string{"", proxy.URL} {
		n, err := proxyConnectionAttemptBudget(cfg, []string{template})
		if err != nil || n != 1 {
			t.Fatal("zero session budget disabled direct/fixed transport", err)
		}
	}
	n, err := proxyConnectionAttemptBudget(config.Config{}, []string{selected})
	if err != nil || n != 1 {
		t.Fatal("Config{} mistaken for an explicit zero budget", err)
	}
}

func TestZeroProxyBudgetStillConnectsDirectAndFixedNativeProxy(t *testing.T) {
	var direct, proxied atomic.Int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { direct.Add(1); w.WriteHeader(200) }))
	defer origin.Close()
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { proxied.Add(1); w.WriteHeader(200) }))
	defer proxy.Close()
	cfg := config.Config{ChatGPTBase: origin.URL, TLSProfile: "auto", ProxyMaxAttempts: 0, ProxyMaxAttemptsSet: true, RequestTimeoutMS: 1000}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	fingerprint, _, _, err := prepareFingerprint(ctx, cfg, "")
	if err != nil {
		t.Fatal("direct fingerprint connection refused at zero proxy budget", err)
	}
	fingerprint.Close()
	native, _, _, err := prepareNative(ctx, cfg, "")
	if err != nil {
		t.Fatal("direct native connection refused at zero proxy budget", err)
	}
	native.Close()
	native, _, _, err = prepareNative(ctx, cfg, proxy.URL)
	if err != nil {
		t.Fatal("fixed native proxy refused at zero session budget", err)
	}
	native.Close()
	if direct.Load() < 2 || proxied.Load() != 1 {
		t.Fatal("unexpected direct/fixed attempt count", direct.Load(), proxied.Load())
	}
}

func TestLegacyConnectionRetryDelayGrowthAndExplicitOverride(t *testing.T) {
	cfg := config.Config{ProxyConnectionRetryBaseMS: 1000}
	want := []time.Duration{0, time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 15 * time.Second, 15 * time.Second}
	for n, expected := range want {
		if got := proxyConnectionRetryDelay(cfg, n); got != expected {
			t.Fatal("backoff mismatch", n, got, expected)
		}
	}
	cfg.ProxyConnectionRetryBaseMS = int(^uint(0) >> 1)
	if proxyConnectionRetryDelay(cfg, 20) != 15*time.Second {
		t.Fatal("large backoff overflowed 15 second cap")
	}
	cfg.SameProxyRetryDelaySet = true
	cfg.SameProxyRetryDelayMS = 0
	if proxyConnectionRetryDelay(cfg, 20) != 0 {
		t.Fatal("explicit zero override ignored")
	}
	cfg.SameProxyRetryDelayMS = 7
	if proxyConnectionRetryDelay(cfg, 3) != 28*time.Millisecond {
		t.Fatal("explicit new delay did not override legacy base")
	}
	if proxyConnectionRetryDelay(config.Config{}, 1) != time.Second {
		t.Fatal("Config{} lost legacy default")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := waitProxyConnectionRetry(ctx, cfg, 1); !errors.Is(err, context.Canceled) {
		t.Fatal("retry delay ignored cancellation")
	}
}

func TestNativeProxyRotationUsesCancelableLegacyBackoff(t *testing.T) {
	var mu sync.Mutex
	starts := []time.Time{}
	identities := []string{}
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		starts = append(starts, time.Now())
		identities = append(identities, r.Header.Get("Proxy-Authorization"))
		mu.Unlock()
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			conn.Close()
		}
	}))
	defer proxy.Close()
	cfg := config.Config{ChatGPTBase: "http://fixture.test", ProxyMaxAttempts: 3, SameProxyRetries: 0, ProxyConnectionRetryBaseMS: 12, RequestTimeoutMS: 500}
	selected := "http://user-session-fixture:synthetic@" + strings.TrimPrefix(proxy.URL, "http://")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	client, _, _, err := prepareNative(ctx, cfg, selected)
	if client != nil {
		client.Close()
	}
	if err == nil {
		t.Fatal("failed proxy unexpectedly succeeded")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(starts) != 3 {
		t.Fatal("proxy session attempt budget not preserved", len(starts))
	}
	if starts[1].Sub(starts[0]) < 11*time.Millisecond || starts[2].Sub(starts[1]) < 23*time.Millisecond {
		t.Fatal("rotation skipped exponential retry delay")
	}
	if identities[0] == identities[1] || identities[1] == identities[2] {
		t.Fatal("connection failures did not rotate proxy sessions")
	}
}

func TestNativeProxyBackoffCancellationAndFailureCeiling(t *testing.T) {
	for _, cancelDuringDelay := range []bool{true, false} {
		t.Run(map[bool]string{true: "cancel-delay", false: "twenty-failure-cap"}[cancelDuringDelay], func(t *testing.T) {
			var calls atomic.Int32
			first := make(chan struct{})
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if calls.Add(1) == 1 {
					close(first)
				}
				conn, _, err := w.(http.Hijacker).Hijack()
				if err == nil {
					conn.Close()
				}
			}))
			defer proxy.Close()
			cfg := config.Config{ChatGPTBase: "http://fixture.test", ProxyMaxAttempts: 100, SameProxyRetries: 3, ProxyConnectionRetryBaseMS: 15000, SameProxyRetryDelaySet: !cancelDuringDelay, SameProxyRetryDelayMS: 0, RequestTimeoutMS: 500}
			selected := "http://user-session-fixture:synthetic@" + strings.TrimPrefix(proxy.URL, "http://")
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() {
				c, _, _, err := prepareNative(ctx, cfg, selected)
				if c != nil {
					c.Close()
				}
				done <- err
			}()
			<-first
			if cancelDuringDelay {
				cancel()
			}
			select {
			case err := <-done:
				if cancelDuringDelay {
					if !errors.Is(err, context.Canceled) || calls.Load() != 1 {
						t.Fatal("cancelled backoff made another request", calls.Load(), err)
					}
				} else if err == nil || calls.Load() != 20 {
					t.Fatal("connection failure ceiling changed", calls.Load(), err)
				}
			case <-time.After(time.Second):
				t.Fatal("proxy retry did not stop promptly")
			}
		})
	}
}
