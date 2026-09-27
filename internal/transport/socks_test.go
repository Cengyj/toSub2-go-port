package transport

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"net/url"
	"testing"
	"time"
)

func TestSOCKS5AndSOCKS5hDNSSemantics(t *testing.T) {
	for _, scheme := range []string{"socks5", "socks5h"} {
		t.Run(scheme, func(t *testing.T) {
			listener, e := net.Listen("tcp", "127.0.0.1:0")
			if e != nil {
				t.Fatal(e)
			}
			defer listener.Close()
			types := make(chan byte, 1)
			go func() {
				c, e := listener.Accept()
				if e != nil {
					return
				}
				defer c.Close()
				head := make([]byte, 2)
				if _, e = io.ReadFull(c, head); e != nil {
					return
				}
				methods := make([]byte, int(head[1]))
				io.ReadFull(c, methods)
				c.Write([]byte{5, 0})
				req := make([]byte, 4)
				if _, e = io.ReadFull(c, req); e != nil {
					return
				}
				types <- req[3]
				var n int
				switch req[3] {
				case 1:
					n = 4
				case 4:
					n = 16
				case 3:
					lenByte := make([]byte, 1)
					io.ReadFull(c, lenByte)
					n = int(lenByte[0])
				}
				target := make([]byte, n+2)
				io.ReadFull(c, target)
				_ = binary.BigEndian.Uint16(target[n:])
				c.Write([]byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 1})
			}()
			u, _ := url.Parse(scheme + "://" + listener.Addr().String())
			d, e := newSOCKSDialer(u, time.Second, nil)
			if e != nil {
				t.Fatal(e)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			c, e := d.DialContext(ctx, "tcp", "localhost:443")
			if e != nil {
				t.Fatal(e)
			}
			c.Close()
			atyp := <-types
			if scheme == "socks5h" && atyp != 3 {
				t.Fatalf("remote DNS sent IP type %d", atyp)
			}
			if scheme == "socks5" && atyp == 3 {
				t.Fatal("local DNS sent domain to proxy")
			}
		})
	}
}
func TestSOCKSContextCancelClosesHandshakeConnection(t *testing.T) {
	listener, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	defer listener.Close()
	closed := make(chan struct{})
	go func() {
		c, e := listener.Accept()
		if e != nil {
			return
		}
		defer c.Close()
		c.SetReadDeadline(time.Now().Add(2 * time.Second))
		io.Copy(io.Discard, c)
		close(closed)
	}()
	u, _ := url.Parse("socks5h://" + listener.Addr().String())
	d, e := newSOCKSDialer(u, time.Second, nil)
	if e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	c, e := d.DialContext(ctx, "tcp", "example.invalid:443")
	if e == nil || c != nil {
		t.Fatal("canceled connection returned")
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("SOCKS handshake connection leaked after cancellation")
	}
}
func TestProxySessionsAreUniqueAcrossInstancesAndRotateCredentials(t *testing.T) {
	examples := []string{"socks5h://acct-region-JP-sid-old-t-20:pass@host:1", "http://acct-session-old:pass@host:1", "http://acct:proxy-secret-JP-91977332-20m@host:1", "http://acct:pass@host:1?session={session}"}
	for _, template := range examples {
		if !ProxySupportsSessionRotation(template) {
			t.Fatal(template)
		}
		a := NewProxySession(template).Rotate()
		b := NewProxySession(template).Rotate()
		if a == b || a == template {
			t.Fatal("session collision")
		}
		if _, e := ParseProxy(a); e != nil {
			t.Fatal(e)
		}
	}
	fixed := "http://acct:pass@host:1"
	if ProxySupportsSessionRotation(fixed) || NewProxySession(fixed).Rotate() != fixed {
		t.Fatal("fixed proxy mutated")
	}
}
