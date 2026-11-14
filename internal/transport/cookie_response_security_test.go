package transport

import (
	"net/http"
	"testing"
)

func TestInsecureResponseCannotSetOrShadowSecureCookie(t *testing.T) {
	for _, value := range []string{
		"sid=forged; Path=/account",
		"sid=; Path=/account; Max-Age=0",
		"sid=forged; Path=/account/verify",
		"sid=forged; Domain=example.test; Path=/account",
	} {
		t.Run(value, func(t *testing.T) {
			jar := NewCookieJar()
			jar.SetFromResponse("https://auth.example.test/account", http.Header{"Set-Cookie": {"sid=protected; Secure; HttpOnly; Path=/account"}})
			jar.SetFromResponse("http://auth.example.test/account", http.Header{"Set-Cookie": {value, "fresh=allowed; Path=/"}})
			if got := jar.HeaderFor("https://auth.example.test/account/verify"); got != "sid=protected; fresh=allowed" {
				t.Fatalf("insecure response changed secure session: %q", got)
			}
		})
	}
	jar := NewCookieJar()
	jar.SetFromResponse("http://auth.example.test/", http.Header{"Set-Cookie": {"sid=forged; Secure; Path=/"}})
	if jar.Has("sid") {
		t.Fatal("insecure response created a Secure cookie")
	}
}

func TestSecureCookieOverlayPolicyKeepsLegitimateUpdates(t *testing.T) {
	jar := NewCookieJar()
	jar.SetFromResponse("https://auth.example.test/account", http.Header{"Set-Cookie": {"sid=protected; Secure; Path=/account"}})
	jar.SetFromResponse("http://auth.example.test/", http.Header{"Set-Cookie": {"sid=unrelated; Path=/accounts"}})
	if got := jar.HeaderFor("http://auth.example.test/accounts"); got != "sid=unrelated" {
		t.Fatalf("different path was incorrectly rejected: %q", got)
	}
	jar.SetFromResponse("https://auth.example.test/account", http.Header{"Set-Cookie": {"sid=rotated; Secure; Path=/account"}})
	if got := jar.Value("sid", "https://auth.example.test/account"); got != "rotated" {
		t.Fatal("secure response could not rotate its cookie")
	}
	jar.SetFromResponse("https://auth.example.test/account", http.Header{"Set-Cookie": {"sid=; Secure; Path=/account; Max-Age=0"}})
	jar.SetFromResponse("http://auth.example.test/account", http.Header{"Set-Cookie": {"sid=fresh; Path=/account"}})
	if got := jar.Value("sid", "http://auth.example.test/account"); got != "fresh" {
		t.Fatal("deleted secure cookie blocked a later legitimate cookie")
	}
}
