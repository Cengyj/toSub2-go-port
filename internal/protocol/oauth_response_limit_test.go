package protocol

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"tosub2-go-port/internal/transport"
)

func TestProtocolDoPreservesCallerResponseLimit(t *testing.T) {
	for _, single := range []bool{false, true} {
		for _, status := range []int{http.StatusOK, http.StatusForbidden} {
			t.Run(fmt.Sprintf("single=%t/status=%d", single, status), func(t *testing.T) {
				var calls, challenges atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					w.Header().Set("Set-Cookie", "accepted=synthetic; Path=/")
					w.Header().Set("Cf-Mitigated", "challenge")
					w.WriteHeader(status)
					fmt.Fprint(w, strings.Repeat("synthetic-private-body", 100))
				}))
				defer server.Close()
				jar := transport.NewCookieJar()
				native := transport.NewClient(jar)
				defer native.Close()
				client := NewClient(native, jar, "auto")
				client.ConnectionRetries = 3
				client.Challenge = func(_ context.Context, _ string) error { challenges.Add(1); return nil }
				response, err := client.Do(t.Context(), transport.Request{Method: http.MethodPost, URL: server.URL, SingleAttempt: single, MaxResponseBytes: 8})
				if !errors.Is(err, transport.ErrResponseTooLarge) || response == nil || response.StatusCode != status || len(response.Body) != 0 || calls.Load() != 1 || challenges.Load() != 0 {
					t.Fatalf("response limit was lost or mutation replayed: status=%v calls=%d challenges=%d error=%v", response.Status(), calls.Load(), challenges.Load(), err)
				}
				if !strings.Contains(jar.HeaderFor(server.URL), "accepted=synthetic") || strings.Contains(err.Error(), "synthetic-private-body") {
					t.Fatal("limited response lost accepted cookies or leaked response data")
				}
			})
		}
	}
}

func TestOAuthCodeAndRefreshResponsesAreBoundedWithoutReplay(t *testing.T) {
	for _, clientID := range []string{DefaultCodexClientID, DefaultBPSClientID} {
		for _, grant := range []string{"authorization_code", "refresh_token"} {
			t.Run(clientID+"/"+grant, func(t *testing.T) {
				var calls atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					if r.Method != http.MethodPost || r.URL.Path != "/oauth/token" {
						t.Error("unexpected token endpoint")
					}
					w.Header().Set("Content-Type", "application/json")
					fmt.Fprintf(w, `{"access_token":"synthetic-access","refresh_token":"synthetic-refresh","id_token":"synthetic-id","padding":%q}`, strings.Repeat("x", 1<<20))
				}))
				defer server.Close()
				native := transport.NewClient(nil)
				defer native.Close()
				client := NewClient(native, native.Jar, "auto")
				client.ConnectionRetries = 3
				var tokens TokenSet
				var err error
				if grant == "authorization_code" {
					tokens, err = ExchangeOAuthCode(t.Context(), client, server.URL, clientID, "synthetic-code", "synthetic-verifier", DefaultCodexRedirectURI)
				} else {
					tokens, err = RefreshOAuthToken(t.Context(), client, server.URL, clientID, "synthetic-refresh")
				}
				if !errors.Is(err, transport.ErrResponseTooLarge) || tokens.AccessToken != "" || calls.Load() != 1 {
					t.Fatalf("unbounded OAuth response accepted or replayed: calls=%d error=%v", calls.Load(), err)
				}
			})
		}
	}
}
