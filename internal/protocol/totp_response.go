package protocol

import (
	"encoding/json"
	"strings"
)

type totpMutationOutcome uint8

const (
	totpMutationUnknown totpMutationOutcome = iota
	totpMutationSucceeded
	totpMutationRejected
)

// A duplicate business field is contradictory evidence, not a last-value-wins
// success. Validate only the explicit endpoint objects we interpret.
func decodeTOTPResponse(body []byte) (map[string]any, bool) {
	raw, valid := recentAuthObject(body)
	if !valid {
		return nil, false
	}
	if _, valid := totpKnownResponseObject(raw["factor"]); !valid {
		return nil, false
	}
	if _, valid := totpKnownResponseObject(raw["account"]); !valid {
		return nil, false
	}
	factors, valid := totpKnownResponseObject(raw["factors"])
	if !valid {
		return nil, false
	}
	if list := strings.TrimSpace(string(factors["totp"])); strings.HasPrefix(list, "[") {
		var entries []json.RawMessage
		if json.Unmarshal(factors["totp"], &entries) != nil {
			return nil, false
		}
		for _, factor := range entries {
			if _, valid := totpKnownResponseObject(factor); !valid {
				return nil, false
			}
		}
	}
	if _, valid := totpKnownResponseObject(raw["error"]); !valid {
		return nil, false
	}
	detailBody := raw["detail"]
	var detailText string
	if json.Unmarshal(detailBody, &detailText) == nil {
		detailBody = []byte(detailText)
	}
	detail, valid := totpKnownResponseObject(detailBody)
	if !valid {
		return nil, false
	}
	if _, valid := totpKnownResponseObject(detail["error"]); !valid {
		return nil, false
	}
	var result map[string]any
	if json.Unmarshal(body, &result) != nil || result == nil {
		return nil, false
	}
	return result, true
}

// Inspect only object layers that the endpoint contract actually uses. An
// unknown scalar/array is left intact so validation can reject its semantics
// without discarding separately recognized enrollment material. This does not
// recursively search unrelated objects for fields or infer another layout.
func totpKnownResponseObject(body json.RawMessage) (map[string]json.RawMessage, bool) {
	if !strings.HasPrefix(strings.TrimSpace(string(body)), "{") {
		return nil, true
	}
	return recentAuthObject(body)
}

// These are endpoint-local contracts from the HAR (disable message) and the
// existing redacted protocol fixtures (success/activated and the second disable
// wording). Unknown wrappers and string booleans deliberately remain unknown.
func classifyTOTPMutation(step string, result map[string]any, factorID, sessionID string) totpMutationOutcome {
	if result == nil || !totpOutcomeFieldsValid(result) || !totpResponseBindingMatches(result, factorID, sessionID) {
		return totpMutationUnknown
	}
	for _, key := range []string{"secret", "otpauth_uri", "new_totp_factor_id", "enrollment_factor"} {
		if _, present := result[key]; present {
			return totpMutationUnknown
		}
	}
	success := result["success"] == true
	switch step {
	case "disable":
		success = success || totpDisabledMessage(result["message"])
	case "activation":
		success = success || result["activated"] == true
	default:
		return totpMutationUnknown
	}
	rejected := totpResponseRejected(result)
	if success && (rejected || totpCrossedMutationSignals(step, result)) {
		return totpMutationUnknown
	}
	if rejected {
		return totpMutationRejected
	}
	if success {
		return totpMutationSucceeded
	}
	return totpMutationUnknown
}

// A generic success acknowledges the requested operation, not its opposite.
func totpCrossedMutationSignals(step string, result map[string]any) bool {
	switch step {
	case "activation":
		return totpDisabledMessage(result["message"]) && (result["success"] == true || result["activated"] == true)
	case "disable":
		return result["activated"] == true && (result["success"] == true || totpDisabledMessage(result["message"]))
	}
	return false
}

func totpDisabledMessage(value any) bool {
	message, ok := value.(string)
	if !ok {
		return false
	}
	switch strings.TrimSuffix(strings.ToLower(strings.TrimSpace(message)), ".") {
	case "factor disabled", "factor successfully disabled":
		return true
	}
	return false
}

func totpOutcomeFieldsValid(result map[string]any) bool {
	if value, present := result["request_id"]; present {
		if _, ok := value.(string); !ok {
			return false
		}
	}
	for _, key := range []string{"success", "activated"} {
		if value, present := result[key]; present {
			if _, ok := value.(bool); !ok {
				return false
			}
		}
	}
	// Every interpreted error layer must retain a supported type. In
	// particular, a newly shaped detail.error cannot silently disappear and
	// leave an otherwise contradictory success flag authoritative.
	for _, value := range append([]any{result["detail"]}, totpErrorFields(result)...) {
		switch value.(type) {
		case nil, string, map[string]any:
		default:
			return false
		}
	}
	return true
}

// Optional echoed bindings must agree with the request; never harvest IDs or
// success flags from arbitrary nested objects. Null unrelated fields are ignored.
func totpResponseBindingMatches(result map[string]any, factorID, sessionID string) bool {
	for key, expected := range map[string]string{"factor_id": factorID, "session_id": sessionID} {
		if value, present := result[key]; present {
			actual, ok := value.(string)
			if !ok || expected == "" || actual != expected {
				return false
			}
		}
	}
	if value, present := result["factor"]; present {
		factor := payload(value)
		if factor == nil || factorID == "" || factor["id"] != factorID {
			return false
		}
		if v, present := factor["factor_type"]; present && v != "totp" {
			return false
		}
		if v, present := factor["is_recovery"]; present && v != false {
			return false
		}
	}
	return true
}

// The error paths are bounded to the shapes already used by account-security
// responses: error, and detail containing an object or JSON-encoded object.
// This does not recursively interpret arbitrary JSON as a business receipt.
func totpErrorFields(result map[string]any) []any {
	fields := []any{result["error"], result["code"]}
	detail := payload(result["detail"])
	if s, ok := result["detail"].(string); ok {
		if json.Unmarshal([]byte(s), &detail) != nil {
			fields = append(fields, s)
		}
	}
	if detail != nil {
		fields = append(fields, detail["error"], detail["code"])
	}
	return fields
}

func totpResponseRejected(result map[string]any) bool {
	if result["success"] == false || result["activated"] == false {
		return true
	}
	for _, value := range totpErrorFields(result) {
		switch v := value.(type) {
		case string:
			if strings.TrimSpace(v) != "" {
				return true
			}
		case map[string]any:
			if len(v) != 0 {
				return true
			}
		}
	}
	return false
}

func totpInvalidCodeResponse(result map[string]any) bool {
	if !totpOutcomeFieldsValid(result) || result["success"] == true || result["activated"] == true || totpDisabledMessage(result["message"]) {
		return false
	}
	matched := false
	for _, value := range totpErrorFields(result) {
		if value == nil || value == "" {
			continue
		}
		code, _ := value.(string)
		if object := payload(value); object != nil {
			if len(object) == 0 {
				continue
			}
			code, _ = object["code"].(string)
		}
		switch code {
		case "invalid_code", "invalid_totp", "invalid_otp", "incorrect_code", "wrong_code":
			matched = true
		default:
			return false
		}
	}
	return matched
}
