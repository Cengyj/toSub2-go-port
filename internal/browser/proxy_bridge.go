package browser

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/proxy"
)

// proxyBridge belongs to one browser session. It exposes an authenticated,
// loopback-only HTTP proxy and forwards both HTTP and CONNECT through SOCKS5.
// CONNECT forwards raw bytes, so HTTPS is still negotiated by Chromium itself.
type proxyBridge struct {
	server             *http.Server
	listener           net.Listener
	client             *http.Transport
	cancel             context.CancelFunc
	username, password string
	endpoint           string
	mu                 sync.Mutex
	connections        map[net.Conn]struct{}
	once               sync.Once
}

func newProxyBridge(parent context.Context, raw string) (*proxyBridge, error) {
	upstream, err := url.Parse(raw)
	if err != nil || upstream.Hostname() == "" || (upstream.Scheme != "socks5" && upstream.Scheme != "socks5h") {
		return nil, errors.New("invalid SOCKS proxy URL")
	}
	var auth *proxy.Auth
	if upstream.User != nil {
		password, _ := upstream.User.Password()
		auth = &proxy.Auth{User: upstream.User.Username(), Password: password}
	}
	address := upstream.Host
	if upstream.Port() == "" {
		address = net.JoinHostPort(upstream.Hostname(), "1080")
	}
	dialer, err := proxy.SOCKS5("tcp", address, auth, &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second})
	if err != nil {
		return nil, errors.New("create SOCKS proxy dialer failed")
	}
	ctxDialer, ok := dialer.(proxy.ContextDialer)
	if !ok {
		return nil, errors.New("SOCKS dialer does not support context cancellation")
	}
	dial := func(ctx context.Context, network, address string) (net.Conn, error) {
		// socks5h leaves the hostname intact for resolution by the remote proxy.
		if upstream.Scheme == "socks5" {
			host, port, e := net.SplitHostPort(address)
			if e != nil {
				return nil, e
			}
			if net.ParseIP(host) == nil {
				ips, e := net.DefaultResolver.LookupIPAddr(ctx, host)
				if e != nil {
					return nil, errors.New("SOCKS local name resolution failed")
				}
				if len(ips) == 0 {
					return nil, errors.New("SOCKS local name resolution returned no address")
				}
				address = net.JoinHostPort(ips[0].IP.String(), port)
			}
		}
		return ctxDialer.DialContext(ctx, network, address)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, 32)
	if _, err = rand.Read(nonce); err != nil {
		_ = listener.Close()
		return nil, err
	}
	ctx, cancel := context.WithCancel(parent)
	b := &proxyBridge{listener: listener, cancel: cancel, username: "tosub2-session", password: hex.EncodeToString(nonce), connections: map[net.Conn]struct{}{}, endpoint: "http://" + listener.Addr().String()}
	// Chrome owns each protocol request's deadline. Target closure cancels the
	// incoming request context; a fixed header deadline would truncate longer
	// configured browser requests even when their contexts are still live.
	b.client = &http.Transport{Proxy: nil, DialContext: dial, ForceAttemptHTTP2: false, DisableCompression: true, IdleConnTimeout: 30 * time.Second}
	b.server = &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { b.serve(ctx, dial, w, r) }), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second, BaseContext: func(net.Listener) context.Context { return ctx }, ConnState: func(c net.Conn, state http.ConnState) {
		b.mu.Lock()
		defer b.mu.Unlock()
		if state == http.StateClosed {
			delete(b.connections, c)
		} else {
			b.connections[c] = struct{}{}
		}
	}}
	go func() { _ = b.server.Serve(listener) }()
	go func() { <-ctx.Done(); b.Close() }()
	return b, nil
}

func (b *proxyBridge) serve(ctx context.Context, dial func(context.Context, string, string) (net.Conn, error), w http.ResponseWriter, r *http.Request) {
	expected := "Basic " + base64.StdEncoding.EncodeToString([]byte(b.username+":"+b.password))
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("Proxy-Authorization")), []byte(expected)) != 1 {
		w.Header().Set("Proxy-Authenticate", `Basic realm="tosub2-browser-session"`)
		w.WriteHeader(http.StatusProxyAuthRequired)
		return
	}
	if r.Method == http.MethodConnect {
		target := r.Host
		if _, _, err := net.SplitHostPort(target); err != nil {
			http.Error(w, "invalid CONNECT address", http.StatusBadRequest)
			return
		}
		// Before hijacking, keep the upstream SOCKS handshake tied to the
		// individual CONNECT caller, not only the lifetime of the browser.
		requestCtx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		upstream, err := dial(requestCtx, "tcp", target)
		cancel()
		if err != nil {
			http.Error(w, "SOCKS connection failed", http.StatusBadGateway)
			return
		}
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			_ = upstream.Close()
			http.Error(w, "CONNECT unavailable", 500)
			return
		}
		downstream, buffered, err := hijacker.Hijack()
		if err != nil {
			_ = upstream.Close()
			return
		}
		defer func() {
			_ = upstream.Close()
			_ = downstream.Close()
			b.mu.Lock()
			delete(b.connections, downstream)
			b.mu.Unlock()
		}()
		_, err = buffered.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
		if err == nil {
			err = buffered.Flush()
		}
		if err != nil {
			return
		}
		finished := make(chan struct{}, 2)
		go func() { _, _ = io.Copy(upstream, buffered); finished <- struct{}{} }()
		go func() { _, _ = io.Copy(downstream, upstream); finished <- struct{}{} }()
		select {
		case <-ctx.Done():
		case <-finished:
		}
		_ = upstream.Close()
		_ = downstream.Close()
		// Wait for both copy goroutines after closing their connections.
		select {
		case <-finished:
		case <-time.After(time.Second):
		}
		return
	}
	if r.URL.Scheme != "http" || r.URL.Host == "" {
		http.Error(w, "HTTP proxy absolute URL required", http.StatusBadRequest)
		return
	}
	outgoing := r.Clone(r.Context())
	outgoing.RequestURI = ""
	outgoing.Header = r.Header.Clone()
	stripProxyHeaders(outgoing.Header)
	response, err := b.client.RoundTrip(outgoing)
	if err != nil {
		http.Error(w, "SOCKS request failed", http.StatusBadGateway)
		return
	}
	defer response.Body.Close()
	stripProxyHeaders(response.Header)
	for key, values := range response.Header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.WriteHeader(response.StatusCode)
	_, _ = io.Copy(w, response.Body)
}
func stripProxyHeaders(h http.Header) {
	for _, v := range h.Values("Connection") {
		for _, name := range strings.Split(v, ",") {
			h.Del(strings.TrimSpace(name))
		}
	}
	for _, name := range []string{"Proxy-Authorization", "Proxy-Authenticate", "Proxy-Connection", "Connection", "Keep-Alive", "TE", "Trailer", "Transfer-Encoding", "Upgrade"} {
		h.Del(name)
	}
}
func (b *proxyBridge) Close() {
	if b == nil {
		return
	}
	b.once.Do(func() {
		b.cancel()
		_ = b.server.Close()
		b.client.CloseIdleConnections()
		b.mu.Lock()
		for c := range b.connections {
			_ = c.Close()
		}
		b.connections = map[net.Conn]struct{}{}
		b.mu.Unlock()
	})
}
