package protocol

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"

	"tosub2-go-port/internal/privatestate"
)

const maxTOTPJournalBytes = 1 << 20

// A restriction on the private read view, never a proof of identity, dispatch,
// or success. Do not manufacture a recovery_schema when discarding a session.
const legacyReadOnlyKey = "legacy_session_read_only"

var resetSessionFields = [...]string{
	"previous_session_ref", "session_ref", "preparation_checkpoint", "recovery_checkpoint",
	"reauthentication_previous_checkpoint", "cookies", "cookie_ref", "cookie_state", "initial_login_progress",
}

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
	out, err := ReadTOTPResetJournal(path)
	if err != nil {
		return nil, err
	}
	return json.Marshal(out)
}

func ReadTOTPResetJournal(path string) (map[string]any, error) {
	ctx, cancel := privateStateSaveContext()
	defer cancel()
	data, binding, err := privatestate.ReadJSONWithBinding(ctx, path, "totp-reset", maxTOTPJournalBytes)
	if err != nil {
		return nil, err
	}
	out, err := decodeTOTPResetJournal(data)
	if err != nil {
		return nil, err
	}
	if binding.Schema != 0 && (binding.Identity != privatestate.Identity(str(out["email"])) || binding.Operation != str(out["operation_id"])) {
		return nil, errors.New("TOTP_RESET_JOURNAL_INVALID: 操作记录绑定不一致")
	}
	return out, nil
}

func decodeTOTPResetJournal(data []byte) (map[string]any, error) {
	var out map[string]any
	if json.Unmarshal(data, &out) != nil || out == nil {
		return nil, errors.New("TOTP_RESET_JOURNAL_INVALID: 操作记录无法解析")
	}
	if TOTPResetJournalIsReadOnly(out) {
		out[legacyReadOnlyKey] = true
		for _, key := range resetSessionFields {
			delete(out, key)
		}
	}
	return out, nil
}

// Identity is validated without a sendable Cookie, checkpoint or transport.
// Legacy completed records are readable only for local completion; no API can
// use them to resume network work. Unknown legacy operations remain blocked.
func ReadTOTPResetIdentity(path, email string) (map[string]any, error) {
	out, err := ReadTOTPResetJournal(path)
	if err != nil {
		return nil, err
	}
	if err = validateResetMutationEvidence(out); err != nil {
		return nil, err
	}
	invalid := errors.New("TOTP_RESET_JOURNAL_INVALID: 操作身份或发送证据不完整")
	if receiptStatus(out["version"]) != 1 || out["reset_totp_requested"] != true || strings.TrimSpace(email) == "" || !strings.EqualFold(strings.TrimSpace(str(out["email"])), strings.TrimSpace(email)) || strings.TrimSpace(str(out["operation_id"])) == "" || strings.TrimSpace(str(out["account_id"])) == "" || strings.TrimSpace(str(out["old_totp_factor_id"])) == "" && !resetInitialEnrollmentStrategy(out) {
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
	if out["activation_attempted"] == true && !resetHasEnrollmentAttempt(out) {
		return nil, invalid
	}
	if !resetHasEnrollmentAttempt(out) {
		for _, key := range []string{"secret", "session_id", "new_totp_factor_id", "enrollment_factor"} {
			if _, exists := out[key]; exists {
				return nil, invalid
			}
		}
	}
	if (out["operation_scope"] != "current_session" || TOTPResetJournalIsReadOnly(out)) && !verifiedResetActivation(out) {
		return nil, errors.New("TOTP_RESET_LEGACY_REVIEW_REQUIRED: 旧操作结果未确认，需要人工处理")
	}
	return out, nil
}

// TOTPResetJournalIsReadOnly identifies historical session-bearing records.
// Local completion reads their activation evidence and commits the vault and
// metadata without importing sessions or rewriting the original history.
func TOTPResetJournalIsReadOnly(out map[string]any) bool {
	for _, key := range resetSessionFields {
		if _, exists := out[key]; exists {
			return true
		}
	}
	for _, key := range []string{legacyReadOnlyKey, "recovery_schema", "recovery_revision", "recovery_resumed", "retired_material_digests", "disable_reauthentication", "enrollment_reauthentication", "reauthentication_session_rejection", "reauthentication_renewals"} {
		if _, exists := out[key]; exists {
			return true
		}
	}
	return false
}

func writeTOTPJournal(path string, result map[string]any) error {
	if TOTPResetJournalIsReadOnly(result) {
		return errors.New("TOTP_RESET_LEGACY_READ_ONLY: 旧会话资料仅供读取，本地完成不得改写原始记录")
	}
	ctx, cancel := privateStateSaveContext()
	defer cancel()
	data, err := json.Marshal(result)
	if err != nil || len(data) > maxTOTPJournalBytes {
		return errors.New("TOTP_RESET_JOURNAL_TOO_LARGE")
	}
	if err := privatestate.WriteJSON(ctx, path, "totp-reset", str(result["email"]), str(result["operation_id"]), result); err != nil {
		return journalFailure("STORAGE_WRITE_FAILED", err)
	}
	stored, err := ReadTOTPResetJournal(path)
	if err != nil {
		return journalFailure("STORAGE_READBACK_FAILED", err)
	}
	expected, err := decodeTOTPResetJournal(data)
	if err != nil || !reflect.DeepEqual(stored, expected) {
		return journalFailure("STORAGE_READBACK_FAILED", nil)
	}
	return nil
}

func (f *Flow) saveTOTPResetJournal(path string, out map[string]any) error {
	// A display ordinal, not evidence or permission to resume a mutation.
	if f.resetLog != nil {
		out["support_seq"] = f.resetLog.seq
	}
	err := writeTOTPJournal(path, out)
	if err == nil && f.resetLog != nil {
		f.resetLog.material = savedResetMaterial(out)
	}
	return err
}
