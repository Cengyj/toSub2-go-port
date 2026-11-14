package transport

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestCookieJarSetCookieDomainPathAndExpiry(t *testing.T) {
	j := NewCookieJar()
	h := make(http.Header)
	h.Add("Set-Cookie", "sid=abc; Path=/api; HttpOnly")
	h.Add("Set-Cookie", "secure=1; Secure; Max-Age=3600")
	j.SetFromResponse("https://example.com/login", h)
	if got := j.HeaderFor("https://example.com/api/v1"); got != "sid=abc; secure=1" {
		t.Fatalf("header = %q", got)
	}
	if got := j.HeaderFor("http://example.com/api/v1"); got != "sid=abc" {
		t.Fatalf("secure cookie leaked over HTTP: %q", got)
	}
	if got := j.HeaderFor("https://other.example.com/api/v1"); got != "" {
		t.Fatalf("host mismatch: %q", got)
	}
	c, ok := ParseSetCookie("old=x; Max-Age=0", "https://example.com/")
	if !ok {
		t.Fatal("parse failed")
	}
	j.Set(c)
	if j.Has("old") {
		t.Fatal("expired cookie retained")
	}
}

func TestCookiesRejectPersistentImportAndExport(t *testing.T) {
	j := NewCookieJar(Cookie{Name: "private-cookie", Value: "private-value", Domain: "example.com", Path: "/"})
	if _, err := json.Marshal(j.Cookies()); err == nil {
		t.Fatal("cookies were serialized")
	}
	var c Cookie
	if err := json.Unmarshal([]byte(`{"name":"private-cookie","value":"private-value","domain":"example.com"}`), &c); err == nil {
		t.Fatal("cookies were imported")
	}
}

func TestGetSetCookieCombinedAndExpiresComma(t *testing.T) {
	h := http.Header{"Set-Cookie": []string{"a=1; Expires=Wed, 21 Oct 2030 07:28:00 GMT; Path=/, b=2; Path=/"}}
	got := GetSetCookie(h)
	if len(got) != 2 || !strings.HasPrefix(got[0], "a=1") || !strings.HasPrefix(got[1], "b=2") {
		t.Fatalf("%#v", got)
	}
}

func TestCookieMemoryPreservesHostOnlyHTTPOnlyAndSameSite(t *testing.T) {
	parsed, ok := ParseSetCookie("__Host-csrf=fixture; Path=/; Secure; HttpOnly; SameSite=Lax", "https://auth.example/login")
	if !ok || !parsed.HostOnly || !parsed.HTTPOnly || parsed.SameSite != "Lax" {
		t.Fatal("Set-Cookie attributes lost")
	}
	jar := NewCookieJar(parsed)
	if jar.HeaderFor("https://child.auth.example/") != "" {
		t.Fatal("host-only cookie leaked to a subdomain")
	}
	domain, ok := ParseSetCookie("session=value; Domain=.auth.example; Secure; HttpOnly; SameSite=None", "https://auth.example/")
	if !ok || domain.HostOnly || !domain.HTTPOnly || domain.SameSite != "None" {
		t.Fatal("domain cookie attributes changed")
	}
}

func TestLegacyHostPrefixedCookieImportIsRejected(t *testing.T) {
	for _, name := range []string{"__Host-next-auth.csrf-token", "legacy-domain-session"} {
		var cookie Cookie
		if err := json.Unmarshal([]byte(`{"name":"`+name+`","value":"fixture","domain":"chatgpt.com"}`), &cookie); err == nil {
			t.Fatal("legacy cookie imported")
		}
		if cookie.Name != "" || cookie.Value != "" {
			t.Fatal("rejected import retained authentication material")
		}
	}
}
