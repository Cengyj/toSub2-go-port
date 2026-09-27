package transport

import (
	"context"
	"errors"
	"net"
	"net/url"
	"time"

	"golang.org/x/net/proxy"
)

type socksDialer struct {
	inner    proxy.ContextDialer
	localDNS bool
	resolver *net.Resolver
}

func newSOCKSDialer(u *url.URL, timeout time.Duration, localAddr *net.TCPAddr) (*socksDialer, error) {
	var auth *proxy.Auth
	if u.User != nil {
		password, _ := u.User.Password()
		auth = &proxy.Auth{User: u.User.Username(), Password: password}
	}
	forward := &socksProxyForward{base: &net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second, LocalAddr: localAddr}, proxyURL: u}
	d, e := proxy.SOCKS5("tcp", u.Host, auth, forward)
	if e != nil {
		return nil, errors.New("cannot configure SOCKS5 proxy")
	}
	cd, ok := d.(proxy.ContextDialer)
	if !ok {
		return nil, errors.New("SOCKS5 dialer does not support cancellation")
	}
	return &socksDialer{inner: cd, localDNS: u.Scheme == "socks5", resolver: net.DefaultResolver}, nil
}

// Classify only the connection to the SOCKS endpoint here. Target DNS and
// SOCKS negotiation errors are separate and must not be called proxy socket
// failures or inherit a misleading loopback/container hint.
type socksProxyForward struct {
	base     *net.Dialer
	proxyURL *url.URL
}

func (d *socksProxyForward) Dial(network, address string) (net.Conn, error) {
	return d.DialContext(context.Background(), network, address)
}

func (d *socksProxyForward) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	conn, err := d.base.DialContext(ctx, network, address)
	if err != nil {
		return nil, classifyProxyError(ctx, err, phaseProxyConnect, d.proxyURL)
	}
	return conn, nil
}
func (d *socksDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, e := net.SplitHostPort(address)
	if e != nil {
		return nil, errors.New("invalid SOCKS5 target")
	}
	if d.localDNS && net.ParseIP(host) == nil {
		addresses, e := d.resolver.LookupIPAddr(ctx, host)
		if e != nil {
			return nil, e
		}
		for _, ip := range addresses {
			if network == "tcp4" && ip.IP.To4() == nil {
				continue
			}
			if network == "tcp6" && ip.IP.To4() != nil {
				continue
			}
			return d.inner.DialContext(ctx, network, net.JoinHostPort(ip.IP.String(), port))
		}
		return nil, errors.New("SOCKS5 target DNS returned no addresses")
	}
	return d.inner.DialContext(ctx, network, address)
}
