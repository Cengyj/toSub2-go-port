package transport

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
)

func TestStrictRetryReviewWrappedDNSKeepsJoinedFailurePolicy(t *testing.T) {
	timeout := &net.DNSError{Err: "synthetic timeout", IsTimeout: true}
	temporary := &net.DNSError{Err: "synthetic temporary", IsTemporary: true}
	permanent := &net.DNSError{Err: "synthetic missing host", IsNotFound: true}
	for _, wrapper := range []struct {
		name string
		wrap func(error) error
	}{
		{"direct", func(err error) error { return err }},
		{"fmt", func(err error) error { return fmt.Errorf("synthetic wrapper: %w", err) }},
		{"url", func(err error) error {
			return &url.Error{Op: "Get", URL: "http://synthetic.example.test/", Err: err}
		}},
		{"dial", func(err error) error { return &net.OpError{Op: "dial", Net: "tcp", Err: err} }},
	} {
		for _, failure := range []struct {
			name string
			err  error
			want bool
		}{
			{"timeout", timeout, true},
			{"temporary", temporary, true},
			{"all-transient", errors.Join(timeout, temporary, io.ErrUnexpectedEOF), true},
			{"unknown-storage", errors.Join(timeout, errors.New("synthetic unknown storage failure")), false},
			{"permanent-dns", errors.Join(timeout, permanent), false},
			{"certificate", errors.Join(timeout, x509.UnknownAuthorityError{}), false},
			{"canceled", errors.Join(timeout, context.Canceled), false},
		} {
			t.Run(wrapper.name+"/"+failure.name, func(t *testing.T) {
				err := wrapper.wrap(failure.err)
				if got := IsRetryableReadError(err); got != failure.want {
					t.Errorf("wrapped DNS bypassed joined failure policy: retryable=%t want=%t", got, failure.want)
				}
				calls := 0
				_, _ = DoReadWithRetry(t.Context(), Request{Method: http.MethodGet}, 1, 0, func(context.Context, Request) (*Response, error) {
					calls++
					return nil, err
				}, nil)
				wantCalls := 1
				if failure.want {
					wantCalls = 2
				}
				if calls != wantCalls {
					t.Fatalf("wrong retry count for wrapped DNS: calls=%d want=%d", calls, wantCalls)
				}
			})
		}
	}
}

func TestRetryDialFailureCannotEraseEarlierHTTPAttempt(t *testing.T) {
	var calls, dials atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Length", "100")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	tr.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if dials.Add(1) > 1 {
			return nil, context.DeadlineExceeded
		}
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}
	raw := NewClient(nil)
	raw.HTTP.Transport = tr
	defer raw.Close()
	_, err := DoReadWithRetry(t.Context(), Request{Method: http.MethodGet, URL: server.URL}, 1, 0, raw.Do, nil)
	if calls.Load() != 1 || dials.Load() != 2 || !errors.Is(err, context.DeadlineExceeded) || IsRequestDefinitelyUnsent(err) {
		t.Fatalf("later dial failure erased earlier HTTP evidence: calls=%d dials=%d unsent=%t err=%v", calls.Load(), dials.Load(), IsRequestDefinitelyUnsent(err), err)
	}
}

func TestRetryCancellationAfterDialFailureKeepsUnsentProof(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var dials atomic.Int32
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	tr.DialContext = func(context.Context, string, string) (net.Conn, error) {
		dials.Add(1)
		return nil, context.DeadlineExceeded
	}
	raw := NewClient(nil)
	raw.HTTP.Transport = tr
	defer raw.Close()
	_, err := DoReadWithRetry(ctx, Request{Method: http.MethodGet, URL: "http://synthetic.example.test/"}, 1, 0, raw.Do, func(ReadRetryEvent) { cancel() })
	if dials.Load() != 1 || !errors.Is(err, context.Canceled) || !IsRequestDefinitelyUnsent(err) {
		t.Fatalf("cancellation after dial failure lost unsent evidence: dials=%d unsent=%t err=%v", dials.Load(), IsRequestDefinitelyUnsent(err), err)
	}
}
