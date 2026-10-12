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
	"time"
)

const DefaultNetworkRetries = 3
const DefaultNetworkRetryDelay = time.Second

// ReadRetryEvent describes an additional attempt, or the exhausted retry budget.
// Err is for classification only. Log Message rather than serializing raw errors.
type ReadRetryEvent struct {
	Retry, MaxRetries int
	Delay             time.Duration
	Err               error
	Exhausted         bool
}

func (e ReadRetryEvent) Message() string {
	reason := readFailureReason(e.Err)
	if e.Exhausted {
		return fmt.Sprintf("%s；已重试 %d 次（共 %d 次请求），仍未成功", reason, e.Retry, e.Retry+1)
	}
	if e.Delay > 0 {
		return fmt.Sprintf("%s；将在 %s 后进行第 %d/%d 次重试", reason, e.Delay.String(), e.Retry, e.MaxRetries)
	}
	return fmt.Sprintf("%s；正在进行第 %d/%d 次重试", reason, e.Retry, e.MaxRetries)
}

// IsRetryableReadError only accepts recognized transient failures. A received
// HTTP response, security rejection, invalid configuration or certificate error
// is not a transient connection failure. This never authorizes replaying writes.
func IsRetryableReadError(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	var unknown x509.UnknownAuthorityError
	var invalid x509.CertificateInvalidError
	var hostname x509.HostnameError
	var roots x509.SystemRootsError
	if errors.As(err, &unknown) || errors.As(err, &invalid) || errors.As(err, &hostname) || errors.As(err, &roots) {
		return false
	}
	if failure, ok := err.(*TransportError); ok && failure != nil {
		if failure.permanentNetwork {
			return false
		}
		switch failure.kind {
		case failureRequest:
			return failure.transientNetwork
		case failureTimeout, failureDNS, failureRefused, failureUnreachable, failureClosed, failureBody:
			return true
		default:
			return false
		}
	}
	// A joined permanent or unknown failure must not borrow retry permission
	// from a timeout in another branch.
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		children := joined.Unwrap()
		if len(children) == 0 {
			return false
		}
		for _, child := range children {
			if !IsRetryableReadError(child) {
				return false
			}
		}
		return true
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return dns.IsTimeout || dns.IsTemporary
	}
	if isTLSProtocolFailure(err) {
		return false
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return IsRetryableReadError(wrapped.Unwrap())
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNABORTED) || errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ENETUNREACH) || errors.Is(err, syscall.EHOSTUNREACH) || errors.Is(err, syscall.ENETDOWN) || errors.Is(err, syscall.EHOSTDOWN) || errors.Is(err, syscall.ENETRESET) {
		return true
	}
	var network net.Error
	return errors.As(err, &network) && (network.Timeout() || network.Temporary())
}

// IsRequestDefinitelyUnsent accepts only the explicit evidence recorded by a
// transport before an HTTP-capable connection was obtained. Missing header
// callbacks, raw network errors, or an unrelated joined cause are insufficient.
func IsRequestDefinitelyUnsent(err error) bool {
	if failure, ok := err.(*TransportError); ok && failure != nil {
		return failure.requestNotSent
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		children := joined.Unwrap()
		if len(children) == 0 {
			return false
		}
		for _, child := range children {
			if !IsRequestDefinitelyUnsent(child) {
				return false
			}
		}
		return true
	} else if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return IsRequestDefinitelyUnsent(wrapped.Unwrap())
	}
	return false
}

func readFailureReason(err error) string {
	if errors.Is(err, context.Canceled) {
		return "请求已取消"
	}
	var failure *TransportError
	if errors.As(err, &failure) && failure != nil {
		switch failure.kind {
		case failureTimeout:
			return "请求超时"
		case failureDNS:
			if failure.permanentNetwork {
				return "域名不存在或解析配置错误"
			}
			return "域名解析暂时失败"
		case failureRefused:
			return "连接被拒绝"
		case failureUnreachable:
			return "网络暂时不可达"
		case failureClosed:
			return "网络连接中断"
		case failureBody:
			return "响应读取中断"
		case failureCertificate:
			return "证书验证失败"
		case failureTLS:
			return "TLS 协商或协议不兼容"
		case failureResponseTooLarge:
			return "响应超过大小限制"
		case failureProxyAuth:
			return "代理认证被拒绝"
		}
	}
	var network net.Error
	if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &network) && network.Timeout() {
		return "请求超时"
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, syscall.ECONNRESET) {
		return "网络连接中断"
	}
	if IsRetryableReadError(err) {
		return "网络请求未完成"
	}
	return "请求未完成"
}

// ReadFailureMessage exposes the closed failure classification and preserves
// actual exhaustion counts without formatting raw transport or server errors.
func ReadFailureMessage(err error) string {
	var exhausted *readRetriesExhaustedError
	if errors.As(err, &exhausted) && exhausted != nil {
		return exhausted.Error()
	}
	return readFailureReason(err)
}

