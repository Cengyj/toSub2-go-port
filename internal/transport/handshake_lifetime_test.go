package transport

import (
	"context"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type stalledRedialListener struct {
	net.Listener
	stall    atomic.Bool
	accepted chan net.Conn
	stopped  chan struct{}
	mu       sync.Mutex
	held     []net.Conn
	stopOnce sync.Once
}

func (l *stalledRedialListener) Accept() (net.Conn, error) {
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		if !l.stall.Load() {
			return conn, nil
		}
		l.mu.Lock()
		l.held = append(l.held, conn)
		l.mu.Unlock()
		l.accepted <- conn
		go func() {
			defer conn.Close()
			_, _ = io.Copy(io.Discard, conn)
			l.stopOnce.Do(func() { close(l.stopped) })
		}()
	}
}
func (l *stalledRedialListener) Close() error {
	err := l.Listener.Close()
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, c := range l.held {
		_ = c.Close()
	}
	return err
}

func TestFingerprintHTTP2RedialCancellationClosesHandshake(t *testing.T) {
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 2 {
			t.Errorf("fixture must negotiate real HTTP/2, got %s", r.Proto)
		}
		_, _ = io.WriteString(w, "authorized local h2 fixture")
	}))
	listener := &stalledRedialListener{Listener: server.Listener, accepted: make(chan net.Conn, 1), stopped: make(chan struct{})}
	server.Listener = listener
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()
	client := lifetimeClient(t, "fingerprint", NewCookieJar(), server)
	if _, err := client.Do(context.Background(), Request{URL: server.URL, Timeout: time.Second}); err != nil {
		t.Fatal(err)
	}
	client.Close()
	listener.stall.Store(true)
	done := make(chan error, 1)
	go func() {
		_, err := client.Do(context.Background(), Request{URL: server.URL, Timeout: 120 * time.Millisecond})
		done <- err
	}()
	select {
	case <-listener.accepted:
	case <-time.After(time.Second):
		t.Fatal("second HTTP/2 connection was not attempted")
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("request deadline classification: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("HTTP/2 redial ignored the request deadline")
	}
	select {
	case <-listener.stopped:
	case <-time.After(300 * time.Millisecond):
		t.Fatal("timed-out HTTP/2 request retained its stalled TLS handshake socket")
	}
}

func TestNativeTLSCancellationClosesDetachedHandshake(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan net.Conn, 1)
	stopped := make(chan struct{})
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		accepted <- conn
		defer conn.Close()
		_, _ = io.Copy(io.Discard, conn)
		close(stopped)
	}()
	client, err := NewNativeClient("", NewCookieJar())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	_, err = client.Do(context.Background(), Request{URL: "https://" + listener.Addr().String(), Timeout: 100 * time.Millisecond})
	conn := <-accepted
	defer conn.Close()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("request deadline classification: %v", err)
	}
	select {
	case <-stopped:
	case <-time.After(300 * time.Millisecond):
		t.Fatal("native timeout retained its detached TLS handshake socket")
	}
}

func TestNativeHTTP2CancellationDoesNotCloseAnotherStream(t *testing.T) {
	firstStarted := make(chan struct{})
	secondStarted := make(chan struct{})
	finishSecond := make(chan struct{})
	var connections atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 2 {
			t.Errorf("expected real HTTP/2, got %s", r.Proto)
		}
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		if r.URL.Path == "/first" {
			close(firstStarted)
			<-r.Context().Done()
			return
		}
		close(secondStarted)
		select {
		case <-r.Context().Done():
		case <-finishSecond:
			_, _ = io.WriteString(w, "independent stream")
		}
	}))
	server.EnableHTTP2 = true
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}
	server.StartTLS()
	defer server.Close()
	client := lifetimeClient(t, "native", NewCookieJar(), server).(*Client)
	client.HTTP.Transport.(*http.Transport).ForceAttemptHTTP2 = true
	firstContext, cancelFirst := context.WithCancel(context.Background())
	defer cancelFirst()
	firstDone := make(chan error, 1)
	go func() {
		_, err := client.Do(firstContext, Request{URL: server.URL + "/first", Timeout: 3 * time.Second})
		firstDone <- err
	}()
	select {
	case <-firstStarted:
	case <-time.After(time.Second):
		t.Fatal("first stream did not start")
	}
	secondDone := make(chan error, 1)
	go func() {
		response, err := client.Do(context.Background(), Request{URL: server.URL + "/second", Timeout: 3 * time.Second})
		if err == nil && response.Text() != "independent stream" {
			err = errors.New("second stream body was lost")
		}
		secondDone <- err
	}()
	select {
	case <-secondStarted:
	case <-time.After(time.Second):
		t.Fatal("second stream did not start")
	}
	cancelFirst()
	if err := <-firstDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("first stream cancellation: %v", err)
	}
	close(finishSecond)
	if err := <-secondDone; err != nil {
		t.Fatalf("canceling first stream disrupted second: %v", err)
	}
	if got := connections.Load(); got != 1 {
		t.Fatalf("test requires two streams sharing one physical connection, got %d", got)
	}
}

