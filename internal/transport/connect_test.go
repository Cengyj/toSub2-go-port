package transport

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestHTTPSProxyTrustAuthenticationAndBufferedTunnel(t *testing.T) {
	expected := "Basic " + base64.StdEncoding.EncodeToString([]byte("authorized:fixture-password"))
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Proxy-Authorization") != expected {
			w.Header().Set("Proxy-Authenticate", `Basic realm="fixture"`)
			w.WriteHeader(407)
			return
		}
		if r.Method != http.MethodConnect || r.Host != "authorized.example:443" {
			w.WriteHeader(400)
			return
		}
		conn, writer, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		fmt.Fprint(writer, "HTTP/1.1 200 Connection Established\r\n\r\nfixture-tunnel")
		_ = writer.Flush()
	}))
	defer server.Close()
	proxyURL, _ := url.Parse(server.URL)
	proxyURL.User = url.UserPassword("authorized", "fixture-password")
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	dialer := &connectProxyDialer{proxyURL: proxyURL, timeout: time.Second, roots: roots}
	conn, err := dialer.DialContext(context.Background(), "tcp", "authorized.example:443")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(conn)
	_ = conn.Close()
	if err != nil || string(body) != "fixture-tunnel" {
		t.Fatalf("buffered tunnel: %v %q", err, body)
	}
	untrusted := *dialer
	untrusted.roots = nil
	if conn, err = untrusted.DialContext(context.Background(), "tcp", "authorized.example:443"); err == nil {
		conn.Close()
		t.Fatal("untrusted HTTPS proxy certificate accepted")
	}
	wrongURL := *proxyURL
	wrongURL.User = url.UserPassword("wrong", "private-password")
	wrong := *dialer
	wrong.proxyURL = &wrongURL
	if conn, err = wrong.DialContext(context.Background(), "tcp", "authorized.example:443"); err == nil {
		conn.Close()
		t.Fatal("proxy accepted missing credentials")
	} else if strings.Contains(err.Error(), "private-password") {
		t.Fatal("proxy password leaked")
	}
}

func TestProxyTLSAndCONNECTCancelCloseConnection(t *testing.T) {
	for _, scheme := range []string{"https", "http"} {
		t.Run(scheme, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			closed := make(chan struct{})
			var wg sync.WaitGroup
			wg.Add(1)
			go func() {
				defer wg.Done()
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				defer conn.Close()
				_, _ = io.Copy(io.Discard, conn)
				close(closed)
			}()
			proxyURL, _ := url.Parse(scheme + "://" + listener.Addr().String())
			ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
			defer cancel()
			start := time.Now()
			_, err = (&connectProxyDialer{proxyURL: proxyURL, timeout: time.Minute}).DialContext(ctx, "tcp", "fixture.example:443")
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("context deadline lost: %v", err)
			}
			if time.Since(start) > time.Second {
				t.Fatal("proxy handshake ignored cancellation")
			}
			select {
			case <-closed:
			case <-time.After(time.Second):
				t.Fatal("canceled proxy connection remained open")
			}
			wg.Wait()
		})
	}
}
