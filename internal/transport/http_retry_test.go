package transport

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

type interruptedHandshakeListener struct {
	net.Listener
	connections atomic.Int32
	failures    int32
}

func (l *interruptedHandshakeListener) Accept() (net.Conn, error) {
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		if l.connections.Add(1) > l.failures {
			return conn, nil
		}
		// Receive part of a real TLS ClientHello, then interrupt its handshake
		// before the transport has an HTTP-capable connection.
		_ = conn.SetDeadline(time.Now().Add(time.Second))
		header := make([]byte, 5)
		_, _ = io.ReadFull(conn, header)
		_ = conn.Close()
	}
}

func TestNetworkRetryRealInterruptedTLSHandshake(t *testing.T) {
	for _, kind := range []string{"native", "fingerprint", "standard-adapter"} {
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			t.Run(kind+"/"+method, func(t *testing.T) {
				var writes atomic.Int32
				server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					writes.Add(1)
					_, _ = io.Copy(io.Discard, r.Body)
					_, _ = w.Write([]byte("recovered"))
				}))
				interrupted := &interruptedHandshakeListener{Listener: server.Listener, failures: 2}
				server.Listener = interrupted
				server.StartTLS()
				defer server.Close()
				roots := x509.NewCertPool()
				roots.AddCert(server.Certificate())
				request := Request{Method: method, URL: server.URL, Body: []byte("{}"), Timeout: time.Second, RetryBeforeSend: method == http.MethodPost}
				var response *Response
				var err error
				events := 0
				onRetry := func(e ReadRetryEvent) {
					events++
					if e.Exhausted {
						t.Error("recovery exhausted budget")
					}
				}
				if kind == "standard-adapter" {
					response, err = DoHTTPWithRetry(t.Context(), server.Client(), request, 1024, 3, 0, onRetry)
				} else {
					client := singleAttemptTestClient(t, kind, roots)
					response, err = DoReadWithRetry(t.Context(), request, 3, 0, client.Do, onRetry)
				}
				if err != nil || response.Text() != "recovered" || interrupted.connections.Load() != 3 || writes.Load() != 1 || events != 2 {
					t.Fatalf("connections=%d HTTPwrites=%d retryEvents=%d response=%v err=%v", interrupted.connections.Load(), writes.Load(), events, response, err)
				}
			})
		}
	}
}

func TestNetworkRetryDoesNotReplaySentWrites(t *testing.T) {
	for _, kind := range []string{"native", "fingerprint", "standard-adapter"} {
		for _, failure := range []string{"headers-lost", "body-lost"} {
			t.Run(kind+"/"+failure, func(t *testing.T) {
				var calls atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					_, _ = io.Copy(io.Discard, r.Body)
					conn, rw, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					defer conn.Close()
					if failure == "body-lost" {
						_, _ = rw.WriteString("HTTP/1.1 200 OK\r\nContent-Length: 20\r\n\r\nshort")
						_ = rw.Flush()
					}
				}))
				defer server.Close()
				request := Request{Method: http.MethodPost, URL: server.URL, Body: []byte("{}"), Timeout: time.Second, RetryBeforeSend: true}
				var err error
				noRetry := func(ReadRetryEvent) { t.Fatal("sent write retry") }
				if kind == "standard-adapter" {
					_, err = DoHTTPWithRetry(t.Context(), server.Client(), request, 1024, 3, 0, noRetry)
				} else {
					client := singleAttemptTestClient(t, kind, nil)
					_, err = DoReadWithRetry(t.Context(), request, 3, 0, client.Do, noRetry)
				}
				if err == nil || calls.Load() != 1 || IsRequestDefinitelyUnsent(err) {
					t.Fatalf("sent write repeated or unsent evidence incorrect calls=%d err=%v", calls.Load(), err)
				}
			})
		}
	}
}

