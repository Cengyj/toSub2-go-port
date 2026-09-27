package transport

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"net/url"
	"time"
)

// connectProxyDialer keeps proxy TLS and CONNECT inside the request context.
// tls-client's bundled HTTPS proxy dialer starts with tls.Dial, which performs
// an unbounded handshake before consulting that context. The origin connection
// still uses tls-client's selected ClientHello after this tunnel is established.
type connectProxyDialer struct {
	proxyURL  *url.URL
	timeout   time.Duration
	localAddr *net.TCPAddr
	roots     *x509.CertPool
}

func (d *connectProxyDialer) Dial(network, address string) (net.Conn, error) {
	return d.DialContext(context.Background(), network, address)
}
func (d *connectProxyDialer) DialContext(parent context.Context, network, address string) (net.Conn, error) {
	timeout := d.timeout
	if timeout <= 0 {
		// Protocol requests already carry their configured deadline. Preserve a
		// longer caller budget; direct/unbounded dialer use still gets a limit.
		if _, bounded := parent.Deadline(); !bounded {
			timeout = 30 * time.Second
		}
	}
	ctx, cancel := context.WithCancel(parent)
	if timeout > 0 {
		cancel()
		ctx, cancel = context.WithTimeout(parent, timeout)
	}
	defer cancel()
	proxyAddress := d.proxyURL.Host
	if d.proxyURL.Port() == "" {
		port := "80"
		if d.proxyURL.Scheme == "https" {
			port = "443"
		}
		proxyAddress = net.JoinHostPort(d.proxyURL.Hostname(), port)
	}
	raw, err := (&net.Dialer{Timeout: timeout, LocalAddr: d.localAddr, KeepAlive: 30 * time.Second}).DialContext(ctx, network, proxyAddress)
	if err != nil {
		return nil, classifyProxyError(ctx, err, phaseProxyConnect, d.proxyURL)
	}
	conn := raw
	cleanup := true
	defer func() {
		if cleanup {
			_ = raw.Close()
		}
	}()
	stop := context.AfterFunc(ctx, func() { _ = raw.Close() })
	defer stop()
	if deadline, ok := ctx.Deadline(); ok {
		_ = raw.SetDeadline(deadline)
	}
	if d.proxyURL.Scheme == "https" {
		secured := tls.Client(raw, &tls.Config{ServerName: d.proxyURL.Hostname(), MinVersion: tls.VersionTLS12, RootCAs: d.roots, NextProtos: []string{"http/1.1"}})
		if err = secured.HandshakeContext(ctx); err != nil {
			return nil, classifyProxyError(ctx, err, phaseProxyTLS, d.proxyURL)
		}
		conn = secured
	}
	req := &http.Request{Method: http.MethodConnect, URL: &url.URL{Host: address}, Host: address, Header: make(http.Header)}
	if d.proxyURL.User != nil {
		password, _ := d.proxyURL.User.Password()
		req.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(d.proxyURL.User.Username()+":"+password)))
	}
	if err = req.Write(conn); err != nil {
		return nil, classifyProxyError(ctx, err, phaseProxyTunnel, d.proxyURL)
	}
	reader := bufio.NewReader(conn)
	response, err := http.ReadResponse(reader, req)
	if err != nil {
		return nil, classifyProxyError(ctx, err, phaseProxyTunnel, d.proxyURL)
	}
	if response.StatusCode != http.StatusOK {
		kind := failureProxyStatus
		if response.StatusCode == http.StatusProxyAuthRequired {
			kind = failureProxyAuth
		}
		return nil, &TransportError{kind: kind, phase: phaseProxyTunnel, status: response.StatusCode}
	}
	if !stop() || ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err = conn.SetDeadline(time.Time{}); err != nil {
		return nil, errors.New("proxy tunnel deadline reset failed")
	}
	cleanup = false
	return &bufferedTunnel{Conn: conn, reader: reader}, nil
}

// CONNECT response and the first tunnel bytes may arrive in one TCP packet.
// Keep the buffered reader so the origin TLS handshake never loses those bytes.
type bufferedTunnel struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedTunnel) Read(p []byte) (int, error) { return c.reader.Read(p) }

// A socket deadline can fire just before the context timer is scheduled.
// Preserve the request's cancellation classification in that narrow race.
func proxyContextError(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if deadline, ok := ctx.Deadline(); ok && !time.Now().Before(deadline) {
		return context.DeadlineExceeded
	}
	return nil
}
