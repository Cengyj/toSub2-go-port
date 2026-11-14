package protocol

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
)

// The private proof binds the actual confirmation source to the exact saved
// enrollment. A business receipt and a matching live read are separate facts.
func resetActivationEvidence(out map[string]any) map[string]any {
	secret, session, factor, _ := resetEnrollmentMaterial(out)
	material, _ := json.Marshal([]string{secret, session, factor})
	digest := sha256.Sum256(material)
	evidence := map[string]any{
		"operation_id": str(out["operation_id"]), "account_id": str(out["account_id"]),
		"email":     strings.ToLower(strings.TrimSpace(str(out["email"]))),
		"factor_id": factor, "enrollment_digest": hex.EncodeToString(digest[:]),
		"confirmed_at": nowRFC3339(),
	}
	if source := str(out["activation_confirmation_source"]); source != "" {
		evidence["confirmation_source"] = source
		evidence["attempt_id"] = str(out["activation_attempt_id"])
	}
	return evidence
}

// acknowledgedResetActivation interprets only the complete response captured
// for this operation's activation attempt. It never infers success from HTTP
// status alone or supplies missing enrollment material.
func acknowledgedResetActivation(out map[string]any) bool {
	_, session, factor, valid := resetEnrollmentMaterial(out)
	if !valid || out["activation_attempted"] != true || resetRequestUnsent(out, "activation") ||
		strings.TrimSpace(str(out["operation_id"])) == "" || strings.TrimSpace(str(out["account_id"])) == "" ||
		strings.TrimSpace(str(out["email"])) == "" || strings.TrimSpace(str(out["activation_attempt_id"])) == "" {
		return false
	}
	receipt := payload(out["activation_receipt"])
	status := receiptStatus(receipt["http_status"])
	if receiptStatus(receipt["version"]) != 1 || receipt["step"] != "activation" ||
		receipt["operation_id"] != out["operation_id"] || receipt["attempt_id"] != out["activation_attempt_id"] ||
		receipt["body_complete"] != true || receipt["termination"] != "complete" || status < http.StatusOK || status >= 300 {
		return false
	}
	body, err := base64.StdEncoding.DecodeString(str(receipt["body_base64"]))
	if err != nil || len(body) > maxResetReceiptBody {
		return false
	}
	result, valid := decodeTOTPResponse(body)
	if !valid {
		return false
	}
	return classifyTOTPMutation("activation", result, factor, session) == totpMutationSucceeded
}

func verifiedResetActivation(out map[string]any) bool {
	_, _, _, valid := resetEnrollmentMaterial(out)
	if !valid || out["activation_attempted"] != true || out["activation_succeeded"] != true || out["activation_verified"] != true || resetRequestUnsent(out, "activation") {
		return false
	}
	version, exists := out["activation_evidence_schema"]
	if !exists {
		// Older records explicitly persisted confirmed_by_read after matching the
		// live factor. Keep that compatibility, never synthesize the missing flag.
		return out["activation_confirmation_source"] == nil && out["activation_confirmed_by_read"] == true
	}
	switch receiptStatus(version) {
	case 1:
		if out["activation_confirmation_source"] != nil || out["activation_confirmed_by_read"] != true {
			return false
		}
	case 2:
		switch str(out["activation_confirmation_source"]) {
		case "business_receipt":
			if out["activation_acknowledged"] != true || out["activation_confirmed_by_read"] == true || !acknowledgedResetActivation(out) {
				return false
			}
		case "state_read":
			if out["activation_confirmed_by_read"] != true {
				return false
			}
		default:
			return false
		}
	default:
		return false
	}
	evidence, expected := payload(out["activation_evidence"]), resetActivationEvidence(out)
	for _, key := range []string{"operation_id", "account_id", "email", "factor_id", "enrollment_digest"} {
		if str(evidence[key]) == "" || evidence[key] != expected[key] {
			return false
		}
	}
	if receiptStatus(version) == 2 {
		if evidence["confirmation_source"] != expected["confirmation_source"] || evidence["attempt_id"] != expected["attempt_id"] {
			return false
		}
	}
	return str(evidence["confirmed_at"]) != ""
}
