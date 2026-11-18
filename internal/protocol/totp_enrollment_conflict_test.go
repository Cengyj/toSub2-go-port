package protocol

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// These are compatibility examples for the explicit-refusal contract. They
// are synthetic responses, not a claim about a captured upstream error code.
func TestTOTPEnrollmentConflictRecognizesExplicitRefusal(t *testing.T) {
	for _, body := range []string{
		`{"error":{"code":"totp_already_enrolled"}}`,
		`{"request_id":"private-correlation-canary","error":{"code":"totp_already_enrolled"}}`,
		`{"error":"totp_already_enabled"}`,
		`{"code":"totp_already_exists"}`,
		`{"error":{"code":"mfa_already_enrolled"}}`,
		`{"error":{"code":"mfa_already_enabled","message":"MFA is already enabled."}}`,
		`{"code":"mfa_already_exists","success":false}`,
		`{"error":"factor_already_enrolled","activated":false}`,
		`{"error":"factor_already_exists"}`,
		`{"message":"TOTP is already enabled."}`,
		`{"detail":"A TOTP factor already exists"}`,
		`{"detail":{"error":{"code":"totp_already_enrolled"}}}`,
		`{"detail":{"code":"mfa_already_enabled","message":"MFA is already enabled"}}`,
		`{"detail":"{\"error\":{\"code\":\"totp_already_enrolled\"}}"}`,
		`{"error":"totp_already_enrolled","message":null,"detail":" ","success":false}`,
	} {
		t.Run(body, func(t *testing.T) {
			result, valid := decodeTOTPResponse([]byte(body))
			if !valid || !totpEnrollmentAlreadyExists(result) {
				t.Fatal("explicit refusal was not recognized")
			}
			projected := totpReceiptBody([]byte(body))
			stored, valid := decodeTOTPResponse(projected)
			if !valid || !totpEnrollmentAlreadyExists(stored) {
				t.Fatal("explicit refusal did not survive durable projection")
			}
			want := map[string]any{"error": map[string]any{"code": totpEnrollmentExistsCode}}
			if !reflect.DeepEqual(stored, want) {
				t.Fatalf("projection retained more than normalized refusal: %s", projected)
			}
			if classifyTOTPMutation("disable", stored, "", "") != totpMutationRejected || classifyTOTPMutation("activation", stored, "", "") != totpMutationRejected {
				t.Fatal("projection changed a refusal into an accepted mutation")
			}
		})
	}
}

func TestTOTPEnrollmentConflictNeverPromotesAmbiguousResponse(t *testing.T) {
	for _, body := range []string{
		`{}`, `null`, `[]`, `{"status":409}`, `{"error":"conflict"}`, `{"error":"already_exists"}`,
		`{"message":"already enabled"}`, `{"message":"TOTP is already enabled, or the session expired"}`,
		`{"message":"TOTP is not already enabled"}`, `{"message":"If TOTP is already enabled, remove it first"}`,
		`{"message":"TOTP is already enabled. Extra account material: private-canary"}`,
		`{"error":"totp_already_enrolled","success":true}`,
		`{"error":"totp_already_enrolled","activated":true}`,
		`{"error":"totp_already_enrolled","success":"false"}`,
		`{"error":"totp_already_enrolled","success":null}`,
		`{"error":"totp_already_enrolled","code":"recent_auth_required"}`,
		`{"code":"TOTP is already enabled","message":"MFA is already enrolled"}`,
		`{"error":{"code":"totp_already_enrolled","message":"Invalid code"}}`,
		`{"error":"totp_already_enrolled","message":"Factor disabled"}`,
		`{"error":"totp_already_enrolled","detail":{"error":"invalid_code"}}`,
		`{"error":"totp_already_enrolled","detail":{"success":true}}`,
		`{"error":{"code":"totp_already_enrolled","data":{"secret":"private-canary"}}}`,
		`{"error":"totp_already_enrolled","data":{"secret":"private-canary"}}`,
		`{"data":{"error":"totp_already_enrolled"}}`,
		`{"detail":{"detail":{"error":"totp_already_enrolled"}}}`,
		`{"detail":[{"code":"totp_already_enrolled"}]}`,
		`{"error":["totp_already_enrolled"]}`,
		`{"error":"totp_already_enrolled","secret":"seed"}`,
		`{"error":"totp_already_enrolled","session_id":"session"}`,
		`{"error":"totp_already_enrolled","factor":{"id":"factor"}}`,
		`{"error":"totp_already_enrolled","factor_id":"factor"}`,
		`{"error":"totp_already_enrolled","otpauth_uri":"uri"}`,
		`{"error":"totp_already_enrolled","error":"invalid_code"}`,
		`{"error":{"code":"invalid_code","code":"totp_already_enrolled"}}`,
		`{"detail":"{\"error\":\"invalid_code\",\"error\":\"totp_already_enrolled\"}"}`,
		`{"error":"totp_already_enrolled"`, `{"error":"totp_already_enrolled"} {}`,
	} {
		t.Run(body, func(t *testing.T) {
			result, valid := decodeTOTPResponse([]byte(body))
			if valid && totpEnrollmentAlreadyExists(result) {
				t.Fatal("ambiguous response authorized conflict fallback")
			}
			projection := totpReceiptBody([]byte(body))
			stored, valid := decodeTOTPResponse(projection)
			if valid && totpEnrollmentAlreadyExists(stored) {
				t.Fatalf("projection promoted ambiguous response: %s", projection)
			}
			if strings.Contains(string(projection), "private-canary") {
				t.Fatal("projection retained unknown private response content")
			}
		})
	}
}

func TestTOTPEnrollmentConflictRejectsEvenEmptyMaterial(t *testing.T) {
	for _, key := range []string{"secret", "session_id", "factor", "factor_id", "otpauth_uri", "new_totp_factor_id", "enrollment_factor"} {
		for _, value := range []any{nil, "", false, map[string]any{}, []any{}} {
			result := map[string]any{"error": totpEnrollmentExistsCode, key: value}
			body, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			if totpEnrollmentAlreadyExists(result) {
				t.Fatalf("material %s=%v did not block fallback", key, value)
			}
			projected, valid := decodeTOTPResponse(totpReceiptBody(body))
			if valid && totpEnrollmentAlreadyExists(projected) {
				t.Fatalf("projection erased material %s=%v and authorized fallback", key, value)
			}
		}
	}
}
