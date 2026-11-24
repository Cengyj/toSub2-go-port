package application

import (
	"strings"
	"time"

	"tosub2-go-port/internal/config"
	"tosub2-go-port/internal/protocol"
	"tosub2-go-port/internal/transport"
)

type readOnlyTransportCode string

const (
	readOnlyAuthorizationExpired   readOnlyTransportCode = "authorization_expired"
	readOnlyProxySelectionRequired readOnlyTransportCode = "proxy_selection_required"
	readOnlyTransportUnavailable   readOnlyTransportCode = "transport_unavailable"
)

type readOnlyTransportError struct{ Code readOnlyTransportCode }

func (e *readOnlyTransportError) Error() string { return string(e.Code) }

type readOnlyTransportFactory func(string) (protocol.Doer, func(), error)

func readOnlyProxy(cfg config.Config, g *AccountInsightGrant) string {
	if g.ProxyOverride {
		return g.ProxyURL
	}
	return cfg.ProxyURL
}

func readOnlyTLSProfile(cfg config.Config) string {
	if readOnlyUsesNative(cfg) {
		return transport.NativeProfile
	}
	if cfg.AuthTLSProfile != "" {
		return cfg.AuthTLSProfile
	}
	return "chrome146"
}

func readOnlyUsesNative(cfg config.Config) bool {
	return cfg.CLI.NativeHTTP || cfg.TLSProfile == transport.NativeProfile
}

// openReadOnlyTransport creates a fixed transport for an already committed
// grant. It never selects a pool member, expands a rotating proxy, authenticates,
// refreshes, retries a request or falls back to another TLS identity. Each caller
// retains its own query/probe policy and public error vocabulary.
func openReadOnlyTransport(cfg config.Config, g *AccountInsightGrant, factory readOnlyTransportFactory) (protocol.Doer, func(), *readOnlyTransportError) {
	if g.ExpiresAt != nil && !time.Now().Before(*g.ExpiresAt) {
		return nil, nil, &readOnlyTransportError{Code: readOnlyAuthorizationExpired}
	}
	proxy := readOnlyProxy(cfg, g)
	if !g.ProxyOverride && proxy == "" && len(cfg.ProxyURLs) > 0 || strings.Contains(proxy, "{session}") || strings.Contains(proxy, "{sid}") || strings.Contains(proxy, "{random}") {
		return nil, nil, &readOnlyTransportError{Code: readOnlyProxySelectionRequired}
	}
	var doer protocol.Doer
	var closeTransport func()
	var err error
	if factory != nil {
		doer, closeTransport, err = factory(proxy)
	} else if readOnlyUsesNative(cfg) {
		client, e := transport.NewNativeClient(proxy, transport.NewCookieJar())
		err = e
		if err == nil {
			doer, closeTransport = client, client.Close
		}
	} else {
		client, e := transport.NewCurlCompatibleFingerprintClientExact(readOnlyTLSProfile(cfg), proxy, transport.NewCookieJar())
		err = e
		if err == nil {
			doer, closeTransport = client, client.Close
		}
	}
	if err != nil || doer == nil || closeTransport == nil {
		if closeTransport != nil {
			closeTransport()
		}
		return nil, nil, &readOnlyTransportError{Code: readOnlyTransportUnavailable}
	}
	return doer, closeTransport, nil
}