func TestNetworkRetryDoesNotRepeatPermanentTLSRejection(t *testing.T) {
	for _, kind := range []string{"native", "fingerprint", "standard-adapter"} {
		t.Run(kind, func(t *testing.T) {
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("TLS rejected request reached HTTP") }))
			listener := &interruptedHandshakeListener{Listener: server.Listener}
			server.Listener = listener
			server.TLS = &tls.Config{MinVersion: tls.VersionTLS10, MaxVersion: tls.VersionTLS10}
			server.StartTLS()
			defer server.Close()
			roots := x509.NewCertPool()
			roots.AddCert(server.Certificate())
			request := Request{Method: http.MethodPost, URL: server.URL, RetryBeforeSend: true, Timeout: time.Second}
			var err error
			noRetry := func(ReadRetryEvent) { t.Fatal("permanent TLS rejection retried") }
			if kind == "standard-adapter" {
				_, err = DoHTTPWithRetry(t.Context(), server.Client(), request, 1024, 3, 0, noRetry)
			} else {
				client := singleAttemptTestClient(t, kind, roots)
				_, err = DoReadWithRetry(t.Context(), request, 3, 0, client.Do, noRetry)
			}
			if err == nil || IsRetryableReadError(err) || listener.connections.Load() != 1 {
				t.Fatalf("TLS rejection repeated connections=%d err=%v", listener.connections.Load(), err)
			}
		})
	}
}

func TestProtocolTransportsDoNotRetryHTTPRejectionWithTruncatedBody(t *testing.T) {
	for _, kind := range []string{"native", "fingerprint"} {
		for _, status := range []int{403, 503} {
			t.Run(fmt.Sprintf("%s/%d", kind, status), func(t *testing.T) {
				var calls atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					w.Header().Set("Content-Length", "100")
					w.WriteHeader(status)
					_, _ = w.Write([]byte("partial error"))
				}))
				defer server.Close()
				client := singleAttemptTestClient(t, kind, nil)
				response, err := DoReadWithRetry(t.Context(), Request{URL: server.URL, Timeout: time.Second}, 3, 0, client.Do, func(ReadRetryEvent) { t.Fatal("received HTTP rejection retried") })
				if err == nil || response == nil || response.StatusCode != status || calls.Load() != 1 {
					t.Fatalf("HTTP status lost or repeated calls=%d response=%v err=%v", calls.Load(), response, err)
				}
			})
		}
	}
}

func TestHTTPAdapterParentCancellationBeforeConnectionRetainsUnsentProof(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var dials atomic.Int32
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
		dials.Add(1)
		cancel()
		<-ctx.Done()
		return nil, ctx.Err()
	}
	client := &http.Client{Transport: tr}
	_, err := DoHTTPWithRetry(ctx, client, Request{Method: http.MethodPost, URL: "http://fixture.invalid", RetryBeforeSend: true, Timeout: time.Second}, 1024, 3, 0, func(ReadRetryEvent) { t.Error("cancellation retried") })
	if !errors.Is(err, context.Canceled) || !IsRequestDefinitelyUnsent(err) || dials.Load() != 1 {
		t.Fatalf("parent cancellation lost evidence dials=%d err=%v", dials.Load(), err)
	}
}

func TestHTTPAdapterRetriesReadBodyAndBeforeSendDialFailures(t *testing.T) {
	for _, scenario := range []string{"body-GET", "dial-POST"} {
		t.Run(scenario, func(t *testing.T) {
			var calls, dials atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n := calls.Add(1)
				if scenario == "body-GET" && n <= 2 {
					conn, rw, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					_, _ = rw.WriteString("HTTP/1.1 200 OK\r\nContent-Length: 20\r\n\r\nshort")
					_ = rw.Flush()
					_ = conn.Close()
					return
				}
				_, _ = w.Write([]byte("recovered"))
			}))
			defer server.Close()
			client := server.Client()
			request := Request{Method: http.MethodGet, URL: server.URL, Timeout: time.Second}
			if scenario == "dial-POST" {
				tr := http.DefaultTransport.(*http.Transport).Clone()
				tr.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
					if dials.Add(1) <= 2 {
						return nil, &net.OpError{Op: "dial", Net: network, Err: syscall.ECONNREFUSED}
					}
					return (&net.Dialer{}).DialContext(ctx, network, address)
				}
				client = &http.Client{Transport: tr}
				defer client.CloseIdleConnections()
				request.Method = http.MethodPost
				request.RetryBeforeSend = true
			}
			response, err := DoHTTPWithRetry(t.Context(), client, request, 1024, 3, 0, nil)
			wantCalls := int32(3)
			if scenario == "dial-POST" {
				wantCalls = 1
			}
			if err != nil || response.Text() != "recovered" || calls.Load() != wantCalls {
				t.Fatalf("calls=%d dials=%d err=%v", calls.Load(), dials.Load(), err)
			}
		})
	}
}

