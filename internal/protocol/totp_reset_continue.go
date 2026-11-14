package protocol

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"tosub2-go-port/internal/transport"
)

func isTOTPResetUncertain(err error) bool {
	var failure *totpResetError
	return errors.As(err, &failure) && failure.uncertain
}

func resetEnrollmentMaterial(out map[string]any) (secret, sid, id string, ok bool) {
	secret, sid, id = normalizeTOTPSecret(str(out["secret"])), str(out["session_id"]), str(out["new_totp_factor_id"])
	_, e := BuildTOTPURI(str(out["email"]), secret)
	factor := payload(out["enrollment_factor"])
	ok = out["enroll_attempted"] == true && out["enrollment_validated"] == true && e == nil && strings.TrimSpace(sid) != "" && strings.TrimSpace(id) != "" && id != str(out["old_totp_factor_id"]) && str(factor["id"]) == id && factor["factor_type"] == "totp" && factor["is_recovery"] == false
	return
}

// continueTOTPReset follows a durably acknowledged removal in the same run, or
// a live read confirming disabled during reconciliation. Durable attempted flags
// are write-ahead intents, not claims of remote success.
func (f *Flow) continueTOTPReset(ctx context.Context, token, account, path string, out map[string]any) error {
	save := func() error { return f.saveTOTPResetJournal(path, out) }
	if out["enroll_attempted"] != true {
		out["reset_totp_phase"], out["enroll_attempted"] = "enroll_intent", true
		if err := save(); err != nil {
			return err
		}
		if _, err := ReadTOTPResetIdentity(path, f.Options.Email); err != nil {
			return err
		}
		if err := f.resetDispatch(ctx, path, out, "enroll"); err != nil {
			return err
		}
		f.emit("totp_reset_enroll", "running", "已确认旧 2FA 停用，正在保留的会话中创建新密钥")
		enrollment, requestErr := f.totpResetRequest(ctx, token, account, http.MethodPost, "/backend-api/accounts/mfa/enroll", map[string]any{"factor_type": "totp", "source": "settings"})
		if transport.IsRequestDefinitelyUnsent(requestErr) {
			out["enroll_dispatch"] = "proven_unsent"
		}
		preserveEnrollmentMaterial(out, enrollment, f.Options.Email, requestErr == nil)
		out["reset_totp_phase"] = "enrollment_unverified"
		if out["enrollment_validated"] == true {
			out["reset_totp_phase"] = "enrolled"
		}
		if saveErr := save(); saveErr != nil {
			return errors.Join(requestErr, saveErr)
		}
		if requestErr != nil && restoreEnrollmentReceipt(out, f.Options.Email) {
			if err := save(); err != nil {
				return errors.Join(requestErr, err)
			}
			requestErr = nil
		}
		if requestErr != nil {
			if isTOTPResetUncertain(requestErr) && out["enrollment_validated"] != true {
				return &totpResetError{message: "登记结果未知，缺少完整可靠的新密钥、登记会话或因子绑定；已保留收到的材料，不会重复登记。", uncertain: true, cause: requestErr}
			}
			return requestErr
		}
		if totpResponseRejected(enrollment) {
			return resetError("New TOTP enrollment was explicitly refused; activation was not sent.", false)
		}
	}
	secret, sid, newID, valid := resetEnrollmentMaterial(out)
	if !valid {
		return resetError("New enrollment did not contain a complete valid distinct factor; recognized operation material was preserved and enrollment will not be replayed.", true)
	}
	if out["activation_attempted"] == true {
		return resetError("Activation was already dispatched; only live state reconciliation is allowed.", true)
	}
	uri, _ := BuildTOTPURI(f.Options.Email, secret)
	out["reset_totp_phase"] = "enrolled"
	if err := save(); err != nil {
		return err
	}
	if f.Options.TOTPEnrollment == nil {
		return errors.New("TOTP_RESET_PERSISTENCE_REQUIRED: 待激活密钥必须先可靠保存并读回")
	}
	if err := f.Options.TOTPEnrollment(ctx, secret, uri); err != nil {
		return err
	}
	var err error
	out["reset_totp_phase"], out["activation_attempted"] = "activation_intent", true
	if err = save(); err != nil {
		return err
	}
	if _, err = ReadTOTPResetIdentity(path, f.Options.Email); err != nil {
		return err
	}
	if err = f.resetDispatch(ctx, path, out, "activation"); err != nil {
		return err
	}
	// Journal publication and readback can cross a TOTP time window. Generate
	// the code only after those synchronous writes, immediately before dispatch.
	code, err := GenerateTOTP(secret, time.Now())
	if err != nil {
		out["activation_dispatch"] = "proven_unsent"
		return err
	}
	activation, requestErr := f.totpResetRequest(ctx, token, account, http.MethodPost, "/backend-api/accounts/mfa/user/activate_enrollment", map[string]any{"factor_type": "totp", "session_id": sid, "code": code, "source": "settings"})
	if transport.IsRequestDefinitelyUnsent(requestErr) {
		out["activation_dispatch"] = "proven_unsent"
	}
	if requestErr == nil {
		switch classifyTOTPMutation("activation", activation, newID, sid) {
		case totpMutationRejected:
			requestErr = resetError("New TOTP activation was explicitly refused; activation will not be repeated.", false)
		case totpMutationUnknown:
			requestErr = resetError("New TOTP activation response did not confirm success; checking live state without repeating activation.", true)
		}
	}
	if requestErr == nil {
		out["activation_succeeded"], out["activation_acknowledged"] = true, true
		out["activated_at"], out["reset_totp_phase"] = nowRFC3339(), "activation_acknowledged"
	} else {
		out["activation_response_error"], out["reset_totp_phase"] = requestErr.Error(), "activation_unconfirmed"
	}
	if err = save(); err != nil {
		return errors.Join(requestErr, err)
	}
	if acknowledgedResetActivation(out) {
		return f.commitAcknowledgedTOTPReset(ctx, path, out, account)
	}
	if requestErr != nil && !isTOTPResetUncertain(requestErr) {
		return requestErr
	}
	if requestErr == nil {
		return f.commitAcknowledgedTOTPReset(ctx, path, out, account)
	}
	f.emit("totp_reset_verify_enabled", "running", "正在原会话中核实新因子；不会重复激活")
	state, err := f.waitForTOTPResetState(ctx, token, account, totpStateEnabled, newID, str(out["old_totp_factor_id"]))
	if err != nil {
		return &totpResetError{message: "新 2FA 因子状态未能核实；新密钥和会话已保留，后续修改已停止。" + resetFailureSummary(err), uncertain: true, cause: err}
	}
	return f.commitReconciledTOTPReset(ctx, path, out, account, state)
}

