package transport

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	tlsclient "github.com/bogdanfinn/tls-client"
)

type lifetimeDoer interface {
	Do(context.Context, Request) (*Response, error)
	Close()
}

func lifetimeClient(t *testing.T, kind string, jar *CookieJar, server *httptest.Server) lifetimeDoer {
	t.Helper()
	if kind == "native" {
		c, err := NewNativeClient("", jar)
		if err != nil {
			t.Fatal(err)
		}
		if server.TLS != nil {
			base := c.HTTP.Transport.(*http.Transport)
			base.TLSClientConfig.RootCAs = x509.NewCertPool()
			base.TLSClientConfig.RootCAs.AddCert(server.Certificate())
		}
		t.Cleanup(c.Close)
		return c
	}
	var opts []tlsclient.HttpClientOption
	if server.TLS != nil {
		roots := x509.NewCertPool()
		roots.AddCert(server.Certificate())
		opts = append(opts, tlsclient.WithTransportOptions(&tlsclient.TransportOptions{RootCAs: roots}))
	}
	c, err := newFingerprintClientExact("chrome_133", "", jar, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}

func TestTransportBodyCancellationPreservesAcceptedCookieHeaders(t *testing.T) {
	for _, kind := range []string{"native", "fingerprint"} {
		t.Run(kind, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/resume" {
					fmt.Fprint(w, r.Header.Get("Cookie"))
					return
				}
				w.Header().Add("Set-Cookie", "old_session=; Path=/; Max-Age=0; Secure; HttpOnly")
				w.Header().Add("Set-Cookie", "new_session=synthetic; Path=/; Secure; HttpOnly")
				w.WriteHeader(200)
				w.(http.Flusher).Flush()
				<-r.Context().Done()
			}))
			defer server.Close()
			jar := NewCookieJar()
			jar.SetFromResponse(server.URL, http.Header{"Set-Cookie": {"old_session=synthetic-old; Path=/; Secure"}})
			c := lifetimeClient(t, kind, jar, server)
			_, err := c.Do(context.Background(), Request{URL: server.URL + "/slow", Timeout: 200 * time.Millisecond})
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("response body should report request deadline: %v", err)
			}
			if got := jar.HeaderFor(server.URL); got != "new_session=synthetic" {
				t.Fatalf("accepted cookie update/deletion was lost: %q", got)
			}
			response, err := c.Do(context.Background(), Request{URL: server.URL + "/resume", Timeout: time.Second})
			if err != nil || response.Text() != "new_session=synthetic" {
				t.Fatalf("resumed request sent stale session: %v", err)
			}
		})
	}
}

func TestTransportTruncatedBodyPreservesCookiesAndRejectsPartialSuccess(t *testing.T) {
	for _, kind := range []string{"native", "fingerprint"} {
		t.Run(kind, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Add("Set-Cookie", "received=synthetic; Path=/")
				w.Header().Set("Content-Length", "100")
				fmt.Fprint(w, "incomplete")
			}))
			defer server.Close()
			jar := NewCookieJar()
			c := lifetimeClient(t, kind, jar, server)
			response, err := c.Do(context.Background(), Request{URL: server.URL + "/?code=never-log-this", Timeout: time.Second})
			if err == nil || response != nil {
				t.Fatal("truncated body reported success")
			}
			if strings.Contains(err.Error(), "never-log-this") {
				t.Fatal("request URL leaked in body error")
			}
			if jar.HeaderFor(server.URL) != "received=synthetic" {
				t.Fatal("truncated response lost its accepted cookie")
			}
		})
	}
}

// This black-box regression crosses the former real 30s client timeout. It is
// opt-in to avoid adding 31s to every fast local run, and uses local TLS only.
func TestTransportConfiguredBudgetCanExceedThirtySeconds(t *testing.T) {
	if os.Getenv("TOSUB2_LONG_TRANSPORT_TEST") != "1" {
		t.Skip("set TOSUB2_LONG_TRANSPORT_TEST=1 for a real 31-second request-budget regression")
	}
	for _, kind := range []string{"native", "fingerprint"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				timer := time.NewTimer(31 * time.Second)
				defer timer.Stop()
				select {
				case <-r.Context().Done():
					return
				case <-timer.C:
				}
				fmt.Fprint(w, "configured-budget-honored")
			}))
			defer server.Close()
			c := lifetimeClient(t, kind, NewCookieJar(), server)
			response, err := c.Do(context.Background(), Request{URL: server.URL, Timeout: 35 * time.Second})
			if err != nil || response.Text() != "configured-budget-honored" {
				t.Fatalf("configured request budget was capped: %v", err)
			}
		})
	}
}
