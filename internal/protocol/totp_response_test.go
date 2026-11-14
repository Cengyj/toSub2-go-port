package protocol

import (
	"encoding/json"
	"testing"
)

func totpOutcomeFixture(t *testing.T, raw string) map[string]any {
	t.Helper()
	var result map[string]any
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestDecodeTOTPResponseRejectsKnownObjectDuplicateFields(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"top-level outcome", `{"success":false,"success":true}`},
		{"enrollment factor", `{"success":true,"factor":{"id":"other-factor","id":"new-factor"}}`},
		{"status factor list", `{"mfa_enabled_v2":false,"factors":{"totp":[{"id":"old-factor"}],"totp":[]}}`},
		{"status factor identity", `{"mfa_enabled_v2":true,"factors":{"totp":[{"id":"other-factor","id":"new-factor","factor_type":"totp","is_recovery":false}]}}`},
		{"status factor recovery flag", `{"mfa_enabled_v2":true,"factors":{"totp":[{"id":"new-factor","factor_type":"totp","is_recovery":true,"is_recovery":false}]}}`},
		{"error code", `{"error":{"code":"forbidden","code":"invalid_code"}}`},
		{"detail code", `{"detail":{"code":"forbidden","code":"invalid_code"}}`},
		{"detail error envelope", `{"detail":{"error":{"code":"forbidden"},"error":{"code":"invalid_code"}}}`},
		{"detail nested error code", `{"detail":{"error":{"code":"forbidden","code":"invalid_code"}}}`},
		{"encoded detail code", `{"detail":"{\"code\":\"forbidden\",\"code\":\"invalid_code\"}"}`},
		{"encoded detail nested error code", `{"detail":"{\"error\":{\"code\":\"forbidden\",\"code\":\"invalid_code\"}}"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, valid := decodeTOTPResponse([]byte(tc.body))
			if valid || result != nil || totpInvalidCodeResponse(result) || classifyTOTPMutation("activation", result, "new-factor", "new-session") == totpMutationSucceeded {
				t.Fatal("contradictory duplicate fields authorized success or code retry")
			}
		})
	}
}

func TestDecodeTOTPResponseLeavesUnknownTypesAndUnrelatedObjectsIntact(t *testing.T) {
	for _, body := range []string{
		`{"success":true,"extra":{"success":false,"success":true}}`,
		`{"success":true,"passkey_credential_manager_sync":null}`,
		`{"factor":null}`, `{"factor":[]}`, `{"factor":"future-shape"}`,
		`{"factors":[]}`, `{"factors":{"totp":null}}`, `{"factors":{"totp":"future-shape"}}`,
		`{"factors":{"totp":[null,3,"future-shape"]}}`,
		`{"error":true}`, `{"error":["future-shape"]}`, `{"detail":"plain refusal"}`,
		`{"detail":{"error":{"code":"invalid_code"},"extra":{"code":"one","code":"two"}}}`,
	} {
		if result, valid := decodeTOTPResponse([]byte(body)); !valid || result == nil {
			t.Fatal("decoder discarded parseable unknown structure or unrelated fields")
		}
	}
	for _, factor := range []string{`null`, `[]`, `"future-shape"`} {
		body := `{"secret":"` + resetFixtureSecret + `","session_id":"new-session","factor":` + factor + `}`
		result, valid := decodeTOTPResponse([]byte(body))
		if !valid {
			t.Fatal("unrecognized factor type discarded available enrollment material")
		}
		out := map[string]any{}
		_, _, _, err := preserveSetupEnrollment(out, result, "owner@example.test")
		if err == nil || out["secret"] != resetFixtureSecret || out["session_id"] != "new-session" {
			t.Fatal("unknown factor was accepted or recognized seed/session were lost")
		}
	}
}

func TestTOTPInvalidCodeResponseRejectsConflictingErrorLayers(t *testing.T) {
	for _, body := range []string{
		`{"code":"invalid_code","error":true}`,
		`{"error":{"code":"invalid_code"},"detail":{"error":true}}`,
		`{"error":{"code":"invalid_code"},"detail":"{\"error\":true}"}`,
		`{"error":{"code":"invalid_code"},"detail":{"error":["forbidden"]}}`,
		`{"error":{"code":"invalid_code"},"detail":{"error":500}}`,
		`{"error":{"code":"invalid_code"},"detail":false}`,
		`{"error":{"code":"forbidden"},"code":"invalid_code"}`,
		`{"error":{"code":"invalid_code"},"detail":{"error":{"code":"rate_limited"}}}`,
	} {
		if totpInvalidCodeResponse(totpOutcomeFixture(t, body)) {
			t.Fatal("a single invalid-code field overrode a conflicting error layer")
		}
	}
	if !totpInvalidCodeResponse(totpOutcomeFixture(t, `{"error":{"code":"invalid_code"},"detail":{"code":"invalid_totp"}}`)) {
		t.Fatal("consistent known invalid-code fields were rejected")
	}
}

func TestTOTPMutationResponseCompatibility(t *testing.T) {
	// Only the original disable message and null adjunct come from the HAR.
	// Other known variants are synthetic cases already present in this repository.
	for _, tc := range []struct {
		name, step, body string
		want             totpMutationOutcome
	}{
		{"har_disable", "disable", `{"message":"Factor disabled","passkey_credential_manager_sync":null}`, totpMutationSucceeded},
		{"disable_optional_field_omitted", "disable", `{"message":"Factor disabled"}`, totpMutationSucceeded},
		{"disable_known_message_variant", "disable", `{"message":"Factor successfully disabled"}`, totpMutationSucceeded},
		{"disable_machine_receipt", "disable", `{"success":true}`, totpMutationSucceeded},
		{"activate_machine_receipt", "activation", `{"success":true}`, totpMutationSucceeded},
		{"activate_known_field_variant", "activation", `{"activated":true}`, totpMutationSucceeded},
		{"irrelevant_fields", "activation", `{"success":true,"passkey_credential_manager_sync":null,"extra":{"future":true}}`, totpMutationSucceeded},
		{"null_error_is_not_refusal", "activation", `{"success":true,"error":null}`, totpMutationSucceeded},
		{"matched_factor_and_session", "activation", `{"success":true,"factor_id":"fixture-factor","session_id":"fixture-session"}`, totpMutationSucceeded},
		{"matched_factor_object", "activation", `{"success":true,"factor":{"id":"fixture-factor","factor_type":"totp","is_recovery":false}}`, totpMutationSucceeded},
		{"null_response", "activation", `null`, totpMutationUnknown},
		{"empty_response", "activation", `{}`, totpMutationUnknown},
		{"success_string", "activation", `{"success":"true"}`, totpMutationUnknown},
		{"success_null", "activation", `{"success":null}`, totpMutationUnknown},
		{"success_number", "activation", `{"success":1}`, totpMutationUnknown},
		{"activated_string", "activation", `{"activated":"true"}`, totpMutationUnknown},
		{"no_success_substring_matching", "disable", `{"message":"The operation may have succeeded"}`, totpMutationUnknown},
		{"no_disabled_substring_matching", "disable", `{"message":"Factor disabled status is unavailable"}`, totpMutationUnknown},
		{"no_recursive_success", "activation", `{"unrelated":{"success":true}}`, totpMutationUnknown},
		{"no_unsampled_success_wrapper", "activation", `{"data":{"success":true}}`, totpMutationUnknown},
		{"no_unsampled_disable_wrapper", "disable", `{"result":{"message":"Factor disabled"}}`, totpMutationUnknown},
		{"different_step_message", "activation", `{"message":"Factor disabled"}`, totpMutationUnknown},
		{"wrong_factor_id", "activation", `{"success":true,"factor_id":"another-factor"}`, totpMutationUnknown},
		{"null_factor_id", "activation", `{"success":true,"factor_id":null}`, totpMutationUnknown},
		{"numeric_factor_id", "activation", `{"success":true,"factor_id":123}`, totpMutationUnknown},
		{"wrong_session", "activation", `{"success":true,"session_id":"another-session"}`, totpMutationUnknown},
		{"null_session", "activation", `{"success":true,"session_id":null}`, totpMutationUnknown},
		{"wrong_factor_object", "activation", `{"success":true,"factor":{"id":"another-factor","factor_type":"totp","is_recovery":false}}`, totpMutationUnknown},
		{"wrong_factor_type", "activation", `{"success":true,"factor":{"id":"fixture-factor","factor_type":"sms","is_recovery":false}}`, totpMutationUnknown},
		{"recovery_factor", "activation", `{"success":true,"factor":{"id":"fixture-factor","factor_type":"totp","is_recovery":true}}`, totpMutationUnknown},
		{"contradictory_disable_bool", "disable", `{"message":"Factor disabled","success":false}`, totpMutationUnknown},
		{"contradictory_disable_error", "disable", `{"message":"Factor disabled","error":"forbidden"}`, totpMutationUnknown},
		{"contradictory_activation_flags", "activation", `{"success":true,"activated":false}`, totpMutationUnknown},
		{"contradictory_activation_error", "activation", `{"success":true,"error":{"code":"invalid_code"}}`, totpMutationUnknown},
		{"contradictory_activation_detail", "activation", `{"activated":true,"detail":{"error":{"code":"invalid_code"}}}`, totpMutationUnknown},
		{"unrecognized_error_bool", "activation", `{"success":true,"error":true}`, totpMutationUnknown},
		{"unrecognized_detail_error_bool", "activation", `{"success":true,"detail":{"error":true}}`, totpMutationUnknown},
		{"unrecognized_encoded_detail_error_bool", "activation", `{"success":true,"detail":"{\"error\":true}"}`, totpMutationUnknown},
		{"unrecognized_detail_error_list", "activation", `{"success":true,"detail":{"error":["denied"]}}`, totpMutationUnknown},
		{"unrecognized_detail_error_number", "activation", `{"success":true,"detail":{"error":500}}`, totpMutationUnknown},
		{"unrecognized_detail_outer_bool", "activation", `{"success":true,"detail":true}`, totpMutationUnknown},
		{"unrecognized_detail_outer_list", "activation", `{"success":true,"detail":["denied"]}`, totpMutationUnknown},
		{"explicit_false", "activation", `{"success":false}`, totpMutationRejected},
		{"explicit_activated_false", "activation", `{"activated":false}`, totpMutationRejected},
		{"explicit_error_string", "disable", `{"error":"forbidden"}`, totpMutationRejected},
		{"explicit_error_object", "activation", `{"error":{"code":"invalid_code"}}`, totpMutationRejected},
		{"known_detail_object", "disable", `{"detail":{"error":{"code":"recent_auth_required"}}}`, totpMutationRejected},
		{"known_detail_json_string", "disable", `{"detail":"{\"error\":{\"code\":\"recent_auth_required\"}}"}`, totpMutationRejected},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyTOTPMutation(tc.step, totpOutcomeFixture(t, tc.body), "fixture-factor", "fixture-session")
			if got != tc.want {
				t.Fatalf("outcome=%v want=%v", got, tc.want)
			}
		})
	}
}

func TestTOTPResponseExplicitRejection(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       bool
	}{
		{"success_false", `{"success":false}`, true},
		{"error_string", `{"error":"forbidden"}`, true},
		{"error_object", `{"error":{"code":"recent_auth_required"}}`, true},
		{"detail_object", `{"detail":{"error":{"code":"recent_auth_required"}}}`, true},
		{"detail_json_string", `{"detail":"{\"error\":{\"code\":\"recent_auth_required\"}}"}`, true},
		{"empty", `{}`, false},
		{"null_error", `{"error":null}`, false},
		{"empty_error", `{"error":""}`, false},
		{"unknown_success_type", `{"success":"false"}`, false},
		{"nested_unrelated_error", `{"unrelated":{"error":"forbidden"}}`, false},
		{"unstructured_message", `{"message":"Request failed"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := totpResponseRejected(totpOutcomeFixture(t, tc.body)); got != tc.want {
				t.Fatalf("rejected=%t want=%t", got, tc.want)
			}
		})
	}
}

