package protocol

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"tosub2-go-port/internal/transport"
)

func TestPasswordPromptExplainsMissingOrRejectedCredential(t *testing.T) {
	for _, tc := range []struct {
		name, candidate, reason string
		responseStatus          int
		wantInputs, wantPosts   int
	}{
		{name: "imported-password-is-automatic", candidate: "Synthetic-Candidate9!", responseStatus: 200, wantPosts: 1},
		{name: "missing-password", reason: "没有可用密码", wantInputs: 1},
		{name: "rejected-password", candidate: "Synthetic-Candidate9!", responseStatus: 401, reason: "拒绝了已提交的账号密码", wantInputs: 1, wantPosts: 1},
		{name: "temporary-server-error-does-not-request-password", candidate: "Synthetic-Candidate9!", responseStatus: 503, wantPosts: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inputs, posts := 0, 0
			client := NewClient(doerFunc(func(_ context.Context, req transport.Request) (*transport.Response, error) {
				posts++
				if req.Method != http.MethodPost || req.URL != "https://auth.example.test/api/accounts/password/verify" {
					t.Fatal("unexpected password verification request")
				}
				var body map[string]string
				if json.Unmarshal(req.Body, &body) != nil || body["password"] != tc.candidate {
					t.Fatal("imported password was not preserved for verification")
				}
				if tc.responseStatus == 401 {
					return response(401, `{"error":{"code":"invalid_password"}}`), nil
				}
				return response(tc.responseStatus, `{"continue_url":"https://chat.example.test/"}`), nil
			}), nil, "auto")
			client.ConnectionRetries = 0
			flow := NewFlow(client, FlowOptions{AuthBase: "https://auth.example.test", Password: tc.candidate,
				Input: func(_ context.Context, stage, prompt string) (string, error) {
					inputs++
					if stage != "password" || tc.reason == "" || !strings.Contains(prompt, tc.reason) {
						t.Fatal("password prompt omitted the reason for manual input")
					}
					if tc.candidate != "" && strings.Contains(prompt, tc.candidate) {
						t.Fatal("password leaked into the manual input prompt")
					}
					return "", context.Canceled
				},
				Sentinel: func(context.Context, string, string, string) (http.Header, error) {
					return http.Header{"Openai-Sentinel-Token": []string{"synthetic-sentinel-token"}}, nil
				},
			})
			_, err := flow.verifyPassword(context.Background(), "https://auth.example.test/log-in/password")
			if tc.wantInputs > 0 && !errors.Is(err, context.Canceled) {
				t.Fatal("manual input cancellation did not stop password verification")
			}
			if tc.responseStatus == 200 && err != nil {
				t.Fatal(err)
			}
			if posts != tc.wantPosts || inputs != tc.wantInputs {
				t.Fatalf("posts=%d inputs=%d; want posts=%d inputs=%d", posts, inputs, tc.wantPosts, tc.wantInputs)
			}
		})
	}
}
