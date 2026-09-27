package browser

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"tosub2-go-port/internal/transport"
)

func TestChromiumSOCKS5UsesLocalDNSAndPreservesCredentials(t *testing.T) {
	if os.Getenv("TOSUB2_BROWSER_TEST") != "1" {
		t.Skip("set TOSUB2_BROWSER_TEST=1 for real Chromium SOCKS local DNS")
	}
	path := os.Getenv("CHROME_PATH")
	if path == "" {
		path = DefaultChrome()
	}
	if path == "" {
		t.Skip("Chromium not installed")
	}
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Proxy-Authorization") != "" {
			t.Error("proxy credentials reached origin")
		}
		fmt.Fprint(w, "local-dns-authorized")
	}))
	defer target.Close()
	upstream, _ := url.Parse(target.URL)
	proxyURL, names := socksFixture(t, upstream.Host)
	proxyURL = strings.Replace(proxyURL, "socks5h:", "socks5:", 1)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	manager := NewManager(path, 1)
	defer manager.Close()
	session, err := manager.Start(ctx, proxyURL, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	response, err := NewTransport(session, nil).Do(ctx, transport.Request{URL: "http://localhost:" + upstream.Port() + "/authorized"})
	if err != nil || string(response.Body) != "local-dns-authorized" {
		t.Fatalf("SOCKS local DNS: %v", err)
	}
	if _, found := names.Load("localhost"); found {
		t.Fatal("socks5 forwarded hostname for remote DNS")
	}
	_, ipv4 := names.Load("127.0.0.1")
	_, ipv6 := names.Load("::1")
	if !ipv4 && !ipv6 {
		t.Fatal("SOCKS proxy did not receive locally resolved address")
	}
	bridgeEndpoint := session.bridge.endpoint
	if err = session.Close(); err != nil {
		t.Fatal(err)
	}
	client := http.Client{Timeout: time.Second}
	if response, err := client.Get(bridgeEndpoint); err == nil {
		response.Body.Close()
		t.Fatal("closed proxy session left the local bridge accepting requests")
	}
}

func TestChromiumProxySessionReplacementHasFreshCookiesAndProfile(t *testing.T) {
	original, ctx := integrationSession(t)
	manager := original.owner
	_ = original.Close()
	var first, second atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Proxy-Authorization")
		wantFirst := "Basic " + base64.StdEncoding.EncodeToString([]byte("user-sid-one:fixture"))
		wantSecond := "Basic " + base64.StdEncoding.EncodeToString([]byte("user-sid-two:fixture"))
		if auth != wantFirst && auth != wantSecond {
			w.Header().Set("Proxy-Authenticate", `Basic realm="session-fixture"`)
			w.WriteHeader(407)
			return
		}
		if auth == wantFirst {
			first.Add(1)
			http.SetCookie(w, &http.Cookie{Name: "session_cookie", Value: "old-session", Path: "/"})
		} else {
			second.Add(1)
			if r.Header.Get("Cookie") != "" {
				t.Error("cookie crossed proxy/browser session boundary")
			}
		}
		fmt.Fprint(w, "authorized")
	}))
	defer proxy.Close()
	u, _ := url.Parse(proxy.URL)
	u.User = url.UserPassword("user-sid-one", "fixture")
	a, err := manager.Start(ctx, u.String(), "", "", "first-device")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = NewTransport(a, nil).Do(ctx, transport.Request{URL: "http://session.fixture/auth"}); err != nil {
		t.Fatal(err)
	}
	oldDirectory := a.Dir
	if err = a.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(oldDirectory); !os.IsNotExist(err) {
		t.Fatal("old profile directory survived session rotation")
	}
	if _, err = a.Evaluate(ctx, "1"); !errors.Is(err, ErrClosed) {
		t.Fatal("old CDP session survived rotation")
	}
	u.User = url.UserPassword("user-sid-two", "fixture")
	b, err := manager.Start(ctx, u.String(), "", "", "second-device")
	if err != nil {
		t.Fatal(err)
	}
	if b.Dir == oldDirectory {
		t.Fatal("new proxy reused old browser profile")
	}
	if _, err = NewTransport(b, nil).Do(ctx, transport.Request{URL: "http://session.fixture/auth"}); err != nil {
		t.Fatal(err)
	}
	if first.Load() < 1 || second.Load() < 1 {
		t.Fatal("both proxy session identities were not exercised")
	}
}

func TestChromiumParentCancellationTerminatesProcessAndProfile(t *testing.T) {
	existing, ctx := integrationSession(t)
	manager := existing.owner
	_ = existing.Close()
	parent, cancel := context.WithCancel(ctx)
	session, err := manager.Start(parent, "", "", "", "")
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	directory := session.Dir
	done := make(chan error, 1)
	go func() { _, err := session.Evaluate(parent, `new Promise(()=>{})`); done <- err }()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("cancellation left a CDP call blocked")
	}
	if err = session.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-session.processDone:
	default:
		t.Fatal("Chromium process survived cancellation")
	}
	if _, err = os.Stat(directory); !os.IsNotExist(err) {
		t.Fatal("canceled browser profile survived")
	}
}

func TestChromiumRejectsUntrustedHTTPSProxyBeforeSendingCredentials(t *testing.T) {
	existing, ctx := integrationSession(t)
	manager := existing.owner
	_ = existing.Close()
	var received atomic.Int32
	proxy := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { received.Add(1); fmt.Fprint(w, "must-not-be-trusted") }))
	defer proxy.Close()
	u, _ := url.Parse(proxy.URL)
	u.User = url.UserPassword("fixture-user", "fixture-password")
	session, err := manager.Start(ctx, u.String(), "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	requestCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err = NewTransport(session, nil).Do(requestCtx, transport.Request{URL: "http://authorized.fixture/proxy-test"}); err == nil {
		t.Fatal("Chromium accepted untrusted HTTPS proxy certificate")
	}
	if received.Load() != 0 {
		t.Fatal("credentials/request reached HTTPS proxy before certificate verification")
	}
}

func TestChromiumOriginAuthNeverReceivesProxyCredentials(t *testing.T) {
	existing, ctx := integrationSession(t)
	manager := existing.owner
	_ = existing.Close()
	expected := "Basic " + base64.StdEncoding.EncodeToString([]byte("proxy-user:proxy-password"))
	var originAuthRequests, leaked atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Proxy-Authorization") != expected {
			w.Header().Set("Proxy-Authenticate", `Basic realm="proxy-fixture"`)
			w.WriteHeader(407)
			return
		}
		originAuthRequests.Add(1)
		if r.Header.Get("Authorization") != "" {
			leaked.Add(1)
		}
		w.Header().Set("WWW-Authenticate", `Basic realm="origin-fixture"`)
		w.WriteHeader(401)
	}))
	defer proxy.Close()
	u, _ := url.Parse(proxy.URL)
	u.User = url.UserPassword("proxy-user", "proxy-password")
	session, err := manager.Start(ctx, u.String(), "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	requestCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	_, _ = NewTransport(session, nil).Do(requestCtx, transport.Request{URL: "http://authorized.fixture/origin-auth"})
	if originAuthRequests.Load() == 0 {
		t.Fatal("origin authentication challenge not exercised")
	}
	if leaked.Load() != 0 {
		t.Fatal("proxy credentials were sent to origin authentication challenge")
	}
}
