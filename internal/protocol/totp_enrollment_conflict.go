package protocol

import "strings"

const totpEnrollmentExistsCode = "totp_already_enrolled"

// totpEnrollmentAlreadyExists recognizes an explicit enrollment refusal, not
// an HTTP status or a substring of an arbitrary server message. These bounded
// compatibility forms are covered by synthetic fixtures; an unknown upstream
// wording remains unknown. The caller must separately require a complete
// response for the current enrollment attempt and an allowed HTTP status.
func totpEnrollmentAlreadyExists(result map[string]any) bool {
	matched, valid := totpEnrollmentConflictEnvelope(result, true)
	return valid && matched
}

func totpEnrollmentConflictEnvelope(result map[string]any, allowDetail bool) (bool, bool) {
	if result == nil {
		return false, false
	}
	matched := false
	for key, value := range result {
		var found, valid bool
		switch key {
		case "request_id":
			// Opaque correlation metadata, never a signal or a public ID.
			if _, ok := value.(string); !ok {
				return false, false
			}
			continue
		case "success", "activated":
			// A simultaneous success, even with an explicit conflict code,
			// cannot prove that enrollment made no change.
			flag, ok := value.(bool)
			if !ok || flag {
				return false, false
			}
			continue
		case "error":
			found, valid = totpEnrollmentConflictError(value)
		case "code", "message":
			found, valid = totpEnrollmentConflictField(key, value)
		case "detail":
			if !allowDetail {
				return false, false
			}
			switch detail := value.(type) {
			case nil:
				valid = true
			case string:
				if len(detail) > maxResetReceiptBody {
					return false, false
				}
				if strings.HasPrefix(strings.TrimSpace(detail), "{") {
					decoded, ok := decodeTOTPResponse([]byte(detail))
					if !ok {
						return false, false
					}
					found, valid = totpEnrollmentConflictEnvelope(decoded, false)
				} else {
					found, valid = totpEnrollmentConflictSignal(detail)
				}
			case map[string]any:
				found, valid = totpEnrollmentConflictEnvelope(detail, false)
			default:
				return false, false
			}
		default:
			// This also rejects any enrollment material, including empty or
			// malformed secret, session or factor fields. Unknown wrappers
			// must not hide material or a contradictory outcome.
			return false, false
		}
		if !valid {
			return false, false
		}
		matched = matched || found
	}
	return matched, true
}

func totpEnrollmentConflictError(value any) (bool, bool) {
	if object, ok := value.(map[string]any); ok {
		matched := false
		for key, value := range object {
			if key != "code" && key != "message" {
				return false, false
			}
			found, valid := totpEnrollmentConflictField(key, value)
			if !valid {
				return false, false
			}
			matched = matched || found
		}
		return matched, true
	}
	return totpEnrollmentConflictSignal(value)
}

// Prose is a fallback for a missing code, never a substitute for a malformed
// or contradictory code. This diagnostic classifier grants no authority itself.
func totpEnrollmentConflictField(key string, v any) (bool, bool) {
	if key == "code" {
		if s, ok := v.(string); ok && strings.ContainsAny(strings.TrimSpace(s), " \r\n\t") {
			return false, false
		}
	}
	return totpEnrollmentConflictSignal(v)
}

func totpEnrollmentConflictSignal(value any) (bool, bool) {
	if value == nil {
		return false, true
	}
	text, ok := value.(string)
	if !ok {
		return false, false
	}
	text = strings.ToLower(strings.TrimSpace(text))
	if text == "" {
		return false, true
	}
	switch text {
	case totpEnrollmentExistsCode, "totp_already_enabled", "totp_already_exists",
		"mfa_already_enrolled", "mfa_already_enabled", "mfa_already_exists",
		"factor_already_enrolled", "factor_already_exists":
		return true, true
	}
	switch strings.TrimSuffix(text, ".") {
	case "totp is already enabled", "totp is already enrolled", "totp factor already exists",
		"a totp factor already exists", "totp factor is already enrolled",
		"mfa is already enabled", "mfa is already enrolled",
		"two-factor authentication is already enabled", "2fa is already enabled",
		"multi-factor authentication is already enabled":
		return true, true
	}
	return false, false
}
