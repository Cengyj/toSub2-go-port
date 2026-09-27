package transport

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"sync"

	"golang.org/x/net/proxy"
)

type requestDialScopeKey struct{}

// A scope keeps the original request cancellation through transports which
// intentionally detach their dial context. It owns only newly dialled sockets,
// never a connection obtained from an existing pool.
type requestDialScope struct {
	ctx                     context.Context
	mu                      sync.Mutex
	finished                bool
	native                  bool
	connections             map[net.Conn]func() bool
	tlsStages, tlsRemaining int
}

func newRequestDialScope(ctx context.Context) *requestDialScope {
	return &requestDialScope{ctx: ctx, connections: make(map[net.Conn]func() bool)}
}

func (s *requestDialScope) dial(parent context.Context, base proxy.ContextDialer, network, address string) (net.Conn, error) {
	s.mu.Lock()
	finished := s.finished
	s.mu.Unlock()
	if finished {
		return nil, context.Canceled
	}
	if err := s.ctx.Err(); err != nil {
		return nil, err
	}
	dialCtx, cancel := context.WithCancel(s.ctx)
	stopParent := context.AfterFunc(parent, cancel)
	defer cancel()
	defer stopParent()
	conn, err := base.DialContext(dialCtx, network, address)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finished || s.ctx.Err() != nil {
		_ = conn.Close()
		if err := s.ctx.Err(); err != nil {
			return nil, err
		}
		return nil, context.Canceled
	}
	s.tlsRemaining = s.tlsStages
	// Plain native HTTP has no handshake after DialContext returns. The pool
	// may hand that socket to a different waiter, so ownership ends here.
	if s.native && s.tlsStages == 0 {
		return conn, nil
	}
	s.connections[conn] = context.AfterFunc(s.ctx, func() { _ = conn.Close() })
	return conn, nil
}

func (s *requestDialScope) releaseLocked(closePending bool) {
	for conn, stop := range s.connections {
		stopped := stop()
		if closePending || !stopped {
			_ = conn.Close()
		}
		delete(s.connections, conn)
	}
}

// Native transports relinquish ownership before an established HTTP/2 socket
// enters a shared pool; subsequent cancellation belongs to the HTTP stream.
func (s *requestDialScope) nativeTrace() *httptrace.ClientTrace {
	return &httptrace.ClientTrace{
		TLSHandshakeDone: func(_ tls.ConnectionState, err error) {
			if err != nil {
				return
			}
			s.mu.Lock()
			defer s.mu.Unlock()
			s.tlsRemaining--
			if s.tlsRemaining <= 0 {
				s.releaseLocked(false)
			}
		},
	}
}

func (s *requestDialScope) finish(closePending bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.finished = true
	s.releaseLocked(closePending || s.ctx.Err() != nil)
}

type contextDialFunc func(context.Context, string, string) (net.Conn, error)

func (f contextDialFunc) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	return f(ctx, network, address)
}

func preserveNativeDialScope(tr *http.Transport) {
	dial := tr.DialContext
	if dial == nil {
		dial = (&net.Dialer{}).DialContext
	}
	tr.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if scope, ok := ctx.Value(requestDialScopeKey{}).(*requestDialScope); ok {
			return scope.dial(ctx, contextDialFunc(dial), network, address)
		}
		return dial(ctx, network, address)
	}
	if original := tr.Proxy; original != nil {
		tr.Proxy = func(req *http.Request) (*url.URL, error) {
			u, err := original(req)
			if scope, ok := req.Context().Value(requestDialScopeKey{}).(*requestDialScope); ok {
				scope.mu.Lock()
				scope.tlsStages = 0
				if req.URL.Scheme == "https" {
					scope.tlsStages++
				}
				if u != nil && u.Scheme == "https" {
					scope.tlsStages++
				}
				scope.mu.Unlock()
			}
			return u, err
		}
	}
}

// tls-client v1.9's HTTP/2 redial callback supplies context.Background. The
// per-client gate makes this owner unambiguous without changing ClientHello,
// disabling HTTP/2, or coupling independent job clients.
type fingerprintDialer struct {
	mu    sync.RWMutex
	owner *requestDialScope
	base  proxy.ContextDialer
}

func (d *fingerprintDialer) bind(scope *requestDialScope) {
	d.mu.Lock()
	d.owner = scope
	d.mu.Unlock()
}
func (d *fingerprintDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	scope, _ := ctx.Value(requestDialScopeKey{}).(*requestDialScope)
	if scope == nil {
		d.mu.RLock()
		scope = d.owner
		d.mu.RUnlock()
	}
	if scope == nil {
		return nil, context.Canceled
	}
	return scope.dial(ctx, d.base, network, address)
}
func (d *fingerprintDialer) Dial(network, address string) (net.Conn, error) {
	return d.DialContext(context.Background(), network, address)
}
