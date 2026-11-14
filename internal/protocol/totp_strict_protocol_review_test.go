package protocol

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"tosub2-go-port/internal/transport"
)

func TestStrictProtocolReviewCompleteResponseKeepsReceiptAndCookies(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Set-Cookie", "strict-cookie=accepted; Path=/; HttpOnly")
		_, _ = w.Write([]byte(`{"success":true}`))
	}))
	defer server.Close()
	jar := transport.NewCookieJar()
	raw := transport.NewClient(jar)
	defer raw.Close()
	client := NewClient(doerFunc(func(ctx context.Context, request transport.Request) (*transport.Response, error) {
		r, err := raw.Do(ctx, request)
		if err != nil {
			return r, err
		}
		return r, io.EOF // A late transport failure cannot erase a complete body.
	}), jar, "")
	flow := NewFlow(client, FlowOptions{Email: "strict@example.test", ChatGPTBase: server.URL})
	out, err := flow.totpResetRequest(t.Context(), "synthetic-token", "synthetic-account", http.MethodPost, "/backend-api/accounts/mfa/user/activate_enrollment", map[string]any{"code": "123456"})
	if err != nil || out["success"] != true || calls.Load() != 1 || jar.Value("strict-cookie", server.URL) != "accepted" {
		t.Fatal("complete receipt or current-session Cookie was lost", err)
	}
}
