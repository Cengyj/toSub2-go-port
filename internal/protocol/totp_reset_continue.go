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
	ok = resetHasEnrollmentAttempt(out) && out["enrollment_validated"] == true && e == nil && strings.TrimSpace(sid) != "" && strings.TrimSpace(id) != "" && id != str(out["old_totp_factor_id"]) && str(factor["id"]) == id && factor["factor_type"] == "totp" && factor["is_recovery"] == false
	return
}

// continueTOTPReset follows a durably acknowledged removal in the same run, or
// a live read confirming disabled during reconciliation, or an already accepted
// initial enrollment (without sending enrollment again). Durable attempted flags
// are write-ahead intents, not claims of remote success.
func (f *Flow) continueTOTPReset(ctx context.Context, token, account, path string, out map[string]any) error {
	save := func() error { return f.saveTOTPResetJournal(path, out) }
	if !resetHasEnrollmentAttempt(out) {
		conflict, err := f.sendResetEnrollment(ctx, token, account, path, out, "enroll")
		if err != nil {
			return err
		}
		if conflict && !resetInitialEnrollmentStrategy(out) {
			// The first enrollment followed a removal. That removal budget is
			// spent; a fresh read can only authorize a no-removal second attempt.
			f.decision("enroll", "FIRST_CONFLICT_AFTER_REMOVE", "business_receipt")
			state, err := f.readTOTPAfterEnrollmentConflict(ctx, token, account)
			if err != nil {
				return err
			}
			if state.State != totpStateDisabled {
				f.decision("conflict_read", "REMOVAL_BUDGET_USED", "state_read")
				return resetStop("REMOVAL_BUDGET_USED", "本轮已移除过旧验证器，复查仍显示验证器；未再次移除或登记，请核对账号安全设置。", false)
			}
			out["post_conflict_totp_state"] = totpStateDisabled
			conflict, err = f.sendResetEnrollment(ctx, token, account, path, out, "retry_enroll")
			if err != nil {
				return err
			}
		}
		if conflict {
			f.decision(resetEnrollmentSource(out), "FORMAL_CONFLICT", "business_receipt")
			return resetStop("FORMAL_CONFLICT", "第二次登记仍明确提示已有验证器；未再移除、登记或激活。", false)
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
	f.decision(resetEnrollmentSource(out), "ENROLLMENT_BOUND", "business_receipt")
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
	out["pending_credential_readback"] = true
	f.decision("pending", "PENDING_VERIFIED", "credential_readback")
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
	f.emit("totp_reset_activate", "running", "新 2FA 密钥已安全保存，正在提交验证码确认启用")
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
	acknowledged := acknowledgedResetActivation(out)
	if acknowledged {
		out["activation_succeeded"], out["activation_acknowledged"] = true, true
		out["activated_at"], out["reset_totp_phase"] = nowRFC3339(), "activation_acknowledged"
	} else {
		if requestErr == nil {
			requestErr = resetError("激活回执未能绑定到本次登记，只读核对，不重复激活。", true)
		}
		out["activation_response_error"], out["reset_totp_phase"] = requestErr.Error(), "activation_unconfirmed"
	}
	if acknowledged {
		if account == "" || account != str(out["account_id"]) || out["pending_credential_readback"] != true || receiptStatus(out["activation_evidence_schema"]) != 2 {
			return resetError("激活回执已收到，但操作绑定或本地待保存凭据不一致；未提交或重复激活。", true)
		}
		out["activation_verified"], out["activation_confirmation_source"] = true, "business_receipt"
		out["activation_list_check_skipped"] = true // display only, never proof of visibility
		out["activation_evidence"] = resetActivationEvidence(out)
		f.decision("activation", "ACTIVATION_RECEIPT_CONFIRMED", "business_receipt")
		return f.commitConfirmedTOTPReset(ctx, path, out)
	}
	if err = save(); err != nil {
		return errors.Join(requestErr, err)
	}
	if !isTOTPResetUncertain(requestErr) {
		return requestErr
	}
	f.emit("totp_reset_verify_enabled", "running", "激活响应尚未确认，正在原会话中只读核对本次新因子；不会重复激活")
	// A matching list entry can reconcile a lost receipt; it does not prove exclusivity.
	state, err := f.waitForTOTPResetState(ctx, token, account, totpStateEnabled, newID, str(out["old_totp_factor_id"]))
	if err != nil {
		message := "本次新 2FA 与当前返回的因子尚未核对一致；新密钥和登记材料已保留，未重复设置、激活或移除。"
		return &totpResetError{message: message + resetFailureSummary(err), uncertain: true, cause: err}
	}
	return f.commitReconciledTOTPReset(ctx, path, out, account, state)
}

func (f *Flow) commitReconciledTOTPReset(ctx context.Context, path string, out map[string]any, account string, state totpLiveState) error {
	_, _, id, valid := resetEnrollmentMaterial(out)
	if !valid || account == "" || account != str(out["account_id"]) || !state.matches(id) || out["activation_attempted"] != true {
		return resetError("Live factor has no complete matching saved enrollment; credential commit is blocked.", true)
	}
	out["activation_succeeded"], out["activation_verified"], out["activation_confirmed_by_read"] = true, true, true
	out["activation_confirmation_source"] = "state_read"
	out["activation_state_checked_at"] = nowRFC3339()
	out["activation_evidence_schema"] = 3
	out["activation_evidence"] = resetActivationEvidence(out)
	out["old_factor_also_visible"] = state.matches(str(out["old_totp_factor_id"]))
	// Display-only snapshot of this existing read, never authority to remove another factor.
	out["verified_ordinary_factor_count"] = len(state.Factors)
	f.emit("totp_reset_factor_confirmed", "running", "本次只读查询返回的 2FA 因子与新密钥登记记录一致，正在确认本地凭据保存；其他会话仍可能随后修改账号")
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
	out["verified_journal_readback"] = true
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := f.Options.TOTPResetPersist(ctx, secret, uri); err != nil {
		return &totpResetError{message: "本次激活已有确认依据，但本地凭据提交未确认。可仅补本地保存，无需重新设置。", uncertain: true, cause: err}
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
