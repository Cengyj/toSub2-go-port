package transport

import (
	"net/http"
	"strings"
	"testing"
)

func TestDocumentCookieSharesAuthorityAndCannotReplaceHTTPOnly(t *testing.T) {
	jar := NewCookieJar()
	page := "https://auth.example.test/login/password"
	jar.SetFromResponse(page, http.Header{"Set-Cookie": {"session=server; Secure; HttpOnly; Path=/", "device=initial; Secure; Path=/"}})
	if got := jar.DocumentCookie(page); got != "device=initial" {
		t.Fatalf("script cookie visibility: %q", got)
	}
	for _, value := range []string{"session=script; Secure; Path=/", "session=; Max-Age=0; Secure; Path=/", "secret=new; HttpOnly; Secure; Path=/"} {
		jar.SetDocumentCookie(page, value)
	}
	jar.SetDocumentCookie(page, "device=changed; Secure; Path=/")
	if jar.HeaderFor(page) != "session=server; device=changed" {
		t.Fatal("script writes damaged protected cookies or missed the network jar")
	}
	jar.SetFromResponse(page, http.Header{"Set-Cookie": {"device=; Max-Age=0; Secure; Path=/"}})
	if jar.DocumentCookie(page) != "" {
		t.Fatal("server deletion left stale document cookie state")
	}
}

func TestDocumentCookieScopeAndSecureOverwriteProtection(t *testing.T) {
	jar := NewCookieJar()
	page := "https://auth.example.test/login/password"
	jar.SetDocumentCookie(page, "path=visible")
	jar.SetDocumentCookie(page, "shared=yes; Domain=example.test; Path=/; Secure")
	jar.SetDocumentCookie(page, "foreign=no; Domain=other.test; Path=/")
	jar.SetDocumentCookie(page, "suffix=no; Domain=test; Path=/")
	if !strings.Contains(jar.DocumentCookie(page), "path=visible") || jar.DocumentCookie("https://auth.example.test/other") != "shared=yes" || jar.DocumentCookie("https://sibling.example.test/") != "shared=yes" {
		t.Fatal("document cookie domain/path semantics changed")
	}
	jar.SetDocumentCookie("http://auth.example.test/", "shared=insecure; Domain=example.test; Path=/")
	jar.SetDocumentCookie("http://auth.example.test/", "forbidden=value; Secure; Path=/")
	if jar.DocumentCookie("http://auth.example.test/") != "" || jar.DocumentCookie("https://sibling.example.test/") != "shared=yes" {
		t.Fatal("insecure page modified or exposed secure cookies")
	}
	jar.SetDocumentCookie(page, "path=; Max-Age=0")
	if jar.DocumentCookie(page) != "shared=yes" {
		t.Fatal("script deletion did not use the document's default cookie path")
	}
}
