package legacyruntime

import (
	"context"
	"errors"
	"strings"
	"testing"

	"tosub2-go-port/internal/logging"
)

func TestChallengeFailureWordingSurvivesProductionRedaction(t *testing.T) {
	progress := challengeProgress{stage: "parent_execution", requests: 2, lastHTTP: 200}
	cause := errors.New("GO_JS_RUNTIME: SCRIPT_EXCEPTION")
	err := progress.failure(context.Background(), cause)
	if !errors.Is(err, cause) {
		t.Fatal("diagnostic wording changed the underlying failure")
	}
	for _, redact := range []struct {
		name string
		run  func(string) string
	}{
		{"text", logging.RedactText},
		{"known", func(text string) string { return logging.RedactKnown(text, "synthetic-private-known") }},
	} {
		t.Run(redact.name, func(t *testing.T) {
			got := redact.run(err.Error())
			if got != err.Error() || !strings.Contains(got, "LEGACY_RUNTIME_CHALLENGE: challenge execution failed") {
				t.Fatalf("safe challenge wording was hidden by credential redaction: %s", got)
			}
			for _, evidence := range []string{"stage=parent_execution", "frame=false child=false", "requests=2 last_http=200", "GO_JS_RUNTIME: SCRIPT_EXCEPTION"} {
				if !strings.Contains(got, evidence) {
					t.Fatalf("safe challenge evidence disappeared: %s", evidence)
				}
			}
		})
	}
}

func TestChallengeFailureRedactionStillHidesAuthenticationMaterial(t *testing.T) {
	progress := challengeProgress{stage: "parent_execution", requests: 2, lastHTTP: 200}
	base := progress.failure(context.Background(), errors.New("GO_JS_RUNTIME: SCRIPT_EXCEPTION")).Error()
	// Deliberately synthetic authentication values exercise the real redactors.
	private := "\nverification code is 654321; security code A1B2C3" +
		"\nCookie: synthetic-session=synthetic-cookie-value" +
		"\nAuthorization: Bearer synthetic-auth-value" +
		"\npassword=synthetic-password-value" +
		"\nhttps://fixture.invalid/path?token=synthetic-url-value"
	for _, redact := range []struct {
		name string
		run  func(string) string
	}{
		{"text", logging.RedactText},
		{"known", func(text string) string {
			return logging.RedactKnown(text+"\nsynthetic-private-known", "synthetic-private-known")
		}},
	} {
		t.Run(redact.name, func(t *testing.T) {
			got := redact.run(base + private)
			if !strings.Contains(got, "challenge execution failed") {
				t.Fatal("fixed challenge diagnosis disappeared beside sensitive material")
			}
			for _, secret := range []string{"654321", "A1B2C3", "synthetic-cookie-value", "synthetic-auth-value", "synthetic-password-value", "fixture.invalid", "synthetic-url-value", "synthetic-private-known"} {
				if strings.Contains(got, secret) {
					t.Fatal("authentication material survived challenge diagnostic redaction")
				}
			}
		})
	}
}
