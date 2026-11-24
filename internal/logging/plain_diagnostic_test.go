package logging

import (
	"strings"
	"testing"
)

// Plain diagnostic text still carries bearer tokens, JWTs, OTPs or known
// account secrets without an assignment. Optimizing assignment detection must
// not become an early return from these independent protections.
func TestPlainDiagnosticsKeepAllIndependentSecretProtections(t *testing.T) {
	for _, tc := range []struct {
		name, text, want string
		known            []string
	}{
		{"ordinary", "retry queued after password prompt", "retry queued after password prompt", nil},
		{"bearer", "request Bearer opaque-canary failed", "request [AUTH REDACTED] failed", nil},
		{"basic", "request Basic cHJpdmF0ZS1jYW5hcnk failed", "request [AUTH REDACTED] failed", nil},
		{"jwt", "received eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjMifQ.signature", "received " + Redacted, nil},
		{"otp", "received 456789 for verification", "received " + Redacted + " for verification", nil},
		{"contextual", "verification code is ABCD9", "verification code is " + Redacted, nil},
		{"known", "received user-held-canary", "received " + Redacted, []string{"user-held-canary"}},
		{"colon", "request password:opaque-canary failed", "request password:" + Redacted + " failed", nil},
		{"equals", "request password=opaque-canary failed", "request password=" + Redacted + " failed", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := RedactKnown(tc.text, tc.known...); got != tc.want {
				t.Fatalf("diagnostic redaction changed: got %q, want %q", got, tc.want)
			}
		})
	}
}

func BenchmarkMetadataLogRedaction(b *testing.B) {
	line := "event 001 " + strings.Repeat(".", 188)
	b.ReportAllocs()
	b.SetBytes(int64(len(line)))
	for b.Loop() {
		if got := RedactText(line); got != line {
			b.Fatal("ordinary metadata log changed")
		}
	}
}
