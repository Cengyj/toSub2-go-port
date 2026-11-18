package protocol

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"
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
	if receiptStatus(out["activation_evidence_schema"]) == 3 {
		evidence["state_checked_at"] = str(out["activation_state_checked_at"])
	}
	return evidence
}

// acknowledgedResetActivation interprets only the complete response captured
// for this operation's activation attempt. It never infers success from HTTP
// status alone or supplies missing enrollment material.
func acknowledgedResetActivation(out map[string]any) bool {
	_, session, factor, valid := resetEnrollmentMaterial(out)
	if !valid || out["activation_attempted"] != true || strings.TrimSpace(str(out["account_id"])) == "" || strings.TrimSpace(str(out["email"])) == "" {
		return false
	}
	result, complete := resetReceiptBody(out, "activation")
	if !complete || !stateAccountMatches(result, str(out["account_id"])) {
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
	case 2, 3:
		if receiptStatus(version) == 3 {
			// A schema-3 record always requires a dated matching read. Never
			// downgrade such a record to receipt-only proof during recovery.
			if out["activation_confirmation_source"] != "state_read" {
				return false
			}
			if _, err := time.Parse(time.RFC3339Nano, str(out["activation_state_checked_at"])); err != nil {
				return false
			}
		}
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
	if receiptStatus(version) >= 2 {
		if evidence["confirmation_source"] != expected["confirmation_source"] || evidence["attempt_id"] != expected["attempt_id"] {
			return false
		}
	}
	if receiptStatus(version) == 3 && evidence["state_checked_at"] != expected["state_checked_at"] {
		return false
	}
	return str(evidence["confirmed_at"]) != ""
}
