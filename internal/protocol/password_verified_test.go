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

func TestPasswordVerifiedCommitsOnlyAcceptedPassword(t *testing.T) {
	storageErr := errors.New("fixture password storage unavailable")
	for _, test := range []struct {
		name                                      string
		rejectFirst, cancelManual, storageFailure bool
		wantHooks                                 int
		wantPassword                              string
		manualPassword                            string
	}{
		{name: "accepted-candidate", wantHooks: 1, wantPassword: "Candidate-Password9!"},
		{name: "rejected-candidate", rejectFirst: true, cancelManual: true},
		{name: "manual-password-after-rejection", rejectFirst: true, wantHooks: 1, wantPassword: "Manual-Password9!"},
		{name: "manual-whitespace-preserved", rejectFirst: true, wantHooks: 1, wantPassword: " \tManual-Password9! ", manualPassword: " \tManual-Password9! "},
		{name: "save-failure", storageFailure: true, wantHooks: 1, wantPassword: "Candidate-Password9!"},
	} {
		t.Run(test.name, func(t *testing.T) {
			attempts, hooks := 0, 0
			acceptedPassword := ""
			doer := doerFunc(func(ctx context.Context, r transport.Request) (*transport.Response, error) {
				if !strings.HasSuffix(r.URL, "/password/verify") {
					t.Error("unexpected password request")
					return nil, errors.New("unexpected request")
				}
				attempts++
				if test.rejectFirst && attempts == 1 {
					return response(http.StatusUnauthorized, `{"error":{"code":"invalid_password"}}`), nil
				}
				var body map[string]any
				if err := json.Unmarshal(r.Body, &body); err != nil {
					t.Fatal(err)
				}
				acceptedPassword, _ = body["password"].(string)
				return response(http.StatusOK, `{"continue_url":"https://chat.example/"}`), nil
			})
			flow := NewFlow(NewClient(doer, nil, "auto"), FlowOptions{Email: "owner@example.test", AuthBase: "https://auth.example", Password: "Candidate-Password9!", Input: func(context.Context, string, string) (string, error) {
				if test.cancelManual {
					return "", context.Canceled
				}
				if test.manualPassword != "" {
					return test.manualPassword, nil
				}
				return "Manual-Password9!", nil
			}, Sentinel: func(context.Context, string, string, string) (http.Header, error) {
				return http.Header{"Openai-Sentinel-Token": []string{"fixture-sentinel"}}, nil
			}, PasswordVerified: func(ctx context.Context, password string) error {
				hooks++
				if password != acceptedPassword || password != test.wantPassword {
					t.Error("callback did not receive the password accepted by the server")
				}
				if test.storageFailure {
					return storageErr
				}
				return nil
			}})
			_, err := flow.verifyPassword(context.Background(), "https://auth.example/log-in/password")
			switch {
			case test.storageFailure:
				if !errors.Is(err, storageErr) {
					t.Fatalf("storage error lost: %v", err)
				}
			case test.cancelManual:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("rejected password did not request replacement: %v", err)
				}
			default:
				if err != nil {
					t.Fatal(err)
				}
			}
			if hooks != test.wantHooks {
				t.Fatalf("password commit calls=%d want=%d", hooks, test.wantHooks)
			}
		})
	}
}

func TestCodeInputStillTrimsWhitespace(t *testing.T) {
	for _, stage := range []string{"email_otp", "sms_otp", "totp", "phone"} {
		t.Run(stage, func(t *testing.T) {
			flow := NewFlow(NewClient(nil, nil, "auto"), FlowOptions{Input: func(context.Context, string, string) (string, error) { return " \t123456 \n", nil }})
			value, err := flow.ask(context.Background(), stage, "fixture input")
			if err != nil || value != "123456" {
				t.Fatalf("code normalization failed: err=%v", err)
			}
		})
	}
}
