package logging

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

func TestMailRequestHeadersArePrivateRegardlessOfHeaderName(t *testing.T) {
	const canary = "synthetic-mail-credential"
	for _, key := range []string{"mailRequestHeaders", "mail_request_headers", "MAIL-REQUEST-HEADERS"} {
		input := map[string]any{key: map[string]string{"X-Mail-Access": canary}, "stage": "email_otp"}
		data, err := json.Marshal(input)
		if err != nil {
			t.Fatal(err)
		}
		var output bytes.Buffer
		NewWithWriter(&output).Warn("mail read failed", slog.Any("request", input), slog.Group(key, slog.String("X-Mail-Access", canary)))
		for _, got := range []string{RedactText(string(data)), output.String()} {
			if strings.Contains(got, canary) || !strings.Contains(got, "email_otp") {
				t.Fatal("mail header redaction leaked credential or removed diagnostic stage")
			}
		}
		if input[key].(map[string]string)["X-Mail-Access"] != canary {
			t.Fatal("redaction changed request headers")
		}
	}
}
