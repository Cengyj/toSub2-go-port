package mail

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func TestLegacyFoldedBase64MailboxContent(t *testing.T) {
	encoded := base64.StdEncoding.EncodeToString([]byte("Your OpenAI account verification code is 654321. Enter this code to continue signing in."))
	for name, separator := range map[string]string{"LF": "\n", "CRLF": "\r\n", "tabs": "\t", "mixed-whitespace": " \r\n\t"} {
		t.Run(name, func(t *testing.T) {
			// MIME/body APIs may fold or indent encoded text anywhere. Legacy
			// textVariants checks the compacted text, including its padding.
			folded := "  " + encoded[:76] + separator + encoded[76:] + "  "
			raw, err := json.Marshal(map[string]any{"id": "synthetic-message", "body": folded})
			if err != nil {
				t.Fatal(err)
			}
			got := ExtractCandidates(string(raw))
			if len(got) != 1 || got[0].Code != "654321" {
				t.Fatalf("folded Base64 message was not decoded: %#v", got)
			}
			plain, _ := json.Marshal(map[string]any{"id": "synthetic-message", "body": "Your OpenAI account verification code is 654321. Enter this code to continue signing in."})
			want := ExtractCandidates(string(plain))
			if got[0].Key != want[0].Key {
				t.Fatal("folded and plain versions of the same message must share the deduplication key")
			}
		})
	}
	if got := ExtractCandidates(strings.Repeat("!", 32)); len(got) != 0 {
		t.Fatal("invalid Base64 unexpectedly produced a code")
	}
}
