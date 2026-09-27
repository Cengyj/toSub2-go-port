package transport

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
	"syscall"
)

type failureKind uint8

const (
	failureRequest failureKind = iota
	failureCanceled
	failureTimeout
	failureDNS
	failureRefused
	failureUnreachable
	failureCertificate
	failureTLS
	failureClosed
	failureProxyAuth
	failureProxyStatus
	failureProxyConfig
	failureProfile
	failureInit
	failureInvalidRequest
	failureBody
)

type failurePhase uint8

const (
	phaseRequest failurePhase = iota
	phaseProxyConnect
	phaseProxyTLS
	phaseProxyTunnel
)

// TransportError stores only a closed classification, an HTTP status, and a
// context sentinel. Never retain a raw error: url.Error, net.OpError and even
// HTTP parsing errors can contain credentials, query tokens or server text.
type TransportError struct {
	kind          failureKind
	phase         failurePhase
	status        int
	loopbackProxy bool
	contextCause  error
}

const loopbackProxyHint = "The configured proxy uses a loopback address; if this service runs in a container, that address refers to the container itself. Configure a proxy endpoint reachable from the container."

func (e TransportError) message() string {
	switch e.kind {
	case failureCanceled:
		return "request canceled"
	case failureTimeout:
		if e.phase == phaseProxyConnect {
			return "proxy connection timed out"
		}
		return "request timed out"
	case failureDNS:
		if e.phase == phaseProxyConnect {
			return "proxy DNS lookup failed"
		}
		return "DNS lookup failed"
	case failureRefused:
		if e.phase == phaseProxyConnect {
			return "proxy connection refused"
		}
		return "connection refused"
	case failureUnreachable:
		if e.phase == phaseProxyConnect {
			return "proxy network is unreachable"
		}
		return "network is unreachable"
	case failureCertificate:
		if e.phase == phaseProxyTLS {
			return "HTTPS proxy certificate validation failed"
		}
		return "TLS certificate validation failed"
	case failureTLS:
		if e.phase == phaseProxyTLS {
			return "HTTPS proxy TLS handshake failed"
		}
		return "TLS handshake or protocol failed"
	case failureClosed:
		return "connection closed before HTTP headers"
	case failureProxyAuth:
		return "proxy authentication rejected (HTTP 407)"
	case failureProxyStatus:
		if e.status >= 100 && e.status <= 999 {
			return fmt.Sprintf("proxy CONNECT rejected (HTTP %d)", e.status)
		}
		return "proxy CONNECT rejected"
	case failureProxyConfig:
		return "invalid or unsupported proxy configuration"
	case failureProfile:
		return "requested TLS profile is not bundled"
	case failureInit:
		return "TLS transport initialization failed"
	case failureInvalidRequest:
		return "invalid TLS request"
	case failureBody:
		return "TLS response body read failed"
	default:
		if e.phase == phaseProxyConnect {
			return "proxy connection failed"
		}
		if e.phase == phaseProxyTunnel {
			return "proxy CONNECT request failed"
		}
		return "TLS transport request failed before HTTP headers"
	}
}

func (e TransportError) hasLoopbackHint() bool {
	return e.loopbackProxy && e.phase == phaseProxyConnect &&
		(e.kind == failureRefused || e.kind == failureUnreachable || e.kind == failureTimeout || e.kind == failureRequest)
}
func (e TransportError) Error() string {
	message := e.message()
	if e.hasLoopbackHint() {
		message += ". " + loopbackProxyHint
	}
	return message
}
func (e TransportError) Unwrap() error                 { return e.contextCause }
func (e TransportError) MarshalJSON() ([]byte, error)  { return json.Marshal(e.Error()) }
func (e TransportError) Format(s fmt.State, verb rune) { formatSafeError(s, verb, e.Error()) }

func formatSafeError(s fmt.State, verb rune, message string) {
	if verb == 'q' {
		message = strconv.Quote(message)
	}
	_, _ = io.WriteString(s, message)
}

