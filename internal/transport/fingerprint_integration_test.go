package transport

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	tlsclient "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
)

func TestProfilesResolveBundledIdentityAndPlatform(t *testing.T) {
	for _, os := range []string{"darwin", "windows", "linux"} {
		p := ProfileForPlatform("chrome999", os)
		if p.Name != "chrome_133" || p.Major != 133 || strings.Contains(p.UserAgent, "999") {
			t.Fatal(p)
		}
		if _, ok := profiles.MappedTLSClients[p.Name]; !ok {
			t.Fatal("nonexistent profile")
		}
		if !strings.Contains(p.UserAgent, "Chrome/133.") || !strings.Contains(p.ClientHints, `v="133"`) {
			t.Fatal("mismatched identity")
		}
		if os == "windows" && p.Platform != "Windows" {
			t.Fatal(p)
		}
		if os == "linux" && p.Platform != "Linux" {
			t.Fatal(p)
		}
	}
}
func TestTLSProfileProbeUsesRealClientHelloAndFallback(t *testing.T) {
	var mu sync.Mutex
	hellos := 0
	grease := false
	versions := []string{}
	selectedProfile := ""
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		versions = append(versions, r.UserAgent())
		mu.Unlock()
		w.Write([]byte("ok"))
	}))
	server.TLS = &tls.Config{GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
		mu.Lock()
		defer mu.Unlock()
		hellos++
		for _, suite := range hello.CipherSuites {
			if suite&0x0f0f == 0x0a0a {
				grease = true
			}
		}
		if selectedProfile == "chrome_133" {
			return nil, errors.New("fixture rejects this ClientHello before HTTP")
		}
		return nil, nil
	}}
	server.StartTLS()
	defer server.Close()
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	c, e := probeFingerprintWithOptions(context.Background(), "chrome133", server.URL, FingerprintProbeOptions{Concurrency: 1}, func(name string) (*FingerprintClient, error) {
		mu.Lock()
		selectedProfile = name
		mu.Unlock()
		return newFingerprintClientExact(name, "", nil, tlsclient.WithTransportOptions(&tlsclient.TransportOptions{RootCAs: roots}))
	})
	if e != nil {
		t.Fatal(e)
	}
	defer c.Close()
	if c.Profile() != "chrome_131" {
		t.Fatalf("selected=%s", c.Profile())
	}
	mu.Lock()
	defer mu.Unlock()
	if hellos < 2 || !grease || len(versions) != 1 || !strings.Contains(versions[0], "Chrome/131.") {
		t.Fatalf("not real separate Chrome TLS sessions: hellos=%d grease=%t versions=%v", hellos, grease, versions)
	}
}
func TestFingerprintManualRedirectRawCookiesAndSingleJar(t *testing.T) {
	count := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count++
		if r.URL.Path == "/start" {
			w.Header().Add("Set-Cookie", "a=1; Path=/")
			w.Header().Add("Set-Cookie", "b=2; Path=/")
			w.Header().Set("Location", "/next")
			w.WriteHeader(302)
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"cookie": r.Header.Get("Cookie"), "ua": r.UserAgent(), "platform": r.Header.Get("Sec-CH-UA-Platform")})
	}))
	defer server.Close()
	c, e := NewFingerprintClient("chrome146", "", nil)
	if e != nil {
		t.Fatal(e)
	}
	defer c.Close()
	r, e := c.Do(context.Background(), Request{URL: server.URL + "/start"})
	if e != nil {
		t.Fatal(e)
	}
	if r.StatusCode != 302 || len(r.RawSetCookie) != 2 || count != 1 {
		t.Fatal("redirect followed or Set-Cookie lost")
	}
	r, e = c.Do(context.Background(), Request{URL: server.URL + "/next", Headers: http.Header{"User-Agent": {"Chrome/999"}}})
	if e != nil {
		t.Fatal(e)
	}
	var got map[string]string
	json.Unmarshal(r.Body, &got)
	if got["cookie"] != "a=1; b=2" || strings.Contains(got["ua"], "999") || got["platform"] == "" {
		t.Fatalf("identity/cookies drifted: %v", got)
	}
}
func TestTLSProfileProbeDoesNotRotateForChallenge(t *testing.T) {
	var calls atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Cf-Mitigated", "challenge")
		w.WriteHeader(403)
	}))
	defer s.Close()
	c, e := ProbeFingerprintClient(context.Background(), "auto", "", s.URL, nil)
	if e != nil || c == nil || c.Profile() != "chrome_133" || calls.Load() < 1 || calls.Load() > ProfileProbeConcurrency {
		t.Fatalf("challenge must retain the preferred transport for browser handoff: %v calls=%d", e, calls.Load())
	}
	defer c.Close()
}

func TestTLSProbeHTTPResponsesNeverRotateIdentity(t *testing.T) {
	for _, status := range []int{403, 409, 426, 429, 500, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var calls atomic.Int32
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Set-Cookie", "probe=keep; Path=/")
				w.WriteHeader(status)
			}))
			defer s.Close()
			jar := NewCookieJar()
			c, err := ProbeFingerprintClient(context.Background(), "chrome133", "", s.URL, jar)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			if calls.Load() < 1 || calls.Load() > ProfileProbeConcurrency || c.Profile() != "chrome_133" || jar.HeaderFor(s.URL) != "probe=keep" {
				t.Fatalf("HTTP %d changed identity/cookies: calls=%d profile=%s", status, calls.Load(), c.Profile())
			}
		})
	}
}
