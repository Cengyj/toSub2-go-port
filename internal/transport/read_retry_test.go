package transport

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestReadRetryTransientClassification(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"timeout", context.DeadlineExceeded, true},
		{"canceled", context.Canceled, false},
		{"EOF", io.EOF, true},
		{"truncated", io.ErrUnexpectedEOF, true},
		{"reset", fmt.Errorf("private: %w", syscall.ECONNRESET), true},
		{"network-timeout", &net.DNSError{Err: "private", Name: "private", IsTimeout: true}, true},
		{"temporary-DNS", &net.DNSError{Err: "private", IsTemporary: true}, true},
		{"missing-DNS", &net.DNSError{Err: "private", IsNotFound: true}, false},
		{"permanent-DNS", &net.DNSError{Err: "private"}, false},
		{"unknown", errors.New("request timed out private"), false},
		{"certificate", x509.UnknownAuthorityError{}, false},
		{"certificate-and-timeout", errors.Join(context.DeadlineExceeded, x509.UnknownAuthorityError{}), false},
		{"typed-timeout-and-certificate", errors.Join(&TransportError{kind: failureTimeout}, &TransportError{kind: failureCertificate}), false},
		{"timeout-and-unknown", errors.Join(context.DeadlineExceeded, errors.New("private unknown failure")), false},
		{"TLS-version", errors.New("remote error: tls: protocol version not supported"), false},
		{"TLS-handshake-interruption", classifyTransportError(io.ErrUnexpectedEOF, phaseProxyTLS, false), true},
		{"nil", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsRetryableReadError(tc.err); got != tc.want {
				t.Fatalf("retryable=%t want=%t", got, tc.want)
			}
		})
	}
	for kind := failureRequest; kind <= failureBody; kind++ {
		err := &TransportError{kind: kind}
		want := kind == failureTimeout || kind == failureDNS || kind == failureRefused || kind == failureUnreachable || kind == failureClosed || kind == failureBody
		if got := IsRetryableReadError(err); got != want {
			t.Fatalf("kind=%d retryable=%t want=%t", kind, got, want)
		}
	}
}

func TestBeforeSendRetryPolicyAndEvidence(t *testing.T) {
	for _, tc := range []struct {
		name      string
		request   Request
		failure   error
		wantCalls int
	}{
		{"readonly-POST", Request{Method: http.MethodPost, RetryReadOnly: true}, &TransportError{kind: failureTimeout}, 4},
		{"unsent-POST", Request{Method: http.MethodPost, RetryBeforeSend: true}, &TransportError{kind: failureTimeout, requestNotSent: true}, 4},
		{"sent-POST", Request{Method: http.MethodPost, RetryBeforeSend: true}, &TransportError{kind: failureTimeout}, 1},
		{"missing-POST-evidence", Request{Method: http.MethodPost, RetryBeforeSend: true}, context.DeadlineExceeded, 1},
		{"GET-action-already-sent", Request{Method: http.MethodGet, RetryBeforeSend: true}, &TransportError{kind: failureTimeout}, 1},
		{"GET-action-not-sent", Request{Method: http.MethodGet, RetryBeforeSend: true}, &TransportError{kind: failureTimeout, requestNotSent: true}, 4},
		{"single-explicit", Request{Method: http.MethodPost, RetryBeforeSend: true, SingleAttempt: true}, &TransportError{kind: failureTimeout, requestNotSent: true}, 1},
		{"permanent-unsent", Request{Method: http.MethodPost, RetryBeforeSend: true}, &TransportError{kind: failureCertificate, requestNotSent: true}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			_, err := DoReadWithRetry(t.Context(), tc.request, 3, 0, func(context.Context, Request) (*Response, error) { calls++; return nil, tc.failure }, nil)
			if calls != tc.wantCalls || IsRequestDefinitelyUnsent(err) != IsRequestDefinitelyUnsent(tc.failure) {
				t.Fatalf("calls=%d err=%v unsent=%t", calls, err, IsRequestDefinitelyUnsent(err))
			}
		})
	}
	unsent := &TransportError{kind: failureTimeout, requestNotSent: true}
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{fmt.Errorf("wrapper: %w", unsent), true},
		{errors.Join(unsent, &TransportError{kind: failureClosed, requestNotSent: true}), true},
		{errors.Join(unsent, &TransportError{kind: failureClosed}), false},
		{errors.Join(unsent, io.EOF), false},
		{errors.Join(unsent, errors.New("unknown")), false},
		{ReadRetriesExhausted(unsent, 3), true},
	} {
		if IsRequestDefinitelyUnsent(tc.err) != tc.want {
			t.Errorf("unsent evidence wrong for %v", tc.err)
		}
	}
}

