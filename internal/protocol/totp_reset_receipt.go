package protocol

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"

	"tosub2-go-port/internal/transport"
)

const maxResetReceiptBody = 64 << 10

func resetStepForEndpoint(endpoint string) string {
	switch endpoint {
	case "/backend-api/accounts/mfa/user/disable_in_house":
		return "disable"
	case "/backend-api/accounts/mfa/enroll":
		return "enroll"
	case "/backend-api/accounts/mfa/user/activate_enrollment":
		return "activation"
	}
	return ""
}

func (f *Flow) resetReceipt(step string, r *transport.Response, complete bool, requestErr error) error {
	capture := f.resetCapture
	if capture == nil || step == "" || r == nil {
		return nil
	}
	body := r.Body
	if len(body) > maxResetReceiptBody {
		body, complete = body[:maxResetReceiptBody], false
	}
	end := "complete"
	if !complete {
		switch {
		case errors.Is(requestErr, transport.ErrResponseTooLarge):
			end = "too_large"
		case errors.Is(requestErr, context.Canceled):
			end = "canceled"
		case requestErr == nil:
			end = "headers_received"
		default:
			end = "interrupted"
		}
	}
	capture.out[step+"_receipt"] = map[string]any{
		"version": 1, "step": step, "operation_id": capture.out["operation_id"],
		"attempt_id": capture.out[step+"_attempt_id"], "http_status": r.StatusCode,
		"body_complete": complete, "body_base64": base64.StdEncoding.EncodeToString(projectBoundTOTPReceipt(step, body, str(capture.out["account_id"]))),
		"body_projection": "totp-business-v1",
		"termination":     end, "received_at": nowRFC3339(),
	}
	return f.saveTOTPResetJournal(capture.path, capture.out)
}

func preserveEnrollmentMaterial(out, enrollment map[string]any, email string, complete bool) {
	secret, sid := normalizeTOTPSecret(str(enrollment["secret"])), str(enrollment["session_id"])
	factor := payload(enrollment["factor"])
	newID := str(factor["id"])
	uri, uriErr := BuildTOTPURI(email, secret)
	if secret != "" {
		out["secret"] = secret
	}
	if sid != "" {
		out["session_id"] = sid
	}
	if newID != "" {
		out["new_totp_factor_id"] = newID
	}
	if factor != nil {
		out["enrollment_factor"] = map[string]any{"id": newID, "factor_type": factor["factor_type"], "is_recovery": factor["is_recovery"]}
	}
	if uriErr == nil {
		out["otpauth_uri"] = uri
	}
	out["enrollment_validated"] = complete && !totpResponseRejected(enrollment) && totpOutcomeFieldsValid(enrollment) && totpResponseBindingMatches(enrollment, newID, sid) && secret != "" && strings.TrimSpace(sid) != "" && strings.TrimSpace(newID) != "" && newID != str(out["old_totp_factor_id"]) && factor["factor_type"] == "totp" && factor["is_recovery"] == false && uriErr == nil
}

// Only an intact receipt belonging to this exact dispatched attempt can be
// reinterpreted after a local save failure. No network request is needed.
func resetReceiptBody(out map[string]any, step string) (map[string]any, bool) {
	status := receiptStatus(payload(out[step+"_receipt"])["http_status"])
	if status < http.StatusOK || status >= 300 {
		return nil, false
	}
	return resetCompleteReceiptBody(out, step)
}

func resetCompleteReceiptBody(out map[string]any, step string) (map[string]any, bool) {
	r := payload(out[step+"_receipt"])
	if strings.TrimSpace(str(out["operation_id"])) == "" || strings.TrimSpace(str(out[step+"_attempt_id"])) == "" || resetRequestUnsent(out, step) ||
		receiptStatus(r["version"]) != 1 || r["step"] != step || r["operation_id"] != out["operation_id"] || r["attempt_id"] != out[step+"_attempt_id"] ||
		r["body_complete"] != true || r["termination"] != "complete" {
		return nil, false
	}
	b, err := base64.StdEncoding.DecodeString(str(r["body_base64"]))
	if err != nil || len(b) > maxResetReceiptBody {
		return nil, false
	}
	return decodeTOTPResponse(b)
}

func acknowledgedResetRemoval(out map[string]any) bool {
	result, complete := resetReceiptBody(out, "disable")
	return complete && str(out["old_totp_factor_id"]) != "" && str(out["reset_totp_attempted_at"]) != "" &&
		classifyTOTPMutation("disable", result, str(out["old_totp_factor_id"]), "") == totpMutationSucceeded
}

// Interpret a previously received complete enrollment, never repeat its POST.
// Partial responses and receipts from another attempt cannot authorize activation.
func restoreEnrollmentReceipt(out map[string]any, email string) bool {
	if _, _, _, valid := resetEnrollmentMaterial(out); valid {
		return false
	}
	step := resetEnrollmentSource(out)
	enrollment, valid := resetReceiptBody(out, step)
	if !valid || !resetHasEnrollmentAttempt(out) {
		return false
	}
	// Existing material is evidence too. Never replace a recognized nonempty
	// seed/session/factor with a different value during offline interpretation.
	probe := map[string]any{"old_totp_factor_id": out["old_totp_factor_id"]}
	preserveEnrollmentMaterial(probe, enrollment, email, true)
	if probe["enrollment_validated"] != true {
		return false
	}
	for _, key := range []string{"secret", "session_id", "new_totp_factor_id"} {
		if old := str(out[key]); old != "" && old != str(probe[key]) {
			return false
		}
	}
	for key, value := range probe {
		out[key] = value
	}
	return true
}
