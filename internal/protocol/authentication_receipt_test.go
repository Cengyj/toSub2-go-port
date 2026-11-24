package protocol

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"tosub2-go-port/internal/transport"
)

func TestRejectedEmailReceiptCannotTriggerProfileOrOAuth(t *testing.T) {
	for _, reply := range []string{
		`{"success":false,"page":{"type":"about_you"}}`,
		`{"success":"false","page":{"type":"about_you"}}`,
		`{"error":{"code":"session_expired","message":"private-echo"},"page":{"type":"about_you"}}`,
		`{"error":{},"continue_url":"https://auth.example/about-you"}`,
	} {
		t.Run(reply, func(t *testing.T) {
			h := newRecoveryHarness(t)
			verifications, profileWrites := 0, 0
			h.intercept = func(_ context.Context, request transport.Request) (*transport.Response, error, bool) {
				switch pathOf(request.URL) {
				case "/api/accounts/email-otp/validate":
					verifications++
					return response(http.StatusOK, reply), nil, true
				case "/api/accounts/create_account":
					profileWrites++
					return response(http.StatusOK, `{}`), nil, true
				}
				return nil, nil, false
			}
			_, err := h.flow.Run(t.Context())
			var receipt *authenticationReceiptError
			if !errors.As(err, &receipt) || receipt.code != "EMAIL_OTP_RESULT_REJECTED" || verifications != 1 || profileWrites != 0 || h.oauthRequests != 0 || h.tokenRequests != 0 || h.resets != 0 || h.loginStarts != 1 {
				t.Fatalf("rejected email response authorized subsequent work: calls=%d profiles=%d oauth=%d resets=%d err=%v", verifications, profileWrites, h.oauthRequests, h.resets, err)
			}
			if strings.Contains(err.Error(), "private-echo") {
				t.Fatal("provider payload entered public error")
			}
		})
	}
}

func TestRejectedEmailResendDoesNotAdvanceMailboxBaseline(t *testing.T) {
	requests, baselines, sent := 0, 0, 0
	f := NewFlow(NewClient(doerFunc(func(_ context.Context, request transport.Request) (*transport.Response, error) {
		requests++
		if pathOf(request.URL) != "/api/accounts/email-otp/resend" {
			t.Fatal("rejected resend reached validation")
		}
		return response(http.StatusOK, `{"success":false}`), nil
	}), nil, ""), FlowOptions{
		Input:             func(context.Context, string, string) (string, error) { return "r", nil },
		BeforeEmailOTP:    func(context.Context) error { baselines++; return nil },
		EmailOTPRequested: func(_ time.Time) { sent++ },
	})
	_, err := f.verifyEmail(t.Context(), f.Options.AuthBase+"/email-verification")
	if err == nil || requests != 1 || baselines != 1 || sent != 0 {
		t.Fatal("rejected resend was reported sent or replayed", requests, baselines, sent, err)
	}
}

func TestPhoneReceiptRequiresBusinessConfirmationBeforeCompletingOrder(t *testing.T) {
	for _, tc := range []struct {
		name, operation, reply string
	}{
		{"send-empty", "send", `{}`},
		{"send-rejected", "send", `{"success":false,"continue_url":"/phone-verification"}`},
		{"send-error", "send", `{"error":{"message":"private-echo"},"page":{"type":"phone_otp_verification"}}`},
		{"send-outside-origin", "send", `{"continue_url":"https://outside.example/phone-verification"}`},
		{"send-contradiction", "send", `{"page":{"type":"login_password"},"continue_url":"/phone-verification"}`},
		{"validate-empty", "validate", `{}`},
		{"validate-rejected", "validate", `{"success":false,"verified":true}`},
		{"validate-error", "validate", `{"error":{"message":"private-echo"},"verified":true}`},
		{"validate-contradiction", "validate", `{"verified":false,"continue_url":"/workspace"}`},
		{"validate-unadvanced", "validate", `{"verified":true,"continue_url":"/phone-verification"}`},
		{"validate-outside-origin", "validate", `{"verified":true,"continue_url":"https://outside.example/workspace"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sends, validations, inputs, completed, deferred := 0, 0, 0, 0, 0
			f := NewFlow(NewClient(doerFunc(func(_ context.Context, request transport.Request) (*transport.Response, error) {
				switch pathOf(request.URL) {
				case "/add-phone":
					return response(http.StatusOK, "phone"), nil
				case "/api/accounts/add-phone/send":
					sends++
					if tc.operation == "send" {
						return response(http.StatusOK, tc.reply), nil
					}
					return response(http.StatusOK, `{"continue_url":"/phone-verification"}`), nil
				case "/api/accounts/phone-otp/validate":
					validations++
					return response(http.StatusOK, tc.reply), nil
				default:
					t.Fatal("unconfirmed phone response reached later endpoint")
					return nil, context.Canceled
				}
			}), nil, ""), FlowOptions{AuthBase: "https://auth.example", Phone: "+12025550123",
				Input: func(_ context.Context, stage, _ string) (string, error) {
					inputs++
					if tc.operation == "validate" && inputs == 1 {
						if stage != "sms_otp" {
							t.Fatal("wrong input before validation")
						}
						return "123456", nil
					}
					want := "sms_otp"
					if tc.operation == "send" {
						want = "phone"
					}
					if stage != want || deferred != 1 {
						t.Fatal("unconfirmed write did not wait for explicit manual recovery")
					}
					return "", context.Canceled
				},
				EventData: func(stage, _, prompt string, data map[string]any) {
					if stage == "phone_verified" {
						completed++
					}
					if data["phoneRetryRequired"] == true {
						deferred++
					}
					if strings.Contains(prompt, "private-echo") {
						t.Fatal("raw response leaked")
					}
				},
			})
			_, err := f.bindPhone(t.Context(), map[string]any{"addPhoneUrl": "https://auth.example/add-phone"})
			wantValidations := 0
			if tc.operation == "validate" {
				wantValidations = 1
			}
			if !errors.Is(err, context.Canceled) || sends != 1 || validations != wantValidations || completed != 0 || deferred != 1 {
				t.Fatalf("unconfirmed phone reply completed or replayed write sends=%d validates=%d complete=%d err=%v", sends, validations, completed, err)
			}
		})
	}
}
