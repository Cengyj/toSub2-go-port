package legacyruntime

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"tosub2-go-port/internal/transport"
)

func nativeClient(t *testing.T, options Options) *Client {
	t.Helper()
	c, err := New(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := c.Close(); err != nil {
			t.Error(err)
		}
	})
	return c
}

func TestNativeHTTPPreservesBodiesHeadersCookiesAndManualRedirects(t *testing.T) {
	var calls atomic.Int32
	body := "password=含空格 + &&\nfixture"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path == "/set" {
			got, err := io.ReadAll(r.Body)
			if err != nil || string(got) != body || r.Method != http.MethodPost {
				t.Error("native HTTP body/method changed")
			}
			if values := r.Header.Values("X-Repeat"); len(values) != 2 || values[0] != "first" || values[1] != "second" {
				t.Error("duplicate request headers were lost")
			}
			if r.Header.Get("Cookie") != "oai-did=device; removed=old" {
				t.Error("supplied Cookie field shadowed the restored jar")
			}
			if !strings.Contains(r.UserAgent(), "Chrome/146.") || !strings.Contains(r.Header.Get("Sec-CH-UA"), `v="146"`) {
				t.Error("native TLS profile and HTTP identity diverged")
			}
			w.Header().Add("X-Repeated", "one")
			w.Header().Add("X-Repeated", "two")
			w.Header().Add("Set-Cookie", "session=fixture-session; Path=/; HttpOnly; SameSite=Lax")
			w.Header().Add("Set-Cookie", "removed=; Max-Age=0; Path=/")
			w.Header().Set("Location", "/echo")
			w.WriteHeader(http.StatusFound)
			_, _ = w.Write([]byte("manual redirect"))
			return
		}
		_, _ = w.Write([]byte(r.Header.Get("Cookie")))
	}))
	defer server.Close()
	c := nativeClient(t, Options{})
	jar := transport.NewCookieJar(transport.Cookie{Name: "oai-did", Value: "device", Domain: "127.0.0.1", Path: "/", HostOnly: true}, transport.Cookie{Name: "removed", Value: "old", Domain: "127.0.0.1", Path: "/", HostOnly: true})
	c.SetJar(jar)
	response, err := c.Do(context.Background(), transport.Request{Method: http.MethodPost, URL: server.URL + "/set", Body: []byte(body), Headers: http.Header{"Cookie": {"shadow=stale"}, "User-Agent": {"Chrome/999"}, "X-Repeat": {"first", "second"}}})
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != 302 || response.Text() != "manual redirect" || calls.Load() != 1 || len(response.RawSetCookie) != 2 || len(response.Header.Values("X-Repeated")) != 2 {
		t.Fatal("native response or manual redirect contract changed")
	}
	if jar.Value("removed", server.URL) != "" || jar.Value("session", server.URL) != "fixture-session" {
		t.Fatal("response cookie additions/deletion lost")
	}
	for _, cookie := range jar.Cookies() {
		if cookie.Name == "session" && (!cookie.HostOnly || !cookie.HTTPOnly || cookie.SameSite != "Lax") {
			t.Fatal("cookie metadata changed")
		}
	}
	newJar := transport.NewCookieJar(transport.Cookie{Name: "replacement", Value: "fresh", Domain: "127.0.0.1", Path: "/", HostOnly: true})
	c.SetJar(newJar)
	response, err = c.Do(context.Background(), transport.Request{URL: server.URL + "/echo"})
	if err != nil || response.Text() != "replacement=fresh" {
		t.Fatal("SetJar resurrected stale native session cookies", err)
	}
}

