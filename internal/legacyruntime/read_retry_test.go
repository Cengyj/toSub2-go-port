package legacyruntime

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"tosub2-go-port/internal/transport"
)

func closeReadFixture(w http.ResponseWriter, t *testing.T) {
	t.Helper()
	conn, _, err := w.(http.Hijacker).Hijack()
	if err != nil {
		t.Error(err)
		return
	}
	_ = conn.Close()
}

func TestRuntimeResourceReadsRecoverWithoutNestedRetryBudget(t *testing.T) {
	for _, mode := range []string{"loader", "generated-get", "generated-head", "direct-do"} {
		t.Run(mode, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if calls.Add(1) <= 2 {
					closeReadFixture(w, t)
					return
				}
				_, _ = io.WriteString(w, "resource recovered")
			}))
			defer server.Close()
			events := []transport.ReadRetryEvent{}
			c := nativeClient(t, Options{ConnectionRetries: 3, OnRetry: func(event transport.ReadRetryEvent) { events = append(events, event) }})
			var err error
			switch mode {
			case "loader":
				var response *transport.Response
				response, err = c.runtimeGet(t.Context(), server.URL, nil, true, nil)
				if err == nil && response.Text() != "resource recovered" {
					t.Fatal("lost resource bytes")
				}
			case "generated-get", "generated-head":
				method := http.MethodGet
				if mode == "generated-head" {
					method = http.MethodHead
				}
				_, err = c.submitRuntimeRequest(t.Context(), server.URL, generatedRequest{ID: json.RawMessage("1"), Method: method, URL: server.URL}, false)
			case "direct-do":
				_, err = c.Do(t.Context(), transport.Request{URL: server.URL})
			}
			if mode == "direct-do" {
				if err == nil || calls.Load() != 1 || len(events) != 0 {
					t.Fatal("single-attempt transport gained nested retry", calls.Load(), err)
				}
			} else if err != nil || calls.Load() != 3 || len(events) != 2 {
				t.Fatal("script resource did not recover finite retries", calls.Load(), len(events), err)
			}
		})
	}
}

func TestRuntimeReadExhaustionAndWritesRemainSingleAttempt(t *testing.T) {
	for _, mode := range []string{"loader", "generated-post", "single-attempt-get", "zero-budget"} {
		t.Run(mode, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				_, _ = io.Copy(io.Discard, r.Body)
				closeReadFixture(w, t)
			}))
			defer server.Close()
			events := []transport.ReadRetryEvent{}
			budget := 3
			if mode == "zero-budget" {
				budget = 0
			}
			c := nativeClient(t, Options{ConnectionRetries: budget, OnRetry: func(event transport.ReadRetryEvent) { events = append(events, event) }})
			var err error
			switch mode {
			case "generated-post":
				_, err = c.submitRuntimeRequest(t.Context(), server.URL, generatedRequest{ID: json.RawMessage("1"), Method: http.MethodPost, URL: server.URL, Body: "fixture-private-body"}, true)
			case "single-attempt-get":
				_, err = c.runtimeRequest(t.Context(), transport.Request{Method: http.MethodGet, URL: server.URL, SingleAttempt: true})
			default:
				_, err = c.runtimeGet(t.Context(), server.URL, nil, true, nil)
			}
			if err == nil {
				t.Fatal("closed resource unexpectedly succeeded")
			}
			if mode == "loader" {
				if calls.Load() != 4 || len(events) != 4 || !events[3].Exhausted || !strings.Contains(err.Error(), "已重试 3 次") {
					t.Fatal("retry budget changed or nested", calls.Load(), len(events), err)
				}
			} else if calls.Load() != 1 || len(events) != 0 {
				t.Fatal("write/security/zero-budget read was replayed", calls.Load(), len(events), err)
			}
		})
	}
}

type interruptedRuntimeTLSListener struct {
	net.Listener
	connections atomic.Int32
	failures    int32
}

func (l *interruptedRuntimeTLSListener) Accept() (net.Conn, error) {
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		if l.connections.Add(1) > l.failures {
			return conn, nil
		}
		// Interrupt a real ClientHello before an HTTP request can be sent.
		_ = conn.SetDeadline(time.Now().Add(time.Second))
		_, _ = io.ReadFull(conn, make([]byte, 5))
		_ = conn.Close()
	}
}

func TestGeneratedWritesRetryOnlyBeforeHTTPDispatch(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		for _, exhaust := range []bool{false, true} {
			name := method + "/recover"
			failures := int32(2)
			if exhaust {
				name = method + "/exhaust"
				failures = 4
			}
			t.Run(name, func(t *testing.T) {
				var writes atomic.Int32
				server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					writes.Add(1)
					body, err := io.ReadAll(r.Body)
					if err != nil || string(body) != "fixture-script-body" || r.Method != method {
						t.Error("script retry changed HTTP mutation")
					}
					_, _ = io.WriteString(w, "recovered")
				}))
				listener := &interruptedRuntimeTLSListener{Listener: server.Listener, failures: failures}
				server.Listener = listener
				server.StartTLS()
				defer server.Close()
				roots := x509.NewCertPool()
				roots.AddCert(server.Certificate())
				events := []transport.ReadRetryEvent{}
				c, err := NewWithRootCAs(t.Context(), Options{ConnectionRetries: 3, OnRetry: func(e transport.ReadRetryEvent) { events = append(events, e) }}, roots)
				if err != nil {
					t.Fatal(err)
				}
				defer c.Close()
				result, err := c.submitRuntimeRequest(t.Context(), server.URL, generatedRequest{ID: json.RawMessage("1"), Method: method, URL: server.URL, Body: "fixture-script-body"}, true)
				if exhaust {
					if err == nil || !transport.IsRequestDefinitelyUnsent(err) || writes.Load() != 0 || listener.connections.Load() != 4 || len(events) != 4 || !events[3].Exhausted {
						t.Fatalf("unsent script exhaustion lost evidence writes=%d attempts=%d events=%d err=%v", writes.Load(), listener.connections.Load(), len(events), err)
					}
				} else if err != nil || result["body"] != "recovered" || writes.Load() != 1 || listener.connections.Load() != 3 || len(events) != 2 {
					t.Fatalf("unsent script did not recover once writes=%d attempts=%d events=%d err=%v", writes.Load(), listener.connections.Load(), len(events), err)
				}
			})
		}
	}
}

