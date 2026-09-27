package protocol

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"tosub2-go-port/internal/transport"
)

// protocol-login.mjs:267-276 always JSON.parse's getJson responses, including
// /api/auth/providers at :991-993 where the caller ignores the returned data.
func TestLegacyJSONRequestValidatesIgnoredResponse(t *testing.T) {
	for _, test := range []struct {
		name, body string
		valid      bool
	}{
		{"object", "{}", true},
		{"array", "[]", true},
		{"null", "null", true},
		{"html", "<html>fixture-private-response</html>", false},
		{"empty", "", false},
		{"trailing_document", "{}{}", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()
			client := NewClient(transport.NewClient(nil), nil, "")
			response, err := client.JSON(context.Background(), http.MethodGet, server.URL+"/api/auth/providers", nil, RequestOptions{}, nil)
			if (err == nil) != test.valid || response == nil || response.StatusCode != http.StatusOK {
				t.Fatalf("ignored JSON response validation mismatch: valid=%v err=%v", test.valid, err)
			}
			if err != nil && strings.Contains(err.Error(), "fixture-private-response") {
				t.Fatal("invalid JSON error leaked the response body")
			}
		})
	}
}