func TestNativeTLSUsesTrustedCertificateAndRealChrome146Hello(t *testing.T) {
	var hellos atomic.Int32
	var sawGrease atomic.Bool
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.UserAgent(), "Chrome/146.") {
			t.Error("UA differs from exact Chrome 146 profile")
		}
		_, _ = w.Write([]byte("trusted TLS"))
	}))
	server.TLS = &tls.Config{GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
		hellos.Add(1)
		for _, suite := range hello.CipherSuites {
			if suite&0x0f0f == 0x0a0a {
				sawGrease.Store(true)
			}
		}
		return nil, nil
	}}
	server.StartTLS()
	defer server.Close()
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	c, err := NewWithRootCAs(context.Background(), Options{Profile: "chrome146"}, roots)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	response, err := c.Do(context.Background(), transport.Request{URL: server.URL})
	if err != nil || response.Text() != "trusted TLS" || c.Profile() != "chrome_146" || hellos.Load() != 1 || !sawGrease.Load() {
		t.Fatalf("native Chrome TLS was not used: profile=%s hellos=%d grease=%t err=%v", c.Profile(), hellos.Load(), sawGrease.Load(), err)
	}
	actual := identity(c.Profile())
	if actual["userAgent"] != c.UserAgent() || actual["acceptLanguage"] != c.AcceptLanguage() || actual["browserMajorVersion"] != 146 {
		t.Fatal("script identity would drift from HTTP identity")
	}
}

func TestNativeTLSRejectsUntrustedCertificateWithoutCredentialLeak(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { t.Error("untrusted request reached HTTP") }))
	defer server.Close()
	c := nativeClient(t, Options{})
	_, err := c.Do(context.Background(), transport.Request{URL: server.URL + "/?token=fixture-private-secret"})
	if err == nil || !strings.Contains(err.Error(), "certificate validation failed") || strings.Contains(err.Error(), "fixture-private") || IsConnectionError(err) {
		t.Fatal("certificate verification or sanitized failure contract changed", err)
	}
}

func TestUnsupportedNativeProfileCannotSilentlySubstituteIdentity(t *testing.T) {
	for _, profile := range []string{"chrome999", "chrome133a", "firefox146"} {
		c, err := New(context.Background(), Options{Profile: profile})
		if c != nil || err == nil {
			if c != nil {
				_ = c.Close()
			}
			t.Fatal("unsupported explicit profile was silently replaced")
		}
	}
}

func TestNativeProxyAuthorizationIsScopedAndFailuresAreSanitized(t *testing.T) {
	var calls atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodConnect || r.Host != "auth.fixture.invalid:443" {
			t.Error("origin was not reached through its configured tunnel")
		}
		expected := "Basic " + base64.StdEncoding.EncodeToString([]byte("fixture-user:fixture-private-password"))
		if r.Header.Get("Proxy-Authorization") != expected {
			t.Error("proxy authorization was not supplied to CONNECT")
		}
		w.WriteHeader(http.StatusProxyAuthRequired)
		_, _ = w.Write([]byte("fixture-private-response"))
	}))
	defer proxy.Close()
	c := nativeClient(t, Options{ProxyURL: strings.Replace(proxy.URL, "://", "://fixture-user:fixture-private-password@", 1)})
	_, err := c.Do(context.Background(), transport.Request{Method: http.MethodPost, URL: "https://auth.fixture.invalid/verify?token=fixture-private-token", Body: []byte("fixture-private-body")})
	if err == nil || !strings.Contains(err.Error(), "HTTP 407") || strings.Contains(err.Error(), "fixture-private") || calls.Load() != 1 || IsConnectionError(err) {
		t.Fatalf("proxy credentials leaked or write/proxy auth was retried: calls=%d err=%v", calls.Load(), err)
	}
}

func TestNativeCanceledWriteIsSingleAttemptAndSessionRemainsUsable(t *testing.T) {
	var writes atomic.Int32
	entered := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			writes.Add(1)
			_, _ = io.Copy(io.Discard, r.Body)
			close(entered)
			<-r.Context().Done()
			return
		}
		_, _ = w.Write([]byte("next request"))
	}))
	defer server.Close()
	c := nativeClient(t, Options{})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := c.Do(ctx, transport.Request{Method: http.MethodPost, URL: server.URL, Body: []byte("fixture-password")})
	if !errors.Is(err, context.DeadlineExceeded) || writes.Load() != 1 {
		t.Fatal("canceled write was not one attempt", err)
	}
	select {
	case <-entered:
	default:
		t.Fatal("test canceled before the write reached HTTP")
	}
	response, err := c.Do(context.Background(), transport.Request{URL: server.URL})
	if err != nil || response.Text() != "next request" {
		t.Fatal("canceled request killed the unrelated session lifetime", err)
	}
}