func TestGeneratedSentWriteNeverReplaysAfterLostResponse(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		for _, lostBody := range []bool{false, true} {
			name := method + "/headers"
			if lostBody {
				name = method + "/body"
			}
			t.Run(name, func(t *testing.T) {
				var writes atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					writes.Add(1)
					_, _ = io.Copy(io.Discard, r.Body)
					conn, buffer, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					defer conn.Close()
					if lostBody {
						_, _ = buffer.WriteString("HTTP/1.1 200 OK\r\nContent-Length: 50\r\n\r\npartial")
						_ = buffer.Flush()
					}
				}))
				defer server.Close()
				c := nativeClient(t, Options{ConnectionRetries: 3, OnRetry: func(transport.ReadRetryEvent) { t.Error("sent script mutation was retried") }})
				_, err := c.submitRuntimeRequest(t.Context(), server.URL, generatedRequest{ID: json.RawMessage("1"), Method: method, URL: server.URL, Body: "fixture-script-body"}, false)
				if err == nil || writes.Load() != 1 || transport.IsRequestDefinitelyUnsent(err) {
					t.Fatalf("sent script write repeated or marked unsent writes=%d err=%v", writes.Load(), err)
				}
			})
		}
	}
}

func TestRuntimeReadTimeoutRecoversAndParentBudgetIsRetained(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) <= 2 {
			<-r.Context().Done()
			return
		}
		_, _ = io.WriteString(w, "recovered")
	}))
	defer server.Close()
	c := nativeClient(t, Options{ConnectionRetries: 3})
	response, err := c.runtimeRequest(t.Context(), transport.Request{URL: server.URL, Timeout: 25 * time.Millisecond})
	if err != nil || calls.Load() != 3 || response.Text() != "recovered" {
		t.Fatal("per-request timeout killed parent retry budget", calls.Load(), err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	prior := calls.Load()
	_, err = c.runtimeRequest(ctx, transport.Request{URL: server.URL})
	if !errors.Is(err, context.Canceled) || calls.Load() != prior {
		t.Fatal("canceled parent reached HTTP")
	}
	var stalled atomic.Int32
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stalled.Add(1)
		<-r.Context().Done()
	}))
	defer slow.Close()
	ctx, cancel = context.WithTimeout(t.Context(), 25*time.Millisecond)
	defer cancel()
	_, err = c.runtimeRequest(ctx, transport.Request{URL: slow.URL, Timeout: time.Second})
	if !errors.Is(err, context.DeadlineExceeded) || stalled.Load() != 1 || strings.Contains(err.Error(), "已重试") {
		t.Fatal("parent budget was extended or reported as retry exhaustion", stalled.Load(), err)
	}
}

func TestRuntimeReadStopsOnCancellationAndPermanentHTTPFailure(t *testing.T) {
	for _, mode := range []string{"cancel-delay", "owner-cancel", "HTTP-403"} {
		t.Run(mode, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if mode == "HTTP-403" {
					w.WriteHeader(http.StatusForbidden)
					return
				}
				closeReadFixture(w, t)
			}))
			defer server.Close()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var c *Client
			c = nativeClient(t, Options{ConnectionRetries: 3, RetryDelay: time.Minute, OnRetry: func(event transport.ReadRetryEvent) {
				if mode == "HTTP-403" || event.Exhausted {
					t.Fatal("unexpected retry event")
				}
				if mode == "owner-cancel" {
					c.cancel()
				} else {
					cancel()
				}
			}})
			started := time.Now()
			response, err := c.runtimeGet(ctx, server.URL, nil, true, nil)
			if calls.Load() != 1 || time.Since(started) > time.Second {
				t.Fatal("permanent response/cancellation retried or blocked")
			}
			if mode == "HTTP-403" {
				if err != nil || response.StatusCode != 403 {
					t.Fatal("HTTP response lost")
				}
			} else if !errors.Is(err, context.Canceled) {
				t.Fatal("cancellation lost", err)
			}
		})
	}
}

func TestRuntimeReadRejectsUntrustedTLSWithoutRetry(t *testing.T) {
	var handshakes atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("untrusted request reached HTTP")
	}))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			handshakes.Add(1)
		}
	}
	server.StartTLS()
	defer server.Close()
	c := nativeClient(t, Options{ConnectionRetries: 3, OnRetry: func(transport.ReadRetryEvent) { t.Fatal("certificate failure retried") }})
	_, err := c.runtimeGet(t.Context(), server.URL+"/?private=canary", nil, true, nil)
	if err == nil || handshakes.Load() != 1 || !strings.Contains(err.Error(), "certificate validation failed") || strings.Contains(err.Error(), "canary") {
		t.Fatal("untrusted certificate retried or diagnosis disclosed request", handshakes.Load(), err)
	}
}