func TestTOTPInvalidCodeResponseRequiresExactMachineCode(t *testing.T) {
	for _, code := range []string{"invalid_code", "invalid_totp", "invalid_otp", "incorrect_code", "wrong_code"} {
		for _, wrap := range []struct {
			name string
			make func(string) map[string]any
		}{
			{"error_string", func(code string) map[string]any { return map[string]any{"error": code} }},
			{"error_object", func(code string) map[string]any { return map[string]any{"error": map[string]any{"code": code}} }},
			{"detail_object", func(code string) map[string]any {
				return map[string]any{"detail": map[string]any{"error": map[string]any{"code": code}}}
			}},
			{"detail_json_string", func(code string) map[string]any { return map[string]any{"detail": `{"error":{"code":"` + code + `"}}`} }},
		} {
			t.Run(code+"/"+wrap.name, func(t *testing.T) {
				if !totpInvalidCodeResponse(wrap.make(code)) {
					t.Fatal("explicit invalid-code response did not authorize code retry")
				}
			})
		}
	}
	for _, tc := range []struct{ name, body string }{
		{"empty", `{}`},
		{"success_missing", `{"activated":true}`},
		{"generic_rejection", `{"success":false}`},
		{"invalid_success_type", `{"success":"false"}`},
		{"error_null", `{"error":null}`},
		{"network_error", `{"error":"timeout"}`},
		{"recent_auth", `{"error":{"code":"recent_auth_required"}}`},
		{"substring_code", `{"error":{"code":"not_invalid_code"}}`},
		{"code_in_message", `{"error":{"message":"invalid_code"}}`},
		{"code_in_human_text", `{"message":"incorrect_code"}`},
		{"nested_unrelated_code", `{"unrelated":{"error":{"code":"invalid_code"}}}`},
		{"arbitrarily_nested_detail", `{"detail":{"detail":{"error":{"code":"invalid_code"}}}}`},
		{"truncated_detail_json", `{"detail":"{\"error\":{\"code\":\"invalid_code\"}"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if totpInvalidCodeResponse(totpOutcomeFixture(t, tc.body)) {
				t.Fatal("unknown or unrelated outcome incorrectly authorized code retry")
			}
		})
	}
}