type readRetriesExhaustedError struct {
	cause   error
	retries int
}

func (e *readRetriesExhaustedError) Error() string {
	return (ReadRetryEvent{Retry: e.retries, MaxRetries: e.retries, Err: e.cause, Exhausted: true}).Message()
}
func (e *readRetriesExhaustedError) Unwrap() error { return e.cause }
func (e *readRetriesExhaustedError) MarshalJSON() ([]byte, error) {
	return json.Marshal(e.Error())
}
func (e *readRetriesExhaustedError) Format(s fmt.State, verb rune) {
	formatSafeError(s, verb, e.Error())
}

// ReadRetriesExhausted retains errors.Is/As without including private URLs,
// credentials or server text in any formatted or JSON diagnostic.
func ReadRetriesExhausted(err error, retries int) error {
	if err == nil || retries <= 0 {
		return err
	}
	return &readRetriesExhaustedError{cause: err, retries: retries}
}

// DoReadWithRetry owns one finite retry budget. Callers must pass a single
// attempt do function to avoid multiplying retries across transport layers.
// Every attempt retains the parent deadline and the request's own timeout.
func DoReadWithRetry(ctx context.Context, request Request, retries int, delay time.Duration, do func(context.Context, Request) (*Response, error), onRetry func(ReadRetryEvent)) (*Response, error) {
	if retries < 0 || retries > 20 {
		return nil, errors.New("connection retry count must be between 0 and 20")
	}
	if delay < 0 {
		return nil, errors.New("connection retry delay must be nonnegative")
	}
	if do == nil {
		return nil, errors.New("read retry transport is unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	method := strings.ToUpper(request.Method)
	// Explicit before-send policy also marks business writes implemented as
	// GET (for example buying/canceling an SMS number). HTTP method alone must
	// not grant permission to replay these actions after a connection exists.
	readOnly := request.RetryReadOnly || !request.RetryBeforeSend && (method == "" || method == http.MethodGet || method == http.MethodHead)
	if request.SingleAttempt || !readOnly && !request.RetryBeforeSend {
		retries = 0
	}
	// This helper owns the retry decision. Each underlying attempt must disable
	// HTTP-library replay, including GETs on failed pooled HTTP/2 connections.
	attemptRequest := request
	attemptRequest.SingleAttempt = true
	var previous error
	allRequestsUnsent := true
	for attempted := 0; ; attempted++ {
		if err := ctx.Err(); err != nil {
			return nil, retryParentFailure(previous, err, allRequestsUnsent)
		}
		response, err := do(ctx, attemptRequest)
		if err == nil || response != nil {
			return response, err
		}
		allRequestsUnsent = allRequestsUnsent && IsRequestDefinitelyUnsent(err)
		err = preserveRetryEvidence(err, allRequestsUnsent)
		previous = err
		if cause := ctx.Err(); cause != nil {
			return nil, retryParentFailure(err, cause, allRequestsUnsent)
		}
		if !IsRetryableReadError(err) || !readOnly && !IsRequestDefinitelyUnsent(err) {
			return nil, err
		}
		if attempted >= retries {
			if attempted > 0 && onRetry != nil {
				onRetry(ReadRetryEvent{Retry: attempted, MaxRetries: retries, Err: err, Exhausted: true})
			}
			return nil, ReadRetriesExhausted(err, attempted)
		}
		if onRetry != nil {
			onRetry(ReadRetryEvent{Retry: attempted + 1, MaxRetries: retries, Delay: delay, Err: err})
		}
		if delay > 0 {
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil, retryParentFailure(err, ctx.Err(), allRequestsUnsent)
			case <-timer.C:
			}
		}
	}
}

func preserveRetryEvidence(err error, allRequestsUnsent bool) error {
	if allRequestsUnsent || !IsRequestDefinitelyUnsent(err) {
		return err
	}
	// A final dial failure cannot erase the fact that an earlier safe read
	// reached HTTP. Preserve the closed cause while clearing global evidence.
	var failure *TransportError
	if errors.As(err, &failure) && failure != nil {
		copy := *failure
		copy.requestNotSent = false
		return &copy
	}
	return err
}

func retryParentFailure(previous, cause error, allRequestsUnsent bool) error {
	if previous == nil {
		failure := classifyRequestError(nil, cause)
		failure.requestNotSent = true
		return failure
	}
	if errors.Is(previous, cause) {
		return preserveRetryEvidence(previous, allRequestsUnsent)
	}
	var failure *TransportError
	if errors.As(previous, &failure) && failure != nil {
		return preserveRetryEvidence(classifyRequestError(previous, cause), allRequestsUnsent)
	}
	return cause
}
