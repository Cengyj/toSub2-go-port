package legacyruntime

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"tosub2-go-port/internal/transport"
)

func TestNativeCaptureResponseRetainsFailureReceipt(t *testing.T) {
	for _, scenario := range []string{"truncated", "too-large", "header-receipt", "canceled"} {
		t.Run(scenario, func(t *testing.T) {
			const prefix = `{"seed":"synthetic-private-seed"`
			var calls atomic.Int32
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Set-Cookie", "rotated=latest; Path=/; HttpOnly")
				if scenario == "truncated" || scenario == "canceled" {
					w.Header().Set("Content-Length", "4096")
				}
				w.WriteHeader(http.StatusCreated)
				_, _ = io.WriteString(w, prefix)
				if scenario == "too-large" {
					_, _ = io.WriteString(w, strings.Repeat("x", 4096))
				}
				if scenario == "canceled" {
					w.(http.Flusher).Flush()
					<-r.Context().Done()
				}
			}))
			defer server.Close()
			client := nativeClient(t, Options{})
			jar := transport.NewCookieJar()
			client.SetJar(jar)
			cause := errors.New("synthetic-private-receipt-recording-detail")
			request := transport.Request{Method: http.MethodPost, URL: server.URL, SingleAttempt: true, CaptureResponse: true, MaxResponseBytes: 64, Timeout: time.Second}
			if scenario == "header-receipt" {
				request.OnResponseHeaders = func(*transport.Response) error { return cause }
			}
			if scenario == "canceled" {
				request.OnResponseHeaders = func(*transport.Response) error {
					// Cancel after the body reader has had a chance to retain its prefix.
					time.AfterFunc(50*time.Millisecond, cancel)
					return nil
				}
			}
			response, err := client.Do(ctx, request)
			if err == nil || response == nil || response.StatusCode != http.StatusCreated || response.Header.Get("Content-Type") != "application/json" || len(response.Header.Values("Set-Cookie")) != 1 || calls.Load() != 1 {
				t.Fatalf("failure receipt lost or mutation replayed: response=%t calls=%d err=%v", response != nil, calls.Load(), err)
			}
			if len(response.Body) > 64 || !strings.HasPrefix(string(response.Body), prefix) {
				t.Fatal("bounded private response prefix lost")
			}
			wantComplete := scenario == "header-receipt"
			if response.BodyComplete != wantComplete || jar.Value("rotated", server.URL) != "latest" {
				t.Fatal("failure changed body completeness or discarded response cookies")
			}
			if scenario == "too-large" && (!errors.Is(err, transport.ErrResponseTooLarge) || len(response.Body) != 64) {
				t.Fatal("response size boundary was not enforced")
			}
			if scenario == "canceled" && !errors.Is(err, context.Canceled) {
				t.Fatal("explicit cancellation was ignored")
			}
		})
	}
}