func TestClassificationDoesNotHidePermanentJoinedFailures(t *testing.T) {
	for _, err := range []error{
		errors.Join(context.DeadlineExceeded, errors.New("private permanent failure")),
		errors.Join(&TransportError{kind: failureTimeout, requestNotSent: true}, errors.New("private unknown failure")),
		fmt.Errorf("private wrapper: %w", errors.Join(io.EOF, &TransportError{kind: failureCertificate})),
	} {
		failure := classifyTransportError(err, phaseRequest, false)
		if IsRetryableReadError(failure) || IsRequestDefinitelyUnsent(failure) {
			t.Fatalf("classification borrowed joined retry or unsent evidence: %v", failure)
		}
	}
}

func TestRetryCancellationAndExhaustionRetainsAllAttemptEvidence(t *testing.T) {
	for _, firstSent := range []bool{false, true} {
		for _, cancelDuringDelay := range []bool{false, true} {
			t.Run(fmt.Sprintf("sent=%t/cancel=%t", firstSent, cancelDuringDelay), func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				calls := 0
				delay := time.Duration(0)
				if cancelDuringDelay {
					delay = time.Minute
				}
				_, err := DoReadWithRetry(ctx, Request{Method: http.MethodGet}, 3, delay, func(context.Context, Request) (*Response, error) {
					calls++
					return nil, &TransportError{kind: failureTimeout, requestNotSent: !firstSent || calls > 1}
				}, func(ReadRetryEvent) {
					if cancelDuringDelay {
						cancel()
					}
				})
				if IsRequestDefinitelyUnsent(err) == firstSent {
					t.Fatalf("lost aggregate evidence calls=%d err=%v", calls, err)
				}
				if cancelDuringDelay && !errors.Is(err, context.Canceled) {
					t.Fatal("lost parent cancellation", err)
				}
			})
		}
	}
}

func TestReadRetryRecoversTimeoutAndPreservesRequest(t *testing.T) {
	for _, method := range []string{"", http.MethodGet, http.MethodHead} {
		calls := 0
		events := []ReadRetryEvent{}
		request := Request{Method: method, URL: "https://fixture.invalid/?private=canary", Headers: http.Header{"X-Fixture": {"kept"}}, Timeout: 10 * time.Millisecond}
		response := &Response{StatusCode: http.StatusOK}
		got, err := DoReadWithRetry(t.Context(), request, 3, 0, func(ctx context.Context, next Request) (*Response, error) {
			calls++
			if ctx.Err() != nil || next.URL != request.URL || next.Timeout != request.Timeout || next.Headers.Get("X-Fixture") != "kept" || !next.SingleAttempt {
				t.Fatal("retry changed request or parent context")
			}
			if calls <= 2 {
				return nil, fmt.Errorf("https://private.invalid/token=secret: %w", context.DeadlineExceeded)
			}
			return response, nil
		}, func(event ReadRetryEvent) { events = append(events, event) })
		if err != nil || got != response || calls != 3 || len(events) != 2 {
			t.Fatalf("read did not recover calls=%d events=%d err=%v", calls, len(events), err)
		}
		for n, event := range events {
			if event.Retry != n+1 || event.MaxRetries != 3 || event.Exhausted || strings.Contains(event.Message(), "private") {
				t.Fatal("incorrect or private retry progress", event.Message())
			}
		}
	}
}