func TestFingerprintWaitingCancellationPreservesOwnerAndHTTP2Reuse(t *testing.T) {
	started := make(chan struct{})
	finish := make(chan struct{})
	var connections, requests atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 2 {
			t.Errorf("expected real HTTP/2, got %s", r.Proto)
		}
		requests.Add(1)
		if r.URL.Path == "/hold" {
			close(started)
			select {
			case <-r.Context().Done():
				return
			case <-finish:
			}
		}
		_, _ = io.WriteString(w, "pooled response")
	}))
	server.EnableHTTP2 = true
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}
	server.StartTLS()
	defer server.Close()
	client := lifetimeClient(t, "fingerprint", NewCookieJar(), server)
	firstDone := make(chan error, 1)
	go func() {
		_, err := client.Do(context.Background(), Request{URL: server.URL + "/hold", Timeout: 2 * time.Second})
		firstDone <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("owner did not start")
	}
	_, err := client.Do(context.Background(), Request{URL: server.URL + "/waiting", Timeout: 60 * time.Millisecond})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiting request deadline: %v", err)
	}
	if requests.Load() != 1 {
		t.Fatal("canceled waiter issued an HTTP request")
	}
	select {
	case err := <-firstDone:
		t.Fatalf("waiter cancellation disrupted owner: %v", err)
	default:
	}
	close(finish)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(context.Background(), Request{URL: server.URL + "/next", Timeout: time.Second})
	if err != nil || response.Text() != "pooled response" {
		t.Fatalf("next request failed: %v", err)
	}
	if got := connections.Load(); got != 1 {
		t.Fatalf("HTTP/2 pooled connection was not reused: %d connections", got)
	}
}

func TestFingerprintCancellationIsIndependentAcrossClients(t *testing.T) {
	firstStarted, secondStarted, finishSecond := make(chan struct{}), make(chan struct{}), make(chan struct{})
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 2 {
			t.Errorf("expected real HTTP/2, got %s", r.Proto)
		}
		if r.URL.Path == "/first" {
			close(firstStarted)
			<-r.Context().Done()
			return
		}
		close(secondStarted)
		select {
		case <-r.Context().Done():
			return
		case <-finishSecond:
		}
		_, _ = io.WriteString(w, "other job")
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()
	first := lifetimeClient(t, "fingerprint", NewCookieJar(), server)
	second := lifetimeClient(t, "fingerprint", NewCookieJar(), server)
	firstContext, cancelFirst := context.WithCancel(context.Background())
	defer cancelFirst()
	firstDone, secondDone := make(chan error, 1), make(chan error, 1)
	go func() {
		_, err := first.Do(firstContext, Request{URL: server.URL + "/first", Timeout: 3 * time.Second})
		firstDone <- err
	}()
	select {
	case <-firstStarted:
	case <-time.After(time.Second):
		t.Fatal("first client did not start")
	}
	go func() {
		_, err := second.Do(context.Background(), Request{URL: server.URL + "/second", Timeout: 3 * time.Second})
		secondDone <- err
	}()
	select {
	case <-secondStarted:
	case <-time.After(time.Second):
		t.Fatal("second client was serialized behind the first")
	}
	cancelFirst()
	if err := <-firstDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("first client cancellation: %v", err)
	}
	close(finishSecond)
	if err := <-secondDone; err != nil {
		t.Fatalf("first client cancellation disrupted second: %v", err)
	}
}

func TestNativeHTTPSProxyCancellationClosesOriginHandshake(t *testing.T) {
	stopped := make(chan struct{})
	proxyServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			t.Error("expected proxy CONNECT")
			return
		}
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		_, _ = rw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
		_ = rw.Flush()
		_, _ = io.Copy(io.Discard, conn)
		close(stopped)
	}))
	defer proxyServer.Close()
	client, err := NewNativeClient(proxyServer.URL, NewCookieJar())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	roots := x509.NewCertPool()
	roots.AddCert(proxyServer.Certificate())
	client.HTTP.Transport.(*http.Transport).TLSClientConfig.RootCAs = roots
	_, err = client.Do(context.Background(), Request{URL: "https://authorized-fixture.invalid", Timeout: 120 * time.Millisecond})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("origin handshake deadline: %v", err)
	}
	select {
	case <-stopped:
	case <-time.After(300 * time.Millisecond):
		t.Fatal("origin handshake retained its HTTPS proxy socket")
	}
}
