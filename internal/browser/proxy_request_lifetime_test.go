package browser

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"
	"time"

	"tosub2-go-port/internal/transport"
)

func TestSOCKSBridgeCanceledCONNECTStopsOnlyItsPendingHandshake(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan struct{})
	stopped := make(chan error, 1)
	go func() {
		c, err := listener.Accept()
		if err != nil {
			stopped <- err
			return
		}
		defer c.Close()
		_ = c.SetDeadline(time.Now().Add(3 * time.Second))
		header := make([]byte, 2)
		if _, err = io.ReadFull(c, header); err != nil {
			stopped <- err
			return
		}
		methods := make([]byte, int(header[1]))
		if _, err = io.ReadFull(c, methods); err != nil {
			stopped <- err
			return
		}
		close(accepted)
		// Never reply to SOCKS negotiation. Closing the individual CONNECT
		// socket must abort this upstream wait while the browser stays live.
		_, err = c.Read(make([]byte, 1))
		stopped <- err
	}()
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	b, err := newProxyBridge(parent, "socks5h://"+listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	u, _ := url.Parse(b.endpoint)
	client, err := net.DialTimeout("tcp", u.Host, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	auth := base64.StdEncoding.EncodeToString([]byte(b.username + ":" + b.password))
	if _, err = fmt.Fprintf(client, "CONNECT authorized.fixture:443 HTTP/1.1\r\nHost: authorized.fixture:443\r\nProxy-Authorization: Basic %s\r\n\r\n", auth); err != nil {
		t.Fatal(err)
	}
	select {
	case <-accepted:
	case <-time.After(time.Second):
		t.Fatal("upstream handshake did not start")
	}
	_ = client.Close()
	select {
	case err := <-stopped:
		if err != io.EOF {
			t.Fatalf("pending SOCKS handshake was not canceled cleanly: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("individual request cancellation left SOCKS handshake running")
	}
	if parent.Err() != nil {
		t.Fatal("request cancellation canceled the browser session")
	}
	check, err := net.DialTimeout("tcp", u.Host, time.Second)
	if err != nil {
		t.Fatal("request cancellation closed the shared bridge:", err)
	}
	_ = check.Close()
}

func TestChromiumSOCKSRequestBudgetCanExceedThirtySeconds(t *testing.T) {
	if os.Getenv("TOSUB2_BROWSER_TEST") != "1" || os.Getenv("TOSUB2_LONG_TRANSPORT_TEST") != "1" {
		t.Skip("set TOSUB2_BROWSER_TEST=1 and TOSUB2_LONG_TRANSPORT_TEST=1 for a real 31-second Chrome/SOCKS request")
	}
	path := os.Getenv("CHROME_PATH")
	if path == "" {
		path = DefaultChrome()
	}
	if path == "" {
		t.Skip("Chrome not installed; set CHROME_PATH")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/long" {
			w.WriteHeader(204)
			return
		}
		timer := time.NewTimer(31 * time.Second)
		defer timer.Stop()
		select {
		case <-r.Context().Done():
			return
		case <-timer.C:
		}
		fmt.Fprint(w, "browser-budget-honored")
	}))
	defer server.Close()
	origin, _ := url.Parse(server.URL)
	proxyURL, _ := socksFixtureWithTimeout(t, origin.Host, 40*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	pool := NewManager(path, 1)
	defer pool.Close()
	s, err := pool.Start(ctx, proxyURL, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	response, err := NewTransport(s, nil).Do(ctx, transport.Request{URL: "http://authorized.fixture/long", Timeout: 35 * time.Second})
	if err != nil || response.Text() != "browser-budget-honored" {
		t.Fatalf("SOCKS bridge truncated configured browser request: %v", err)
	}
}
