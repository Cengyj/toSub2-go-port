package protocol

import (
	"strings"
	"testing"
)

func TestOAuthCallbackRequiresStateAndRedactsUntrustedErrors(t *testing.T) {
	for _, raw := range []string{"http://localhost:1455/auth/callback?code=c", "http://localhost:1455/auth/callback?code=c&state=s"} {
		if _, err := ParseOAuthCallback(raw, ""); err == nil {
			t.Fatal("callback without expected state accepted")
		}
	}
	for _, code := range []string{"access_denied", "secret-in-error-field"} {
		_, err := ParseOAuthCallback("http://localhost:1455/auth/callback?state=s&error="+code+"&error_description=plain-sensitive-token-without-label", "s")
		if err == nil || strings.Contains(err.Error(), "plain-sensitive") || strings.Contains(err.Error(), "secret-in-error") {
			t.Fatalf("unsafe callback error %v", err)
		}
	}
}
