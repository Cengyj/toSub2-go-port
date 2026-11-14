package transport

import (
	"net/http"
	"testing"
)

func TestSetCookieBatchDoesNotSilentlyDisappearAtHTTPReaderLimit(t *testing.T) {
	t.Setenv("GODEBUG", "httpcookiemaxnum=2")
	jar := NewCookieJar()
	jar.SetFromResponse("https://auth.example/", http.Header{"Set-Cookie": {"a=one; Path=/", "b=two; Path=/", "c=three; Path=/"}})
	if len(jar.Cookies()) != 3 {
		t.Fatal("valid response batch disappeared under net/http bulk-reader limit")
	}
}
