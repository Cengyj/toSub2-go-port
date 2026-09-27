package protocol

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"tosub2-go-port/internal/transport"
)

// Legacy protocol-login.mjs:1614-1646 and 2186-2193 retry an HTTP 400/409
// add-phone/send once without channel when the endpoint describes channel or
// session state in its message. Error-code-only fixtures missed these replies.
func TestLegacyPhoneSendMessageFallbackContract(t *testing.T) {
	for _, message := range []string{
		"The channel parameter is not supported.",
		"This session cannot use the requested delivery method.",
		"The authorization step is no longer valid for the selected channel.",
	} {
		t.Run(message, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.Method != http.MethodPost || r.URL.Path != "/api/accounts/add-phone/send" {
					t.Error("unexpected phone request")
				}
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if body["phone_number"] != "+12025550123" {
					t.Error("fallback changed the authorized phone")
				}
				w.Header().Set("Content-Type", "application/json")
				if _, hasChannel := body["channel"]; hasChannel {
					w.WriteHeader(http.StatusBadRequest)
					_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": message + " fixture-private-echo"}})
					return
				}
				if len(body) != 1 {
					t.Error("fallback removed or added fields other than channel")
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"continue_url": "/phone-verification"})
			}))
			defer server.Close()
			flow := NewFlow(NewClient(transport.NewClient(nil), nil, ""), FlowOptions{AuthBase: server.URL})
			result, err := flow.sendPhone(context.Background(), "+12025550123", server.URL+"/add-phone")
			if err != nil || continuation(result) != "/phone-verification" || requests.Load() != 2 {
				t.Fatalf("legacy message fallback did not reach phone verification: requests=%d err=%v", requests.Load(), err)
			}
		})
	}
}

func TestLegacyPhoneExpiredMessageSurvivesFallbackWithoutSecretEcho(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte("{\"error\":{\"message\":\"The sign-in session is no longer valid. fixture-private-echo\"}}"))
	}))
	defer server.Close()
	flow := NewFlow(NewClient(transport.NewClient(nil), nil, ""), FlowOptions{AuthBase: server.URL})
	_, err := flow.sendPhone(context.Background(), "+12025550123", server.URL+"/add-phone")
	if err == nil || !isExpiredSession(err) || requests.Load() != 2 {
		t.Fatalf("expired message lost bounded authorization recovery: requests=%d err=%v", requests.Load(), err)
	}
	if strings.Contains(err.Error(), "fixture-private-echo") || strings.Contains(err.Error(), "sign-in session") {
		t.Fatal("raw server error entered the public error string")
	}
}