func (f *Flow) commitReconciledTOTPReset(ctx context.Context, path string, out map[string]any, account string, state totpLiveState) error {
	_, _, id, valid := resetEnrollmentMaterial(out)
	if !valid || account == "" || account != str(out["account_id"]) || state.State != totpStateEnabled || len(state.Factors) != 1 || str(state.Factors[0]["id"]) != id || out["activation_attempted"] != true {
		return resetError("Live factor has no complete matching saved enrollment; credential commit is blocked.", true)
	}
	out["activation_succeeded"], out["activation_verified"], out["activation_confirmed_by_read"] = true, true, true
	out["activation_confirmation_source"] = "state_read"
	out["activation_evidence_schema"] = 2
	out["activation_evidence"] = resetActivationEvidence(out)
	return f.commitConfirmedTOTPReset(ctx, path, out)
}

func (f *Flow) commitAcknowledgedTOTPReset(ctx context.Context, path string, out map[string]any, account string) error {
	if account == "" || account != str(out["account_id"]) || !acknowledgedResetActivation(out) {
		return resetError("Activation receipt does not match this account and saved enrollment; credential commit is blocked.", true)
	}
	out["activation_succeeded"], out["activation_acknowledged"], out["activation_verified"] = true, true, true
	out["activation_confirmation_source"] = "business_receipt"
	out["activation_evidence_schema"] = 2
	out["activation_evidence"] = resetActivationEvidence(out)
	return f.commitConfirmedTOTPReset(ctx, path, out)
}

func (f *Flow) commitConfirmedTOTPReset(ctx context.Context, path string, out map[string]any) error {
	if !verifiedResetActivation(out) {
		return resetError("Saved activation confirmation is incomplete or inconsistent; credential commit is blocked.", true)
	}
	secret, _, _, _ := resetEnrollmentMaterial(out)
	uri, _ := BuildTOTPURI(f.Options.Email, secret)
	out["reset_totp_status"], out["reset_totp_phase"], out["reset_totp_error"] = "activated", "activated_verified", nil
	if str(out["activated_at"]) == "" {
		out["activated_at"] = nowRFC3339()
	}
	if err := f.saveTOTPResetJournal(path, out); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := f.Options.TOTPResetPersist(ctx, secret, uri); err != nil {
		return &totpResetError{message: "New TOTP is active, but credential persistence was not confirmed; the activation evidence and key are retained for local completion.", uncertain: true, cause: err}
	}
	out["credential_persisted"], out["reset_totp_phase"] = true, "credential_persisted"
	{
		out["logout_all_devices_error"] = nil
		if out["logout_all_devices_requested"] == true {
			out["logout_all_devices_status"] = "pending"
		}
	}
	return f.saveTOTPResetJournal(path, out)
}
