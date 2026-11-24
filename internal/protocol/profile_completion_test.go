package protocol

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"tosub2-go-port/internal/transport"
)

func TestProfileCompletionReportsMutationOutcomeWithoutReplaying(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     int
		body       string
		requestErr error
		want       string
	}{
		{"completed", 200, `{"page":{"type":"external_url"},"continue_url":"https://chat.example/api/auth/callback/openai?code=synthetic"}`, nil, "profile_completed"},
		{"lost", 0, "", io.EOF, "profile_unknown"},
		{"canceled-after-send", 0, "", context.Canceled, "profile_unknown"},
		{"server-failure", 500, `{}`, nil, "profile_unknown"},
		{"timeout-status", 408, `{}`, nil, "profile_unknown"},
		{"challenge", 503, `<html><script src='/cdn-cgi/challenge-platform/test.js'></script></html>`, nil, "profile_unknown"},
		{"rejected", 400, `{"error":{"code":"registration_disallowed"}}`, nil, "profile_rejected"},
		{"expired-rejection", 401, `{"error":{"code":"session_expired"}}`, nil, "profile_rejected"},
		{"unadvanced", 200, `{"page":{"type":"about_you"}}`, nil, "profile_unknown"},
		{"duplicate", 200, `{"page":{"type":"about_you","type":"external_url"},"continue_url":"https://chat.example/api/auth/callback/openai"}`, nil, "profile_unknown"},
		{"contradictory", 200, `{"success":false,"page":{"type":"external_url"},"continue_url":"https://chat.example/api/auth/callback/openai"}`, nil, "profile_unknown"},
		{"zero-success", 200, `{"success":0,"page":{"type":"external_url"},"continue_url":"https://chat.example/api/auth/callback/openai"}`, nil, "profile_unknown"},
		{"null-success", 200, `{"success":null,"page":{"type":"external_url"},"continue_url":"https://chat.example/api/auth/callback/openai"}`, nil, "profile_unknown"},
		{"wrong-origin", 200, `{"page":{"type":"external_url"},"continue_url":"https://outside.example/api/auth/callback/openai"}`, nil, "profile_unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls, challenges := 0, 0
			stages := []string{}
			client := NewClient(doerFunc(func(_ context.Context, request transport.Request) (*transport.Response, error) {
				calls++
				if request.Method != http.MethodPost || !strings.HasSuffix(request.URL, "/api/accounts/create_account") || !request.SingleAttempt {
					t.Fatal("unexpected profile request")
				}
				var body map[string]any
				if json.Unmarshal(request.Body, &body) != nil || body["name"] != "Private Fixture" || body["birthdate"] != "1990-01-01" {
					t.Fatal("provided profile changed")
				}
				if tc.status == 0 {
					return nil, tc.requestErr
				}
				return response(tc.status, tc.body), tc.requestErr
			}), nil, "")
			client.ConnectionRetries, client.RetryDelay = 2, 0
			client.Challenge = func(context.Context, string) error { challenges++; return nil }
			flow := NewFlow(client, FlowOptions{ChatGPTBase: "https://chat.example", AuthBase: "https://auth.example", Sentinel: fakeSentinel,
				Profile: map[string]any{"name": "Private Fixture", "birthdate": "1990-01-01"},
				EventData: func(stage, status, prompt string, data map[string]any) {
					stages = append(stages, stage)
					encoded, _ := json.Marshal(data)
					for _, secret := range []string{"Private Fixture", "1990-01-01", "synthetic"} {
						if strings.Contains(prompt+string(encoded), secret) {
							t.Fatal("profile event disclosed private material")
						}
					}
				},
			})
			_, err := flow.completeProfile(t.Context(), authPayload{"page": map[string]any{"type": "about_you"}})
			if (err == nil) != (tc.want == "profile_completed") || calls != 1 || challenges != 0 || !reflect.DeepEqual(stages, []string{"profile_required", "profile_submitting", tc.want}) {
				t.Fatalf("profile outcome calls=%d challenges=%d stages=%v err=%v", calls, challenges, stages, err)
			}
			if err != nil && isExpiredSession(err) {
				t.Fatal("profile submission failure authorized another login")
			}
			if tc.requestErr == context.Canceled && !errors.Is(err, context.Canceled) {
				t.Fatal("profile outcome discarded cancellation")
			}
		})
	}
}

func TestProfileCompletionPreparationAndTriggerStayNarrow(t *testing.T) {
	flow := NewFlow(doNotUseClient(), FlowOptions{Sentinel: func(context.Context, string, string, string) (http.Header, error) {
		return nil, errors.New("private-sentinel-detail")
	}})
	for _, current := range []authPayload{nil, {"error": "about_you"}, {"page": map[string]any{"type": "login_password"}}, {"continue_url": "https://auth.example/ordinary-error"}} {
		if _, err := flow.completeProfile(t.Context(), current); err != nil {
			t.Fatal("ordinary login response triggered profile handling", err)
		}
	}
	var stages []string
	flow.Options.Event = func(stage, _, _ string) { stages = append(stages, stage) }
	_, err := flow.completeProfile(t.Context(), authPayload{"page": map[string]any{"type": "about_you"}})
	var failure *profileCompletionError
	if !errors.As(err, &failure) || failure.outcome != profileNotSent || strings.Contains(err.Error(), "private-sentinel-detail") || !reflect.DeepEqual(stages, []string{"profile_required", "profile_not_sent"}) {
		t.Fatalf("preparation failure classification: %v %v", stages, err)
	}
}
