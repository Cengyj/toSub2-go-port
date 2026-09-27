package protocol

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"tosub2-go-port/internal/transport"
)

func TestTOTPEnrollmentRejectsBadSeedBeforePublication(t *testing.T) {
	published := false
	f := enrollmentFlow(t, doerFunc(func(_ context.Context, r transport.Request) (*transport.Response, error) {
		switch {
		case strings.Contains(r.URL, "?action=enable"):
			return response(200, "{\"accessToken\":\"synthetic-access-token-at-least-twenty-characters\"}"), nil
		case strings.HasSuffix(r.URL, "/mfa_info"):
			return response(200, "{}"), nil
		case strings.HasSuffix(r.URL, "/mfa/enroll"):
			return response(200, "{\"secret\":\"invalid-seed-contains-zero0\",\"session_id\":\"s\"}"), nil
		default:
			t.Error("invalid enrollment was activated")
			return nil, errors.New("unexpected request")
		}
	}), FlowOptions{TOTPEnrollment: func(context.Context, string, string) error { published = true; return nil }})
	if _, err := f.SetupTOTP(context.Background(), ""); err == nil || !strings.Contains(err.Error(), "TOTP_ENROLL_INVALID") || published {
		t.Fatalf("unvalidated seed published: %v", err)
	}
}

func TestTOTPEnrollmentRejectsMalformedManualCodesLocally(t *testing.T) {
	calls, asked := 0, 0
	f := enrollmentFlow(t, doerFunc(func(_ context.Context, r transport.Request) (*transport.Response, error) {
		switch {
		case strings.Contains(r.URL, "?action=enable"):
			return response(200, "{\"accessToken\":\"synthetic-access-token-at-least-twenty-characters\"}"), nil
		case strings.HasSuffix(r.URL, "/mfa_info"):
			return response(200, "{}"), nil
		case strings.HasSuffix(r.URL, "/mfa/enroll"):
			return response(200, "{\"secret\":\"JBSWY3DPEHPK3PXP\",\"session_id\":\"s\"}"), nil
		case strings.HasSuffix(r.URL, "/activate_enrollment"):
			calls++
			var body map[string]string
			if err := json.Unmarshal(r.Body, &body); err != nil {
				t.Fatal(err)
			}
			if !ParseTOTPCode(body["code"]) {
				t.Error("invalid manual code sent to activation API")
			}
			if calls == 1 {
				return response(400, "{}"), nil
			}
			if body["code"] != "123456" {
				t.Error("valid manual code not sent")
			}
			return response(200, "{\"success\":true}"), nil
		default:
			return nil, errors.New("unexpected request")
		}
	}), FlowOptions{Input: func(context.Context, string, string) (string, error) {
		values := []string{"bad", "12345", "123456"}
		value := values[asked]
		asked++
		return value, nil
	}})
	f.checkpoint.Stage = "phone_otp"
	f.checkpoint.OAuth = map[string]any{"state": "stale-oauth", "codeVerifier": "stale-verifier"}
	if _, err := f.SetupTOTP(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if calls != 2 || asked != 3 {
		t.Fatalf("activations=%d input=%d", calls, asked)
	}
	if f.checkpoint.Stage != "email_verified" || f.checkpoint.OAuth != nil {
		t.Fatal("completed enrollment retained consumed OAuth state")
	}
}

func TestTOTPSeedNormalizationAndCodeValidation(t *testing.T) {
	a, err := TOTPAtUnix("JBSW Y3DP\tEHPK\n3PXP=", 59)
	if err != nil {
		t.Fatal(err)
	}
	b, err := TOTPAtUnix("JBSWY3DPEHPK3PXP", 59)
	if err != nil || a != b {
		t.Fatal("legacy whitespace normalization changed")
	}
	if _, err := DecodeBase32Secret(strings.Repeat("A", 129)); err == nil {
		t.Fatal("oversized secret accepted")
	}
	for _, code := range []string{"-12345", "+12345", "12345", "1234567", "12 456"} {
		if ParseTOTPCode(code) {
			t.Errorf("invalid TOTP accepted: %s", code)
		}
	}
}