func TestHTTPAdapterStopsAtPermanentFailuresAndHTTPHeaders(t *testing.T) {
	for _, scenario := range []string{"HTTP-403", "HTTP-503", "HTTP-redirect", "headers-only", "oversize", "certificate"} {
		t.Run(scenario, func(t *testing.T) {
			var calls atomic.Int32
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				switch scenario {
				case "HTTP-403":
					w.WriteHeader(403)
				case "HTTP-503":
					w.WriteHeader(503)
				case "HTTP-redirect":
					w.Header().Set("Location", "/redirected")
					w.WriteHeader(307)
				default:
					w.WriteHeader(200)
				}
				if scenario == "oversize" {
					_, _ = w.Write([]byte("too many bytes"))
					return
				}
				w.(http.Flusher).Flush()
				<-r.Context().Done()
			})
			var server *httptest.Server
			if scenario == "certificate" {
				server = httptest.NewTLSServer(handler)
			} else {
				server = httptest.NewServer(handler)
			}
			defer server.Close()
			limit := int64(5)
			if scenario == "headers-only" {
				limit = 0
			}
			started := time.Now()
			response, err := DoHTTPWithRetry(t.Context(), nil, Request{URL: server.URL, Timeout: time.Second}, limit, 3, 0, func(ReadRetryEvent) { t.Fatal("permanent failure/HTTP response retried") })
			if time.Since(started) > 500*time.Millisecond {
				t.Fatal("unneeded body was read to timeout")
			}
			if scenario == "certificate" {
				if err == nil || IsRetryableReadError(err) || calls.Load() != 0 {
					t.Fatalf("certificate rejection did not stop: %v", err)
				}
				return
			}
			if calls.Load() != 1 {
				t.Fatal("HTTP status was retried")
			}
			if scenario == "oversize" {
				if err == nil || IsRetryableReadError(err) {
					t.Fatal("response size limit not permanent", err)
				}
				return
			}
			if err != nil || response == nil || len(response.Body) != 0 {
				t.Fatalf("header response missing: %v", err)
			}
		})
	}
}

type retryFixtureRoundTripper func(*http.Request) (*http.Response, error)

func (f retryFixtureRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type retryFixtureBody struct{ err error }

func (b retryFixtureBody) Read([]byte) (int, error) { return 0, b.err }
func (retryFixtureBody) Close() error               { return nil }

func TestHTTPAdapterCustomTransportUnknownErrorsDoNotBorrowNetworkRetry(t *testing.T) {
	for _, scenario := range []string{"unknown-body", "EOF-body", "unknown-request", "POST-no-trace"} {
		t.Run(scenario, func(t *testing.T) {
			calls := 0
			client := &http.Client{Transport: retryFixtureRoundTripper(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.GetBody != nil || !r.Close {
					t.Error("hidden replay was not disabled")
				}
				if scenario == "POST-no-trace" {
					return nil, io.EOF
				}
				if scenario == "unknown-request" {
					return nil, errors.New("private request error")
				}
				failure := errors.New("private body error")
				if scenario == "EOF-body" {
					failure = io.ErrUnexpectedEOF
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: retryFixtureBody{err: failure}, Request: r}, nil
			})}
			request := Request{URL: "https://fixture.invalid/?secret=canary"}
			if scenario == "POST-no-trace" {
				request.Method = http.MethodPost
				request.RetryBeforeSend = true
			}
			_, err := DoHTTPWithRetry(t.Context(), client, request, 1024, 3, 0, nil)
			want := 1
			if scenario == "EOF-body" {
				want = 4
			}
			if err == nil || calls != want || strings.Contains(fmt.Sprintf("%+v", err), "private") || IsRequestDefinitelyUnsent(err) {
				t.Fatalf("calls=%d err=%v", calls, err)
			}
		})
	}
}
