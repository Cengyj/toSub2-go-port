package transport

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptrace"
	"sync/atomic"
	"time"
)

// DoHTTPWithRetry adapts standard HTTP callers to the same finite retry policy
// used by protocol clients. Only successful response bodies are read: an HTTP
// rejection is a completed request, not a transient network failure. A zero
// size limit requests headers only; positive limits bound the successful body.
// Redirects remain observable to the caller rather than replaying an action.
func DoHTTPWithRetry(ctx context.Context, client *http.Client, request Request, maxResponseBytes int64, retries int, delay time.Duration, onRetry func(ReadRetryEvent)) (*Response, error) {
	if maxResponseBytes < 0 || maxResponseBytes == math.MaxInt64 {
		return nil, errors.New("HTTP response size limit is invalid")
	}
	if client == nil {
		client = http.DefaultClient
	}
	return DoReadWithRetry(ctx, request, retries, delay, func(parent context.Context, next Request) (*Response, error) {
		return doHTTPAttempt(parent, client, next, maxResponseBytes)
	}, onRetry)
}

func doHTTPAttempt(parent context.Context, client *http.Client, request Request, maxResponseBytes int64) (*Response, error) {
	timeout := request.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	copyClient := *client
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	tr, traceSupported := client.Transport.(*http.Transport)
	if client.Transport == nil {
		tr, traceSupported = http.DefaultTransport.(*http.Transport)
	}
	if traceSupported {
		// A new pool for every attempt excludes hidden retries of stale pooled
		// sockets. The trace additionally blocks HTTP/2 connection replay.
		isolated := tr.Clone()
		isolated.DisableKeepAlives = true
		preserveNativeDialScope(isolated)
		defer isolated.CloseIdleConnections()
		copyClient.Transport = isolated
	}
	scope := newRequestDialScope(ctx)
	scope.native = true
	defer scope.finish(true)
	ctx = context.WithValue(ctx, requestDialScopeKey{}, scope)
	ctx = httptrace.WithClientTrace(ctx, scope.nativeTrace())
	var gotConn, wrote atomic.Bool
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		WroteHeaders: func() { wrote.Store(true) },
		GotConn: func(info httptrace.GotConnInfo) {
			repeated := gotConn.Swap(true)
			if repeated || wrote.Load() {
				cancel()
				_ = info.Conn.Close()
			}
		},
	})
	method := request.Method
	if method == "" {
		method = http.MethodGet
	}
	hreq, err := http.NewRequestWithContext(ctx, method, request.URL, bytes.NewReader(request.Body))
	if err != nil {
		return nil, &TransportError{kind: failureInvalidRequest}
	}
	if hreq.URL.Scheme == "https" {
		scope.tlsStages = 1
	}
	hreq.Header = request.Headers.Clone()
	if hreq.Header == nil {
		hreq.Header = make(http.Header)
	}
	hreq.GetBody = nil
	hreq.Close = true
	resp, err := copyClient.Do(hreq)
	if err != nil {
		failure := classifyRequestError(err, ctx.Err())
		// Custom RoundTrippers may omit tracing even after they sent a write.
		// Their absence of GotConn is not transport evidence of an unsent
		// request; only the standard transport has the proven trace contract.
		failure.requestNotSent = traceSupported && !gotConn.Load()
		return nil, failure
	}
	defer resp.Body.Close()
	response := &Response{StatusCode: resp.StatusCode, Header: resp.Header.Clone(), URL: hreq.URL, RawSetCookie: GetSetCookie(resp.Header)}
	if resp.Request != nil && resp.Request.URL != nil {
		response.URL = resp.Request.URL
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || maxResponseBytes == 0 {
		return response, nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if cause := ctx.Err(); cause != nil {
		return nil, classifyRequestError(err, cause)
	}
	if err != nil {
		failure := classifyRequestError(err, nil)
		if IsRetryableReadError(failure) {
			// Headers were received: a mutating request is always considered
			// sent, even if no response body could be read.
			failure.kind = failureBody
		}
		return nil, failure
	}
	if int64(len(body)) > maxResponseBytes {
		return nil, &TransportError{kind: failureResponseTooLarge}
	}
	response.Body = body
	return response, nil
}