func TestNativeWriteIsNotReplayedAfterPeerClosesAcceptedRequest(t *testing.T) {
	var writes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			writes.Add(1)
			_, _ = io.Copy(io.Discard, r.Body)
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			_ = conn.Close()
			return
		}
		_, _ = w.Write([]byte("warm connection"))
	}))
	defer server.Close()
	c := nativeClient(t, Options{})
	if _, err := c.Do(context.Background(), transport.Request{URL: server.URL}); err != nil {
		t.Fatal(err)
	}
	_, err := c.Do(context.Background(), transport.Request{Method: http.MethodPost, URL: server.URL, Body: []byte("fixture-otp"), Headers: http.Header{"Idempotency-Key": {"fixture"}}})
	if err == nil || writes.Load() != 1 {
		t.Fatalf("accepted write was replayed: writes=%d err=%v", writes.Load(), err)
	}
}

func TestNativeCloseWaitsForActiveRequestAndOwnerCancellationStopsSession(t *testing.T) {
	entered := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(entered)
		<-r.Context().Done()
	}))
	defer server.Close()
	c := nativeClient(t, Options{})
	result := make(chan error, 1)
	go func() { _, err := c.Do(context.Background(), transport.Request{URL: server.URL}); result <- err }()
	<-entered
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("Close did not cancel the active operation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close returned before active request released its operation gate")
	}
	if _, err := c.Do(context.Background(), transport.Request{URL: server.URL}); !errors.Is(err, ErrClosed) {
		t.Fatal("closed session was silently resumed", err)
	}
	owner, cancel := context.WithCancel(context.Background())
	owned, err := New(owner, Options{})
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	defer owned.Close()
	if _, err := owned.Do(context.Background(), transport.Request{URL: server.URL}); !errors.Is(err, context.Canceled) {
		t.Fatal("transport outlived its owner", err)
	}
}

func TestNativeQueuedCancellationAndConcurrentResponses(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"path": r.URL.RequestURI()})
	}))
	defer server.Close()
	c := nativeClient(t, Options{})
	if err := c.acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	_, err := c.Do(ctx, transport.Request{URL: server.URL})
	cancel()
	c.release()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("queued request ignored caller deadline", err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			path := "/echo?request=" + string(rune('a'+i))
			response, err := c.Do(context.Background(), transport.Request{URL: server.URL + path})
			if err != nil {
				t.Error(err)
				return
			}
			var echo map[string]string
			if json.Unmarshal(response.Body, &echo) != nil || echo["path"] != path {
				t.Error("response was delivered to another caller")
			}
		}(i)
	}
	wg.Wait()
}

func TestNativeResetPreservesOnlyDeviceAndClearsChallengeMaterial(t *testing.T) {
	c := nativeClient(t, Options{})
	jar := transport.NewCookieJar(transport.Cookie{Name: "oai-did", Value: "device", Domain: "fixture.invalid", Path: "/"}, transport.Cookie{Name: "session", Value: "private", Domain: "fixture.invalid", Path: "/"})
	c.SetJar(jar)
	c.challenges["https://fixture.invalid/"] = []byte("challenge")
	if err := c.ResetSession(context.Background()); err != nil {
		t.Fatal(err)
	}
	if jar.HeaderFor("https://fixture.invalid/") != "oai-did=device" || len(c.challenges) != 0 {
		t.Fatal("reset retained authentication cookies or old challenge body")
	}
}

func TestNativeRefusedConnectionCanBeClassifiedWithoutURLLeak(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	_ = listener.Close()
	c := nativeClient(t, Options{})
	_, err = c.Do(context.Background(), transport.Request{URL: "http://" + address + "/?token=fixture-private-token"})
	if err == nil || !IsConnectionError(err) || strings.Contains(err.Error(), "fixture-private") {
		t.Fatal("connection failure was unclassified or included request secrets", err)
	}
}