func isLoopbackProxy(u *url.URL) bool {
	if u == nil {
		return false
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if host == "localhost" {
		return true
	}
	return net.ParseIP(host).IsLoopback()
}

func classifyTransportError(err error, phase failurePhase, loopback bool) *TransportError {
	var existing *TransportError
	if errors.As(err, &existing) && existing != nil {
		copy := *existing
		return &copy
	}
	d := &TransportError{kind: failureRequest, phase: phase, loopbackProxy: loopback}
	var dns *net.DNSError
	var netErr net.Error
	var unknown x509.UnknownAuthorityError
	var invalid x509.CertificateInvalidError
	var hostname x509.HostnameError
	var roots x509.SystemRootsError
	var record tls.RecordHeaderError
	switch {
	case errors.Is(err, context.Canceled):
		d.kind, d.contextCause = failureCanceled, context.Canceled
	case errors.Is(err, context.DeadlineExceeded):
		d.kind, d.contextCause = failureTimeout, context.DeadlineExceeded
	case errors.As(err, &dns):
		d.kind = failureDNS
	case errors.Is(err, syscall.ECONNREFUSED):
		d.kind = failureRefused
	case errors.Is(err, syscall.ENETUNREACH), errors.Is(err, syscall.EHOSTUNREACH):
		d.kind = failureUnreachable
	case errors.As(err, &unknown), errors.As(err, &invalid), errors.As(err, &hostname), errors.As(err, &roots):
		d.kind = failureCertificate
	case errors.As(err, &netErr) && netErr.Timeout():
		d.kind = failureTimeout
	case errors.As(err, &record), phase == phaseProxyTLS:
		d.kind = failureTLS
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, syscall.ECONNRESET):
		d.kind = failureClosed
	}
	return d
}

// Keep cancellation's errors.Is contract while retaining an already-known
// proxy phase. Raw causes are deliberately discarded after classification.
func classifyRequestError(err, contextErr error) *TransportError {
	if contextErr == nil {
		return classifyTransportError(err, phaseRequest, false)
	}
	var existing *TransportError
	if errors.As(err, &existing) && existing != nil {
		return classifyTransportError(contextErr, existing.phase, existing.loopbackProxy)
	}
	return classifyTransportError(contextErr, phaseRequest, false)
}

func classifyProxyError(ctx context.Context, err error, phase failurePhase, proxyURL *url.URL) *TransportError {
	if cause := proxyContextError(ctx); cause != nil {
		err = cause
	}
	return classifyTransportError(err, phase, isLoopbackProxy(proxyURL))
}

// ProfileProbeError describes why no candidate reached HTTP. A failed network
// path does not establish that any ClientHello profile is incompatible.
type ProfileProbeError struct{ failures []*TransportError }

func (e ProfileProbeError) Error() string {
	var messages []string
	counts := map[string]int{}
	loopback := false
	for _, failure := range e.failures {
		message := failure.message()
		if counts[message] == 0 {
			messages = append(messages, message)
		}
		counts[message]++
		loopback = loopback || failure.hasLoopbackHint()
	}
	for i, message := range messages {
		messages[i] = fmt.Sprintf("%s (%d candidate attempts)", message, counts[message])
	}
	// Do not phrase this as a sensitive-key assignment (for example,
	// "headers: proxy"): generic log redaction would hide the safe cause.
	message := "TLS profile probe failed before receiving an HTTP response: " + strings.Join(messages, "; ")
	if loopback {
		message += ". " + loopbackProxyHint
	}
	return message
}
func (e ProfileProbeError) Unwrap() []error {
	out := make([]error, 0, len(e.failures))
	for _, failure := range e.failures {
		out = append(out, failure)
	}
	return out
}
func (e ProfileProbeError) MarshalJSON() ([]byte, error)  { return json.Marshal(e.Error()) }
func (e ProfileProbeError) Format(s fmt.State, verb rune) { formatSafeError(s, verb, e.Error()) }
