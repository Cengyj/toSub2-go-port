package logging

import (
	"strings"
	"testing"
)

func TestAuthenticationJSONContainersNeverLeavePartialSecretsInLogs(t *testing.T) {
	for _, sample := range []string{
		`response={"cookies":[{"name":"sid","value":"first-canary"},{"name":"auth","value":"second-canary"}],"business":"context"}`,
		`response={"headers":{"Set-Cookie":["sid=first-canary","auth=second-canary"],"Location":"https://example.test/?code=third-canary"}}`,
		`payload credentials={"access_token":"first-canary","nested":{"refresh_token":"second-canary"}}`,
		`state={"transportSession":{"device":"first-canary","cookie":"second-canary"}}`,
		`checkpoint={"login_session":["first-canary","second-canary"]}`,
	} {
		got := RedactText("request timeout\n" + sample + "\nretry exhausted")
		if strings.Contains(got, "canary") || !strings.HasPrefix(got, "request timeout\n") || !strings.HasSuffix(got, "\nretry exhausted") {
			t.Fatalf("structured authentication log escaped or unrelated diagnostics lost: %q", got)
		}
	}
	// A malformed private container has no trustworthy end delimiter. Its
	// remaining fragment is hidden rather than treating later lines as safe.
	got := RedactText("request timeout\nresponse={\"cookies\":[{\"value\":\"first-canary\"},\nsecond-canary")
	if strings.Contains(got, "canary") || !strings.HasPrefix(got, "request timeout\n") {
		t.Fatal("unterminated private container leaked")
	}
}

func TestOldAuthenticationIdentityScalarsAreRedacted(t *testing.T) {
	for _, key := range []string{"authSessionLoggingId", "transport_session", "sessionRef", "previous_session_ref", "callbackUrl", "authorization_code", "oauthState", "loginSession"} {
		if !SensitiveKey(key) {
			t.Errorf("private identity key not classified: %s", key)
		}
		got := RedactText(key + `="synthetic-private-value"`)
		if strings.Contains(got, "synthetic-private-value") {
			t.Errorf("private identity reached log: %s", key)
		}
	}
}
