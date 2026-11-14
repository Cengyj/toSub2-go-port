package protocol

import (
	"encoding/json"
	"errors"
	"strings"
	"tosub2-go-port/internal/privatestate"
)

// ReadTOTPResetCompletion validates operation evidence only. It never restores
// a session; the caller must match operation and seed to the protected vault.
func ReadTOTPResetCompletion(path, email string) (map[string]any, error) {
	out, err := ReadTOTPResetIdentity(path, email)
	if err != nil {
		return nil, err
	}
	if !verifiedResetActivation(out) {
		return nil, errors.New("TOTP_RESET_COMPLETION_UNVERIFIED: a complete verified activation record is required")
	}
	return out, nil
}

// MarkTOTPResetCredentialPersisted follows durable vault readback, not a GET.
func MarkTOTPResetCredentialPersisted(path, email, operationID string) error {
	out, err := ReadTOTPResetCompletion(path, email)
	if err != nil {
		return err
	}
	if operationID == "" || str(out["operation_id"]) != operationID {
		return errors.New("TOTP_RESET_OPERATION_CONFLICT: 本地提交与操作证据不一致")
	}
	out["credential_persisted"], out["reset_totp_phase"] = true, "credential_persisted"
	skipUnattemptedLocalCompletionLogout(out)
	return writeTOTPJournal(path, out)
}

func ReadTOTPResetJournalJSON(path string) ([]byte, error) {
	ctx, cancel := privateStateSaveContext()
	defer cancel()
	data, binding, err := privatestate.ReadJSONWithBinding(ctx, path, "totp-reset", maxTOTPJournalBytes)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if json.Unmarshal(data, &out) != nil || out == nil {
		return nil, errors.New("TOTP_RESET_JOURNAL_INVALID: 操作记录无法解析")
	}
	if binding.Schema != 0 && (binding.Identity != privatestate.Identity(str(out["email"])) || binding.Operation != str(out["operation_id"])) {
		return nil, errors.New("TOTP_RESET_JOURNAL_INVALID: 操作记录绑定不一致")
	}
	filtered, err := decodeTOTPResetJournal(data)
	if err != nil {
		return nil, err
	}
	return json.Marshal(filtered)
}
func ReadTOTPResetJournal(path string) (map[string]any, error) {
	return readPrivateTOTPResetJournal(path)
}
func readPrivateTOTPResetJournal(path string) (map[string]any, error) {
	data, err := ReadTOTPResetJournalJSON(path)
	if err != nil {
		return nil, err
	}
	return decodeTOTPResetJournal(data)
}
func decodeTOTPResetJournal(data []byte) (map[string]any, error) {
	var out map[string]any
	if json.Unmarshal(data, &out) != nil || out == nil {
		return nil, errors.New("TOTP_RESET_JOURNAL_INVALID: 操作记录无法解析")
	}
	// Historical authentication payloads are discarded at the read boundary.
	// Business results and their proof remain available for local completion.
	for _, key := range []string{"previous_session_ref", "session_ref", "preparation_checkpoint", "recovery_checkpoint", "reauthentication_previous_checkpoint", "cookies", "cookie_ref", "cookie_state", "initial_login_progress"} {
		delete(out, key)
	}

	return out, nil
}

// Identity is validated without a sendable Cookie, checkpoint or transport.
// Legacy completed records are readable only for local completion; no API can
// use them to resume network work. Unknown legacy operations remain blocked.
func ReadTOTPResetIdentity(path, email string) (map[string]any, error) {
	out, err := readPrivateTOTPResetJournal(path)
	if err != nil {
		return nil, err
	}
	if err = validateResetMutationEvidence(out); err != nil {
		return nil, err
	}
	invalid := errors.New("TOTP_RESET_JOURNAL_INVALID: 操作身份或发送证据不完整")
	if receiptStatus(out["version"]) != 1 || out["reset_totp_requested"] != true || strings.TrimSpace(email) == "" || !strings.EqualFold(strings.TrimSpace(str(out["email"])), strings.TrimSpace(email)) || strings.TrimSpace(str(out["operation_id"])) == "" || strings.TrimSpace(str(out["account_id"])) == "" || strings.TrimSpace(str(out["old_totp_factor_id"])) == "" {
		return nil, invalid
	}
	if out["operation_scope"] != "current_session" && receiptStatus(out["recovery_schema"]) != 1 {
		return nil, invalid
	}
	if _, ok := out["enroll_attempted"].(bool); !ok {
		return nil, invalid
	}
	if _, ok := out["activation_attempted"].(bool); !ok {
		return nil, invalid
	}
	if out["activation_attempted"] == true && out["enroll_attempted"] != true {
		return nil, invalid
	}
	if out["enroll_attempted"] == false {
		for _, key := range []string{"secret", "session_id", "new_totp_factor_id", "enrollment_factor"} {
			if _, exists := out[key]; exists {
				return nil, invalid
			}
		}
	}
	if out["operation_scope"] != "current_session" && !verifiedResetActivation(out) {
		return nil, errors.New("TOTP_RESET_LEGACY_REVIEW_REQUIRED: 旧操作结果未确认，需要人工处理")
	}
	return out, nil
}

// TOTPResetJournalIsReadOnly identifies historical session-bearing records.
// Local completion reads their activation evidence and commits the vault and
// metadata without importing sessions or rewriting the original history.
func TOTPResetJournalIsReadOnly(out map[string]any) bool {
	for _, key := range []string{"preparation_checkpoint", "recovery_checkpoint", "reauthentication_previous_checkpoint", "previous_session_ref", "session_ref", "recovery_schema", "recovery_revision", "recovery_resumed", "initial_login_progress", "retired_material_digests", "disable_reauthentication", "enrollment_reauthentication", "reauthentication_session_rejection", "reauthentication_renewals"} {
		if _, exists := out[key]; exists {
			return true
		}
	}
	return false
}
func (f *Flow) saveTOTPResetJournal(path string, out map[string]any) error {
	return writeTOTPJournal(path, out)
}
