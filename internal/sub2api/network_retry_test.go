package sub2api

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"tosub2-go-port/internal/transport"
)

func TestSub2APIReadsRecoverButReachedWritesAreNeverReplayed(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodPost} {
		t.Run(method, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != method || r.Header.Get("x-api-key") != "fixture-secret" {
					t.Error("request method or authentication changed")
				}
				if calls.Add(1) <= 2 {
					conn, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					_ = conn.Close()
					return
				}
				_, _ = fmt.Fprint(w, `{"data":[]}`)
			}))
			defer server.Close()
			client := New(Config{BaseURL: server.URL, APIKey: "fixture-secret"})
			var out any
			err := client.request(t.Context(), method, "api/v1/admin/accounts", map[string]any{"credentials": "fixture"}, &out)
			if method == http.MethodGet {
				if err != nil || calls.Load() != 3 {
					t.Fatal("safe Sub2API query did not recover", err)
				}
			} else if err == nil || calls.Load() != 1 || strings.Contains(err.Error(), "fixture-secret") {
				t.Fatal("Sub2API mutation was replayed or leaked credentials", err)
			}
		})
	}
}

func TestSub2APIMutationRetriesOnlyProvenUnsentConnectionFailure(t *testing.T) {
	var received atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { received.Add(1); _, _ = fmt.Fprint(w, `{"ok":true}`) }))
	defer server.Close()
	var dials atomic.Int32
	client := New(Config{BaseURL: server.URL, APIKey: "fixture-secret"})
	client.http.Transport = &http.Transport{DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		if dials.Add(1) <= 2 {
			return nil, &net.OpError{Op: "dial", Net: "tcp", Err: transportFixtureTimeout{}}
		}
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}}
	if _, err := client.Update(t.Context(), "1", map[string]any{"credentials": "fixture"}); err != nil || received.Load() != 1 || dials.Load() != 3 {
		t.Fatal("unsent mutation did not retry safely", err)
	}
}

type transportFixtureTimeout struct{}

func (transportFixtureTimeout) Error() string   { return "synthetic dial timeout" }
func (transportFixtureTimeout) Timeout() bool   { return true }
func (transportFixtureTimeout) Temporary() bool { return true }

func TestSub2APIPermanentHTTPRejectionIsNotRetried(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls.Add(1); w.WriteHeader(http.StatusUnauthorized) }))
	defer server.Close()
	client := New(Config{BaseURL: server.URL, APIKey: "fixture-secret", OnRetry: func(transport.ReadRetryEvent) { t.Error("HTTP rejection triggered network retry") }})
	_, err := client.Groups(t.Context())
	if !IsRemoteStatus(err, http.StatusUnauthorized) || calls.Load() != 1 {
		t.Fatal("HTTP rejection was replayed", err)
	}
}

func TestSub2APINetworkExhaustionPreservesSafeCount(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		_ = conn.Close()
	}))
	defer server.Close()
	var events []transport.ReadRetryEvent
	client := New(Config{BaseURL: server.URL, APIKey: "fixture-secret", OnRetry: func(event transport.ReadRetryEvent) { events = append(events, event) }})
	_, err := client.Groups(t.Context())
	if err == nil || calls.Load() != 4 || len(events) != 4 || !events[3].Exhausted || !strings.Contains(SafeError(err), "已重试 3 次") || strings.Contains(err.Error(), "fixture-secret") {
		t.Fatalf("incorrect safe network exhaustion: requests=%d events=%d error=%v", calls.Load(), len(events), err)
	}
}

func TestSub2APIConfiguredTimeoutHasIndependentRetryBudget(t *testing.T) {
	var received atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		received.Add(1)
		_, _ = fmt.Fprint(w, `{"data":[]}`)
	}))
	defer server.Close()
	var dials, retries atomic.Int32
	client := New(Config{BaseURL: server.URL, APIKey: "fixture-secret", Timeout: 20 * time.Millisecond, OnRetry: func(event transport.ReadRetryEvent) {
		if !event.Exhausted {
			retries.Add(1)
		}
	}})
	client.http.Transport = &http.Transport{DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		if dials.Add(1) <= 2 {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}}
	if _, err := client.Groups(t.Context()); err != nil || dials.Load() != 3 || received.Load() != 1 || retries.Load() != 2 {
		t.Fatalf("attempt timeout consumed total retry budget: dials=%d received=%d retries=%d error=%v", dials.Load(), received.Load(), retries.Load(), err)
	}
}

func TestSub2APICallerCancellationPreservesUnsentEvidence(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		t.Run(fmt.Sprintf("deadline=%t", deadline), func(t *testing.T) {
			ctx := context.Background()
			var cancel context.CancelFunc
			want := context.Canceled
			if deadline {
				ctx, cancel = context.WithTimeout(ctx, 30*time.Millisecond)
				want = context.DeadlineExceeded
			} else {
				ctx, cancel = context.WithCancel(ctx)
			}
			defer cancel()
			dialStarted := make(chan struct{})
			client := New(Config{BaseURL: "http://fixture.invalid", APIKey: "fixture-secret", OnRetry: func(transport.ReadRetryEvent) {
				t.Error("caller cancellation triggered a retry")
			}})
			client.http.Transport = &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				close(dialStarted)
				<-ctx.Done()
				return nil, ctx.Err()
			}}
			if !deadline {
				go func() { <-dialStarted; cancel() }()
			}
			_, err := client.Update(ctx, "1", map[string]any{"credentials": "fixture"})
			if !errors.Is(err, want) || !transport.IsRequestDefinitelyUnsent(err) || strings.Contains(err.Error(), "fixture-secret") {
				t.Fatalf("caller cancellation lost safe transport evidence: %v", err)
			}
		})
	}
}
