package transport

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	tlsclient "github.com/bogdanfinn/tls-client"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

type singleAttemptClient interface {
	Do(context.Context, Request) (*Response, error)
	Close()
}

func singleAttemptTestClient(t *testing.T, kind string, roots *x509.CertPool) singleAttemptClient {
	t.Helper()
	if kind == "native" {
		c := NewClient(nil)
		if roots != nil {
			c.HTTP.Transport.(*http.Transport).TLSClientConfig = &tls.Config{RootCAs: roots}
		}
		t.Cleanup(c.Close)
		return c
	}
	c, err := NewFingerprintClientExact("chrome_146", "", nil, tlsclient.WithTransportOptions(&tlsclient.TransportOptions{RootCAs: roots}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}
func TestSingleAttemptHTTPDoesNotFollowRedirectOrReplayDisconnect(t *testing.T) {
	for _, kind := range []string{"native", "fingerprint"} {
		for _, failure := range []string{"disconnect", "redirect", "forbidden", "unavailable"} {
			t.Run(kind+"/"+failure, func(t *testing.T) {
				var calls atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					_, _ = io.Copy(io.Discard, r.Body)
					switch failure {
					case "disconnect":
						conn, _, err := w.(http.Hijacker).Hijack()
						if err != nil {
							t.Error(err)
							return
						}
						_ = conn.Close()
					case "redirect":
						w.Header().Set("Location", "/replay")
						w.WriteHeader(307)
					case "forbidden":
						w.Header().Set("Cf-Mitigated", "challenge")
						w.WriteHeader(403)
					default:
						w.WriteHeader(503)
					}
				}))
				defer server.Close()
				c := singleAttemptTestClient(t, kind, nil)
				_, err := c.Do(t.Context(), Request{Method: http.MethodPost, URL: server.URL, SingleAttempt: true, Timeout: time.Second})
				if failure == "disconnect" && err == nil {
					t.Fatal("disconnect treated as success")
				}
				if calls.Load() != 1 {
					t.Fatalf("HTTP request count=%d", calls.Load())
				}
			})
		}
	}
}

// A peer can reset an accepted HTTP/2 stream with REFUSED_STREAM or GOAWAY.
// Both Go HTTP libraries would normally replay empty security POSTs. Count real
// wire HEADERS across all connections to verify that the guard prevents that.
func TestSingleAttemptHTTP2BlocksInternalStreamReplay(t *testing.T) {
	for _, kind := range []string{"native", "fingerprint"} {
		for _, failure := range []string{"refused", "goaway"} {
			for _, body := range []string{"", "{}"} {
				t.Run(kind+"/"+failure+"/"+body, func(t *testing.T) {
					certServer := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
					certs := certServer.TLS.Certificates
					roots := x509.NewCertPool()
					roots.AddCert(certServer.Certificate())
					certServer.Close()
					listener, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: certs, NextProtos: []string{"h2"}})
					if err != nil {
						t.Fatal(err)
					}
					var requests atomic.Int32
					var workers sync.WaitGroup
					var mu sync.Mutex
					conns := map[net.Conn]bool{}
					done := make(chan struct{})
					go func() {
						defer close(done)
						for {
							conn, e := listener.Accept()
							if e != nil {
								return
							}
							mu.Lock()
							conns[conn] = true
							mu.Unlock()
							workers.Add(1)
							go func() {
								defer workers.Done()
								defer conn.Close()
								_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
								preface := make([]byte, len(http2.ClientPreface))
								if _, e := io.ReadFull(conn, preface); e != nil {
									return
								}
								if string(preface) != http2.ClientPreface {
									return
								}
								framer := http2.NewFramer(conn, conn)
								framer.ReadMetaHeaders = hpack.NewDecoder(1<<20, nil)
								if framer.WriteSettings() != nil {
									return
								}
								for {
									frame, e := framer.ReadFrame()
									if e != nil {
										return
									}
									switch f := frame.(type) {
									case *http2.SettingsFrame:
										if !f.IsAck() {
											if framer.WriteSettingsAck() != nil {
												return
											}
										}
									case *http2.MetaHeadersFrame:
										requests.Add(1)
										if failure == "refused" {
											e = framer.WriteRSTStream(f.StreamID, http2.ErrCodeRefusedStream)
										} else {
											e = framer.WriteGoAway(0, http2.ErrCodeNo, nil)
										}
										if e != nil {
											return
										}
									}
								}
							}()
						}
					}()
					t.Cleanup(func() {
						_ = listener.Close()
						<-done
						mu.Lock()
						for conn := range conns {
							_ = conn.Close()
						}
						mu.Unlock()
						workers.Wait()
					})
					c := singleAttemptTestClient(t, kind, roots)
					_, err = c.Do(t.Context(), Request{Method: http.MethodPost, URL: "https://" + listener.Addr().String() + "/security", Body: []byte(body), SingleAttempt: true, Timeout: 2 * time.Second})
					if err == nil || requests.Load() != 1 {
						t.Fatalf("security request replay: count=%d error=%v", requests.Load(), err)
					}
				})
			}
		}
	}
}