func TestReadRetryExhaustionAndZeroBudget(t *testing.T) {
	for _, budget := range []int{0, 3, 20} {
		calls := 0
		events := []ReadRetryEvent{}
		failure := fmt.Errorf("private-canary URL: %w", context.DeadlineExceeded)
		_, err := DoReadWithRetry(t.Context(), Request{Method: http.MethodGet}, budget, 0, func(context.Context, Request) (*Response, error) {
			calls++
			return nil, failure
		}, func(event ReadRetryEvent) { events = append(events, event) })
		if calls != budget+1 || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal("read budget or cause lost", calls, err)
		}
		if budget == 0 {
			if err != failure || len(events) != 0 {
				t.Fatal("zero budget was wrapped or announced as exhausted")
			}
			continue
		}
		if len(events) != budget+1 || !events[len(events)-1].Exhausted || events[len(events)-1].Retry != budget {
			t.Fatal("exhaustion did not report actual attempts")
		}
		if !strings.Contains(err.Error(), fmt.Sprintf("已重试 %d 次（共 %d 次请求）", budget, budget+1)) {
			t.Fatal("exhaustion missing count", err)
		}
		if got := ReadFailureMessage(fmt.Errorf("private wrapper: %w", err)); got != err.Error() {
			t.Fatal("public message lost exhaustion or kept private wrapper", got)
		}
		encoded, _ := json.Marshal(err)
		for _, output := range []string{err.Error(), fmt.Sprintf("%v", err), fmt.Sprintf("%+v", err), fmt.Sprintf("%#v", err), fmt.Sprintf("%q", err), string(encoded)} {
			if strings.Contains(output, "private-canary") {
				t.Fatal("exhaustion leaked the underlying error")
			}
		}
		assertDiagnosticSurvivesRedaction(t, err)
	}
}

func TestReadRetryNeverReplaysWritesSecurityRequestsOrHTTPResponses(t *testing.T) {
	for _, tc := range []struct {
		name     string
		request  Request
		response *Response
		failure  error
	}{
		{"post", Request{Method: http.MethodPost}, nil, context.DeadlineExceeded},
		{"put", Request{Method: http.MethodPut}, nil, context.DeadlineExceeded},
		{"patch", Request{Method: http.MethodPatch}, nil, context.DeadlineExceeded},
		{"delete", Request{Method: http.MethodDelete}, nil, context.DeadlineExceeded},
		{"security-read", Request{Method: http.MethodGet, SingleAttempt: true}, nil, context.DeadlineExceeded},
		{"HTTP-403", Request{Method: http.MethodGet}, &Response{StatusCode: 403}, nil},
		{"HTTP-503", Request{Method: http.MethodGet}, &Response{StatusCode: 503}, nil},
		{"response-plus-error", Request{Method: http.MethodGet}, &Response{StatusCode: 200}, io.ErrUnexpectedEOF},
		{"certificate", Request{Method: http.MethodGet}, nil, &TransportError{kind: failureCertificate}},
		{"proxy-auth", Request{Method: http.MethodGet}, nil, &TransportError{kind: failureProxyAuth}},
		{"proxy-config", Request{Method: http.MethodGet}, nil, &TransportError{kind: failureProxyConfig}},
		{"unknown", Request{Method: http.MethodGet}, nil, errors.New("unknown private error")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			response, err := DoReadWithRetry(t.Context(), tc.request, 3, 0, func(context.Context, Request) (*Response, error) {
				calls++
				return tc.response, tc.failure
			}, func(ReadRetryEvent) { t.Fatal("nonretryable request emitted retry") })
			if calls != 1 || response != tc.response || err != tc.failure {
				t.Fatal("nonretryable request was replayed or modified")
			}
		})
	}
}

func TestReadRetryParentCancellationStopsDuringDelay(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	calls := 0
	started := time.Now()
	_, err := DoReadWithRetry(ctx, Request{Method: http.MethodGet}, 3, time.Minute, func(context.Context, Request) (*Response, error) {
		calls++
		return nil, context.DeadlineExceeded
	}, func(event ReadRetryEvent) {
		if event.Exhausted {
			t.Fatal("parent cancellation reported retry exhaustion")
		}
		cancel()
	})
	if !errors.Is(err, context.Canceled) || calls != 1 || time.Since(started) > time.Second {
		t.Fatal("read retry ignored cancellation")
	}
	ctx, cancel = context.WithCancel(t.Context())
	cancel()
	_, err = DoReadWithRetry(ctx, Request{}, 3, 0, func(context.Context, Request) (*Response, error) {
		t.Fatal("canceled parent reached network")
		return nil, nil
	}, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatal("lost parent cancellation")
	}
}

func TestReadRetryInvalidBudgetFailsBeforeNetwork(t *testing.T) {
	for _, tc := range []struct {
		retries int
		delay   time.Duration
	}{{-1, 0}, {21, 0}, {3, -time.Millisecond}} {
		_, err := DoReadWithRetry(t.Context(), Request{}, tc.retries, tc.delay, func(context.Context, Request) (*Response, error) {
			t.Fatal("invalid budget reached network")
			return nil, nil
		}, nil)
		if err == nil {
			t.Fatal("invalid budget silently accepted")
		}
	}
}
