package transport

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
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

func TestCookieJarJSONRoundTrip(t *testing.T) {
	j := NewCookieJar(Cookie{Name: "a", Value: "b", Domain: ".Example.com", Path: "/", Expires: time.Now().Add(time.Hour).UnixMilli()})
	var out []Cookie
	b, _ := json.Marshal(j.Cookies())
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].Domain != "example.com" {
		t.Fatalf("roundtrip: %#v", out)
	}
}

func TestGetSetCookieCombinedAndExpiresComma(t *testing.T) {
	h := http.Header{"Set-Cookie": []string{"a=1; Expires=Wed, 21 Oct 2030 07:28:00 GMT; Path=/, b=2; Path=/"}}
	got := GetSetCookie(h)
	if len(got) != 2 || !strings.HasPrefix(got[0], "a=1") || !strings.HasPrefix(got[1], "b=2") {
		t.Fatalf("%#v", got)
	}
}

func TestCookieCheckpointPreservesHostOnlyHTTPOnlyAndSameSite(t *testing.T) {
	parsed, ok := ParseSetCookie("__Host-csrf=fixture; Path=/; Secure; HttpOnly; SameSite=Lax", "https://auth.example/login")
	if !ok || !parsed.HostOnly || !parsed.HTTPOnly || parsed.SameSite != "Lax" {
		t.Fatal("Set-Cookie attributes lost")
	}
	encoded, e := json.Marshal(parsed)
	if e != nil {
		t.Fatal(e)
	}
	var restored Cookie
	if e = json.Unmarshal(encoded, &restored); e != nil {
		t.Fatal(e)
	}
	if !restored.HostOnly || !restored.HTTPOnly || restored.SameSite != "Lax" {
		t.Fatal("checkpoint lost cookie security attributes")
	}
	jar := NewCookieJar(restored)
	if jar.HeaderFor("https://child.auth.example/") != "" {
		t.Fatal("host-only cookie leaked to subdomain after restore")
	}
	domain, ok := ParseSetCookie("session=value; Domain=.auth.example; Secure; HttpOnly; SameSite=None", "https://auth.example/")
	if !ok || domain.HostOnly || !domain.HTTPOnly || domain.SameSite != "None" {
		t.Fatal("domain cookie attributes changed")
	}
}

func TestLegacyHostPrefixedCookieRestoresAsHostOnly(t *testing.T) {
	for _, tc := range []struct {
		name         string
		wantHostOnly bool
	}{{"__Host-next-auth.csrf-token", true}, {"legacy-domain-session", false}} {
		wire := `{"name":"` + tc.name + `","value":"fixture","domain":"chatgpt.com","path":"/","secure":true,"expires":null}`
		var cookie Cookie
		if e := json.Unmarshal([]byte(wire), &cookie); e != nil {
			t.Fatal(e)
		}
		if cookie.HostOnly != tc.wantHostOnly {
			t.Fatalf("legacy %s host-only policy changed", tc.name)
		}
	}
}
