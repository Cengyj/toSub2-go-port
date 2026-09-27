package transport

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestResponseCookiesRespectDomainPathAndDeletion(t *testing.T) {
	jar := NewCookieJar()
	jar.SetFromResponse("https://auth.example.com/account/login", http.Header{"Set-Cookie": {
		"local=value; HttpOnly",
		"parent=value; Domain=example.com; Path=/",
		"foreign=bad; Domain=unrelated.example; Path=/",
		"suffix=bad; Domain=com; Path=/",
	}})
	if got := jar.HeaderFor("https://auth.example.com/elsewhere"); got != "parent=value" {
		t.Fatalf("default path leaked cookie: %q", got)
	}
	if got := jar.HeaderFor("https://unrelated.example/"); got != "" {
		t.Fatalf("foreign domain accepted: %q", got)
	}
	if got := jar.HeaderFor("https://other.com/"); got != "" {
		t.Fatalf("public suffix accepted: %q", got)
	}
	if got := jar.HeaderFor("https://auth.example.com/account/callback"); !strings.Contains(got, "local=value") {
		t.Fatal("default-path cookie missing from sibling endpoint")
	}
	jar.SetFromResponse("https://auth.example.com/account/login", http.Header{"Set-Cookie": {"local=deleted; Max-Age=0; Expires=Thu, 01 Jan 2099 00:00:00 GMT"}})
	if jar.Has("local") {
		t.Fatal("Expires overrode Max-Age deletion")
	}
}

func TestResponseCookiePrefixesAndLongestPath(t *testing.T) {
	jar := NewCookieJar()
	jar.SetFromResponse("https://auth.example.com/", http.Header{"Set-Cookie": {
		"sid=root; Path=/",
		"sid=scoped; Path=/account",
		"__Host-invalid=bad; Secure; Domain=example.com; Path=/",
		"__Secure-invalid=bad; Path=/",
		"__Host-valid=ok; Secure; Path=/",
	}})
	if jar.Has("__Host-invalid") || jar.Has("__Secure-invalid") || !jar.Has("__Host-valid") {
		t.Fatal("cookie prefix constraints were not enforced")
	}
	if got := jar.HeaderFor("https://auth.example.com/account/login"); !strings.HasPrefix(got, "sid=scoped; sid=root") {
		t.Fatalf("most specific cookie must precede root cookie: %q", got)
	}
	if got := jar.Value("sid", "https://auth.example.com/account/login"); got != "scoped" {
		t.Fatalf("value lookup chose wrong path: %q", got)
	}
	jar.Set(Cookie{Name: "expired", Value: "x", Domain: "auth.example.com", Path: "/", Expires: time.Now().Add(time.Hour).UnixMilli()})
	jar.mu.Lock()
	jar.cookies[len(jar.cookies)-1].Expires = 1
	jar.mu.Unlock()
	if jar.Has("expired") || jar.Value("expired", "https://auth.example.com/") != "" {
		t.Fatal("expired cookie remains observable")
	}
}
