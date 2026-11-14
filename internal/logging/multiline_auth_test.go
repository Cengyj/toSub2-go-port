package logging

import (
	"strings"
	"testing"
)

func TestMultilineAndEscapedAuthenticationContainersAreFullyRedacted(t *testing.T) {
	for _, sample := range []string{
		"credentials={\n  \"opaque\":\"raw-session-canary\"\n}",
		"'cookies': [\n {\"value\":\"raw-session-canary\"}\n]",
		"response={\n  \"cookies\": [\n    {\"name\":\"sid\", \"value\":\"raw-session-canary\"}\n  ]\n}",
		"response={\n  \"cookies\":\n  [\n    {\"value\":\"raw-session-canary\"}\n  ]\n}",
		`response={"c\u006fokies":[{"name":"sid","value":"raw-session-canary"}]}`,
		"response={\n  \"headers\": {\n    \"X-Custom\":\"raw-session-canary\"\n  }\n}",
		"response={\n  \"cookies\": [\n    {\"value\":\"raw-session-canary\"",
	} {
		if got := RedactText("request failed\n" + sample + "\nretry exhausted"); strings.Contains(got, "raw-session-canary") {
			t.Fatal("multiline authentication container leaked")
		}
	}
}

func TestLegacySessionAliasesAreRedactedInScalarDiagnostics(t *testing.T) {
	for _, key := range []string{"oai-client-auth-session", "clientAuthSession", "cookieRef", "cookie_state", "cookie-snapshot", "sessionCookies", "webSession", "recoveryCheckpoint", "preparationCheckpoint", "reauthentication_previous_checkpoint", "initialLoginProgress"} {
		if got := RedactText("request failed " + key + "=raw-session-canary; retry exhausted"); strings.Contains(got, "raw-session-canary") {
			t.Fatalf("session diagnostic alias leaked: %s", key)
		}
		for _, value := range []string{"[\n \"raw-session-canary\"\n]", "{\n \"opaque\":\"raw-session-canary\"\n}"} {
			if got := RedactText("request failed " + key + "=" + value + "; retry exhausted"); strings.Contains(got, "raw-session-canary") {
				t.Fatalf("session container alias leaked: %s", key)
			}
		}
	}
}
