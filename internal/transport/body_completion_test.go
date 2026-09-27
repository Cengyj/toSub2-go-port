package transport

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	tlsclient "github.com/bogdanfinn/tls-client"
)

// A response may finish cleanly after request cancellation. In particular, a
// TLS peer can receive close_notify and finish its chunked response while the
// local transport is closing the connection. io.ReadAll then returns nil even
// though the request context has already ended. Keep this ordering deterministic
// alongside the real TLS fixture in request_lifetime_test.go.
type eofAfterCancellationBody struct {
	ctx    context.Context
	cancel context.CancelFunc
	closed atomic.Bool
}

func (b *eofAfterCancellationBody) Read(p []byte) (int, error) {
	if b.cancel != nil {
		b.cancel()
	}
	<-b.ctx.Done()
	return copy(p, "last response bytes"), io.EOF
}
func (b *eofAfterCancellationBody) Close() error { b.closed.Store(true); return nil }

type completionRoundTripper func(*http.Request) (*http.Response, error)

func (f completionRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type completionFingerprintClient struct {
	tlsclient.HttpClient
	body *eofAfterCancellationBody
}

func (c *completionFingerprintClient) Do(r *fhttp.Request) (*fhttp.Response, error) {
	c.body.ctx = r.Context()
	return &fhttp.Response{StatusCode: 200, Header: fhttp.Header{"Set-Cookie": {"accepted=synthetic; Path=/"}}, Body: c.body, Request: r}, nil
}

func TestTransportCancellationWinsOverCleanBodyEOF(t *testing.T) {
	for _, kind := range []string{"native", "fingerprint"} {
		for _, cause := range []string{"cancel", "deadline"} {
			t.Run(kind+"/"+cause, func(t *testing.T) {
				parent, cancel := context.WithCancel(context.Background())
				defer cancel()
				body := &eofAfterCancellationBody{}
				want := context.DeadlineExceeded
				if cause == "cancel" {
					body.cancel = cancel
					want = context.Canceled
				}
				jar := NewCookieJar()
				var client lifetimeDoer
				if kind == "native" {
					c, err := NewNativeClient("", jar)
					if err != nil {
						t.Fatal(err)
					}
					c.HTTP.Transport = completionRoundTripper(func(r *http.Request) (*http.Response, error) {
						body.ctx = r.Context()
						return &http.Response{StatusCode: 200, Header: http.Header{"Set-Cookie": {"accepted=synthetic; Path=/"}}, Body: body, Request: r}, nil
					})
					client = c
				} else {
					c, err := NewFingerprintClient("chrome_133", "", jar)
					if err != nil {
						t.Fatal(err)
					}
					c.client = &completionFingerprintClient{HttpClient: c.client, body: body}
					client = c
				}
				defer client.Close()
				response, err := client.Do(parent, Request{URL: "https://authorized-fixture.invalid/response", Timeout: 10 * time.Millisecond})
				if !errors.Is(err, want) || response != nil {
					t.Fatalf("EOF after %s must not report success: response=%v error=%v", cause, response != nil, err)
				}
				if !body.closed.Load() {
					t.Fatal("canceled response body was not closed")
				}
				if jar.HeaderFor("https://authorized-fixture.invalid") != "accepted=synthetic" {
					t.Fatal("cancellation discarded already accepted response cookies")
				}
			})
		}
	}
}
