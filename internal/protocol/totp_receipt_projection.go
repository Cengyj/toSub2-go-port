package protocol

import (
	"encoding/json"
	"strings"
)

// Retain only the endpoint fields interpreted as business evidence. Arbitrary
// bodies, unknown extensions and echoed authentication state never reach disk.
// An invalid response remains uninterpretable; dropping a duplicate/error must
// never turn it into an apparently successful receipt.
func totpReceiptBody(body []byte) []byte {
	return projectTOTPReceipt("enroll", body)
}

func projectTOTPReceipt(step string, body []byte) []byte {
	return projectBoundTOTPReceipt(step, body, "")
}

func projectBoundTOTPReceipt(step string, body []byte, account string) []byte {
	response, valid := decodeTOTPResponse(body)
	if !valid || account != "" && !stateAccountMatches(response, account) {
		return nil
	}
	// Normalize only a complete recognized refusal shape. Preserving a known
	// conflict code by itself would be unsafe: projecting away an unknown
	// wrapper or enrollment material could turn contradictory evidence into
	// permission to remove a factor after a failed initial enrollment.
	if isResetEnrollmentStep(step) && totpEnrollmentAlreadyExists(response) {
		encoded, _ := json.Marshal(map[string]any{"error": map[string]any{"code": totpEnrollmentExistsCode}})
		return encoded
	}
	projected := map[string]any{}
	// Preserve presence/type when that alone makes a receipt unusable. Never
	// persist the extension's contents or an echoed seed on a non-enroll step.
	for _, key := range []string{"otpauth_uri", "new_totp_factor_id", "enrollment_factor"} {
		if _, ok := response[key]; ok {
			projected[key] = nil
		}
	}
	if v, ok := response["request_id"]; ok {
		if _, valid := v.(string); !valid {
			projected["request_id"] = nil
		}
	}
	for _, key := range []string{"success", "activated"} {
		if value, exists := response[key]; exists {
			if boolean, ok := value.(bool); ok {
				projected[key] = boolean
			} else {
				projected[key] = nil // An unsupported type remains invalid.
			}
		}
	}
	if totpDisabledMessage(response["message"]) {
		projected["message"] = response["message"]
	}
	for _, key := range []string{"factor_id", "session_id", "secret"} {
		if value, exists := response[key]; exists {
			text, ok := value.(string)
			if key == "secret" && !isResetEnrollmentStep(step) {
				projected[key] = nil
				continue
			}
			if ok {
				projected[key] = text
			} else {
				projected[key] = nil
			}
		}
	}
	if value, exists := response["factor"]; exists {
		factor := payload(value)
		if factor == nil {
			projected["factor"] = nil
		} else {
			copy := map[string]any{}
			for _, key := range []string{"id", "factor_type"} {
				if value, exists := factor[key]; exists {
					text, _ := value.(string)
					copy[key] = text
				}
			}
			if value, exists := factor["is_recovery"]; exists {
				if boolean, ok := value.(bool); ok {
					copy["is_recovery"] = boolean
				} else {
					copy["is_recovery"] = nil
				}
			}
			projected["factor"] = copy
		}
	}
	for _, key := range []string{"error", "code"} {
		if value, exists := response[key]; exists {
			projected[key] = totpReceiptError(value)
		}
	}
	if value, exists := response["detail"]; exists {
		if text, ok := value.(string); ok {
			var detail map[string]any
			if json.Unmarshal([]byte(text), &detail) == nil {
				value = detail
			} else {
				value = totpReceiptError(text)
			}
		}
		if detail, ok := value.(map[string]any); ok {
			copy := map[string]any{}
			for _, key := range []string{"error", "code"} {
				if v, exists := detail[key]; exists {
					copy[key] = totpReceiptError(v)
				}
			}
			projected["detail"] = copy
		} else {
			projected["detail"] = totpReceiptError(value)
		}
	}
	encoded, _ := json.Marshal(projected)
	return encoded
}

func totpReceiptError(value any) any {
	switch v := value.(type) {
	case nil:
		return nil
	case string:
		if strings.TrimSpace(v) == "" {
			return v
		}
		switch v {
		case "invalid_code", "invalid_totp", "invalid_otp", "incorrect_code", "wrong_code", "recent_auth_required", "token_revoked":
			return v
		}
		return "unrecognized_remote_error"
	case map[string]any:
		if len(v) == 0 {
			return map[string]any{}
		}
		code, _ := v["code"].(string)
		if strings.TrimSpace(code) == "" {
			code = "unrecognized_remote_error"
		}
		return map[string]any{"code": totpReceiptError(code)}
	default:
		return []any{} // Preserve the unsupported type classification, not data.
	}
}
