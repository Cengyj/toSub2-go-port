package browser

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"tosub2-go-port/internal/transport"
)

func socksFixture(t *testing.T, upstream string) (string, *sync.Map) {
	return socksFixtureWithTimeout(t, upstream, 20*time.Second)
}

func socksFixtureWithTimeout(t *testing.T, upstream string, timeout time.Duration) (string, *sync.Map) {
	t.Helper()
	listener, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	var connections sync.Map
	var names sync.Map
	t.Cleanup(func() {
		_ = listener.Close()
		connections.Range(func(k, v any) bool { _ = k.(net.Conn).Close(); return true })
	})
	go func() {
		for {
			c, e := listener.Accept()
			if e != nil {
				return
			}
			connections.Store(c, true)
			go func() {
				var e error
				defer func() { _ = c.Close(); connections.Delete(c) }()
				_ = c.SetDeadline(time.Now().Add(timeout))
				head := make([]byte, 2)
				if _, e = io.ReadFull(c, head); e != nil {
					return
				}
				methods := make([]byte, int(head[1]))
				if _, e = io.ReadFull(c, methods); e != nil {
					return
				}
				_, _ = c.Write([]byte{5, 2})
				if _, e = io.ReadFull(c, head); e != nil {
					return
				}
				user := make([]byte, int(head[1]))
				_, _ = io.ReadFull(c, user)
				size := make([]byte, 1)
				_, _ = io.ReadFull(c, size)
				password := make([]byte, int(size[0]))
				_, _ = io.ReadFull(c, password)
				if string(user) != "fixture" || string(password) != "fixture-secret" {
					_, _ = c.Write([]byte{1, 1})
					return
				}
				_, _ = c.Write([]byte{1, 0})
				req := make([]byte, 4)
				if _, e = io.ReadFull(c, req); e != nil {
					return
				}
				host := ""
				switch req[3] {
				case 1:
					addr := make([]byte, 4)
					_, _ = io.ReadFull(c, addr)
					host = net.IP(addr).String()
				case 4:
					addr := make([]byte, 16)
					_, _ = io.ReadFull(c, addr)
					host = net.IP(addr).String()
				case 3:
					_, _ = io.ReadFull(c, size)
					addr := make([]byte, int(size[0]))
					_, _ = io.ReadFull(c, addr)
					host = string(addr)
				default:
					return
				}
				port := make([]byte, 2)
				_, _ = io.ReadFull(c, port)
				names.Store(host, int(binary.BigEndian.Uint16(port)))
				remote, e := net.DialTimeout("tcp", upstream, 5*time.Second)
				if e != nil {
					_, _ = c.Write([]byte{5, 1, 0, 1, 0, 0, 0, 0, 0, 0})
					return
				}
				defer remote.Close()
				_, _ = c.Write([]byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 0})
				done := make(chan struct{}, 1)
				go func() { _, _ = io.Copy(remote, c); done <- struct{}{} }()
				_, _ = io.Copy(c, remote)
				_ = c.Close()
				_ = remote.Close()
				<-done
			}()
		}
	}()
	return "socks5h://fixture:fixture-secret@" + listener.Addr().String(), &names
}

func TestSOCKSBridgeCONNECTAuthenticationDNSAndClose(t *testing.T) {
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Proxy-Authorization") != "" {
			t.Error("bridge credentials leaked to origin")
		}
		fmt.Fprint(w, "tls-through-socks")
	}))
	defer target.Close()
	address, _ := url.Parse(target.URL)
	proxyURL, names := socksFixture(t, address.Host)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bridge, e := newProxyBridge(ctx, proxyURL)
	if e != nil {
		t.Fatal(e)
	}
	defer bridge.Close()
	proxyAddress, _ := url.Parse(bridge.endpoint)
	proxyAddress.User = url.UserPassword(bridge.username, bridge.password)
	tr := &http.Transport{Proxy: http.ProxyURL(proxyAddress), TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, Timeout: 10 * time.Second}
	response, e := client.Get("https://remote.fixture" + ":" + address.Port() + "/authorized")
	if e != nil {
		t.Fatal(e)
	}
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if string(body) != "tls-through-socks" {
		t.Fatal("CONNECT did not preserve TLS")
	}
	if _, ok := names.Load("remote.fixture"); !ok {
		t.Fatal("socks5h failed to preserve remote DNS name")
	}
	bridge.Close()
	conn, e := net.DialTimeout("tcp", proxyAddress.Host, 200*time.Millisecond)
	if e == nil {
		conn.Close()
		t.Fatal("closed bridge still accepts connections")
	}
}

func TestChromiumAuthenticatedSOCKS5Bridge(t *testing.T) {
	if os.Getenv("TOSUB2_BROWSER_TEST") != "1" {
		t.Skip("set TOSUB2_BROWSER_TEST=1 for authenticated SOCKS5 Chromium integration")
	}
	path := os.Getenv("CHROME_PATH")
	if path == "" {
		path = DefaultChrome()
	}
	if path == "" {
		t.Skip("Chromium not installed")
	}
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "chromium-socks-authorized") }))
	defer target.Close()
	u, _ := url.Parse(target.URL)
	proxyURL, names := socksFixture(t, u.Host)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	m := NewManager(path, 1)
	defer m.Close()
	s, e := m.Start(ctx, proxyURL, "", "", "")
	if e != nil {
		t.Fatal(e)
	}
	response, e := NewTransport(s, nil).Do(ctx, transport.Request{URL: "http://remote.fixture/authorized"})
	if e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(string(response.Body), "chromium-socks-authorized") {
		t.Fatal("Chromium SOCKS response missing")
	}
	if _, ok := names.Load("remote.fixture"); !ok {
		t.Fatal("SOCKS remote DNS not observed")
	}
}
