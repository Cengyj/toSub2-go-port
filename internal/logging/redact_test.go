package logging

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

func TestRedactTextJSONHeadersURLsAndOTP(t *testing.T) {
	cases := []struct {
		text    string
		secrets []string
	}{
		{`{"PASSWORD":"spaced pass value","totp_secret":"ABCDSECRET","Access_Token":"token-value"}`, []string{"spaced pass value", "ABCDSECRET", "token-value"}},
		{"Cookie: first=one-secret; second=another-secret\nAuthorization: Bearer opaque-token", []string{"one-secret", "another-secret", "opaque-token"}},
		{"request https://user:proxy-pass@proxy.invalid/path?key=api-secret failed", []string{"proxy-pass", "api-secret"}},
		{"POST failed Bearer plain-bearer-token code=123456 api_key='key value'", []string{"plain-bearer-token", "123456", "key value"}},
		{"email verification code is 654321", []string{"654321"}},
		{"failed eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjMifQ.signature", []string{"eyJhbGciOiJIUzI1NiJ9"}},
	}
	for _, c := range cases {
		got := RedactText(c.text)
		for _, secret := range c.secrets {
			if strings.Contains(got, secret) {
				t.Fatalf("secret survived in %q", got)
			}
		}
	}
}
func TestNestedSlogAndMessageRedaction(t *testing.T) {
	var b bytes.Buffer
	l := NewWithWriter(&b)
	l.Error("error password=message-secret", slog.Any("nested", map[string]any{"items": []any{map[string]any{"AdminApiKey": "api-secret"}}, "error": errors.New(`{"refresh_token":"refresh-secret"}`)}), slog.Group("Credentials", slog.String("unknownName", "unknown-secret")), slog.String("TOTP_SECRET", "totp-secret"))
	for _, secret := range []string{"message-secret", "api-secret", "refresh-secret", "unknown-secret", "totp-secret"} {
		if strings.Contains(b.String(), secret) {
			t.Fatal("secret in structured log")
		}
	}
	var decoded map[string]any
	if json.Unmarshal(b.Bytes(), &decoded) != nil {
		t.Fatal("invalid JSON log")
	}
}
func TestRedactValueDoesNotMutateAndHandlesCycles(t *testing.T) {
	in := map[string]any{"Password": "original", "other": "usable"}
	out := RedactValue(in).(map[string]any)
	if in["Password"] != "original" || out["Password"] != Redacted {
		t.Fatal("mutation or leak")
	}
	in["cycle"] = in
	if _, e := json.Marshal(RedactValue(in)); e != nil {
		t.Fatal(e)
	}
}
func TestSafeErrorRedactsBeforeTruncating(t *testing.T) {
	secret := strings.Repeat("secret", 200)
	out := SafeError(errors.New(`error: {"password":"` + secret + `"}`))
	if strings.Contains(out, "secret") {
		t.Fatal("secret leaked through truncation")
	}
}
