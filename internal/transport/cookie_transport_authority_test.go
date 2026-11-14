package transport

import (
	"context"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"reflect"
	"sync/atomic"
	"testing"

	fhttp "github.com/bogdanfinn/fhttp"
	tlsclient "github.com/bogdanfinn/tls-client"
)

type authorityNativeJar struct {
	http.CookieJar
	reads, writes atomic.Int32
}

func (j *authorityNativeJar) Cookies(u *url.URL) []*http.Cookie {
	j.reads.Add(1)
	return j.CookieJar.Cookies(u)
}

func (j *authorityNativeJar) SetCookies(u *url.URL, cookies []*http.Cookie) {
	j.writes.Add(1)
	j.CookieJar.SetCookies(u, cookies)
}

type authorityFingerprintJar struct {
	fhttp.CookieJar
	reads, writes atomic.Int32
}

func (j *authorityFingerprintJar) Cookies(u *url.URL) []*fhttp.Cookie {
	j.reads.Add(1)
	return j.CookieJar.Cookies(u)
}

func (j *authorityFingerprintJar) SetCookies(u *url.URL, cookies []*fhttp.Cookie) {
	j.writes.Add(1)
	j.CookieJar.SetCookies(u, cookies)
}

func cookieAuthorityServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/rotate" {
			w.Header().Add("Set-Cookie", "accepted=shared; Path=/; HttpOnly")
		}
		fmt.Fprint(w, r.Header.Get("Cookie"))
	}))
}

func exerciseCookieAuthority(t *testing.T, rawURL string, jar *CookieJar, singleAttempt bool, do func(context.Context, Request) (*Response, error)) {
	t.Helper()
	for _, test := range []struct{ path, want string }{
		{"/rotate", ""},
		{"/check", "accepted=shared"},
	} {
		response, err := do(t.Context(), Request{URL: rawURL + test.path, Headers: http.Header{"Cookie": {"forged=explicit"}}, SingleAttempt: singleAttempt})
		if err != nil {
			t.Fatal(err)
		}
		if response.Text() != test.want {
			t.Fatalf("request cookies came from another store: %q", response.Text())
		}
	}
	entries := jar.Cookies()
	if len(entries) != 1 || entries[0].Name != "accepted" || entries[0].Value != "shared" || !entries[0].HTTPOnly {
		t.Fatal("response cookie did not reach the shared authoritative jar")
	}
}

func TestNativeHTTPDisablesInjectedJarWithoutChangingCallerClient(t *testing.T) {
	for _, singleAttempt := range []bool{false, true} {
		t.Run(fmt.Sprint(singleAttempt), func(t *testing.T) {
			server := cookieAuthorityServer(t)
			defer server.Close()
			u, err := url.Parse(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			underlying, err := cookiejar.New(nil)
			if err != nil {
				t.Fatal(err)
			}
			underlying.SetCookies(u, []*http.Cookie{{Name: "stale", Value: "secondary"}})
			external := &authorityNativeJar{CookieJar: underlying}
			jar := NewCookieJar()
			client := NewClient(jar)
			defer client.Close()
			callerClient := client.HTTP
			callerClient.Jar = external
			before := *callerClient
			exerciseCookieAuthority(t, server.URL, jar, singleAttempt, client.Do)
			if client.HTTP != callerClient || callerClient.Jar != external || callerClient.Transport != before.Transport || callerClient.Timeout != before.Timeout || reflect.ValueOf(callerClient.CheckRedirect).Pointer() != reflect.ValueOf(before.CheckRedirect).Pointer() {
				t.Fatal("adapter changed the caller-owned HTTP client")
			}
			if external.reads.Load() != 0 || external.writes.Load() != 0 {
				t.Fatal("adapter accessed the injected cookie store")
			}
			if cookies := underlying.Cookies(u); len(cookies) != 1 || cookies[0].Name != "stale" || cookies[0].Value != "secondary" {
				t.Fatal("response changed the caller's cookie store")
			}
		})
	}
}

func TestFingerprintOptionsCannotInstallSecondCookieJar(t *testing.T) {
	server := cookieAuthorityServer(t)
	defer server.Close()
	u, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	underlying := tlsclient.NewCookieJar()
	underlying.SetCookies(u, []*fhttp.Cookie{{Name: "stale", Value: "secondary"}})
	external := &authorityFingerprintJar{CookieJar: underlying}
	jar := NewCookieJar()
	client, err := NewFingerprintClientExact("chrome133", "", jar, tlsclient.WithCookieJar(external))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if client.client.GetCookieJar() != nil {
		t.Fatal("TLS customization installed a second cookie jar")
	}
	exerciseCookieAuthority(t, server.URL, jar, false, client.Do)
	if external.reads.Load() != 0 || external.writes.Load() != 0 {
		t.Fatal("TLS adapter accessed the injected cookie store")
	}
	if cookies := underlying.Cookies(u); len(cookies) != 1 || cookies[0].Name != "stale" || cookies[0].Value != "secondary" {
		t.Fatal("TLS response changed the external cookie store")
	}
}
