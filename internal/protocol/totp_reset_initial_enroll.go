package protocol

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"tosub2-go-port/internal/transport"
)

// Missing/lagging metadata can justify a single enrollment attempt. A business
// refusal, malformed container, multiple factors or recovery factor cannot.
// This never supplies an ID or authorizes removal from an unknown response.
func totpInitialEnrollmentAllowed(info map[string]any) bool {
	if info == nil || totpResponseRejected(info) || !totpOutcomeFieldsValid(info) {
		return false
	}
	if value, exists := info["mfa_enabled_v2"]; exists {
		if _, ok := value.(bool); !ok {
			return false
		}
	}
	value, exists := info["factors"]
	if !exists || value == nil {
		return true
	}
	factors := payload(value)
	if factors == nil {
		return false
	}
	value, exists = factors["totp"]
	if !exists || value == nil {
		return true
	}
	list, ok := value.([]any)
	if !ok || len(list) > 1 {
		return false
	}
	if enabled, ok := info["mfa_enabled_v2"].(bool); ok && !enabled && len(list) != 0 {
		return false
	}
	for _, item := range list {
		factor := payload(item)
		if factor == nil {
			return false
		}
		if value, exists := factor["is_recovery"]; exists && value != false {
			return false
		}
		if value, exists := factor["factor_type"]; exists && value != "totp" {
			return false
		}
		if value, exists := factor["id"]; exists {
			if _, ok := value.(string); !ok {
				return false
			}
		}
	}
	return true
}

func totpInitialStateMessage(state totpLiveState) string {
	switch state.State {
	case totpStateEnabled:
		return "登录已成功，已找到一个有效的 2FA 验证器，准备移除后设置新密钥"
	case totpStateDisabled:
		return "登录已成功，当前状态显示未启用 2FA，准备直接设置新密钥"
	default:
		return "登录已成功，当前查询暂未提供明确可移除的 2FA 验证器。" + state.Summary
	}
}

func resetEnrollmentSource(out map[string]any) string {
	if step := str(out["enrollment_source"]); isResetEnrollmentStep(step) {
		return step
	}
	return "enroll"
}

func isResetEnrollmentStep(step string) bool {
	return step == "enroll" || step == "initial_enroll" || step == "retry_enroll"
}

func resetHasEnrollmentAttempt(out map[string]any) bool {
	return out[resetEnrollmentSource(out)+"_attempted"] == true
}

func resetInitialEnrollmentStrategy(out map[string]any) bool {
	return out["reset_totp_strategy"] == "enroll_first" &&
		(out["initial_totp_state"] == totpStateDisabled || out["initial_totp_state"] == totpStateUnknown)
}

func resetInitialEnrollmentConflict(out map[string]any) bool {
	return resetInitialEnrollmentStrategy(out) && resetEnrollmentConflict(out, "initial_enroll")
}

func resetEnrollmentConflict(out map[string]any, step string) bool {
	body, ok := resetCompleteReceiptBody(out, step)
	return ok && out[step+"_attempted"] == true && enrollmentConflictStatus(receiptStatus(payload(out[step+"_receipt"])["http_status"])) && totpEnrollmentAlreadyExists(body)
}

func enrollmentConflictStatus(status int) bool {
	return status >= 200 && status < 300 || status == 400 || status == 409 || status == 422
}

// Each slot has its own durable intent and receipt. A conflict is a complete
// business refusal, not the absence of a secret or an uncertain network error.
func (f *Flow) sendResetEnrollment(ctx context.Context, token, account, path string, out map[string]any, step string) (bool, error) {
	if step == "retry_enroll" && out[step+"_dispatch"] == nil {
		out[step+"_dispatch"] = "not_sent"
	}
	if out[step+"_dispatch"] != "not_sent" || out[step+"_attempted"] == true || out["activation_attempted"] == true {
		return false, resetError("没有确定的未发送证据，不重复登记。", true)
	}
	out["reset_totp_phase"], out["enrollment_source"], out[step+"_attempted"] = step+"_intent", step, true
	if step == "initial_enroll" {
		out["initial_enroll_attempted_at"] = nowRFC3339()
	}
	if err := f.resetDispatch(ctx, path, out, step); err != nil {
		return false, err
	}
	if step == "initial_enroll" {
		f.emit("totp_reset_initial_enroll", "running", "未移除验证器，正在原会话中首次登记新 2FA")
	} else {
		f.emit("totp_reset_enroll", "running", "正在当前账号的原会话中登记新验证器")
	}
	f.resetCapture.enrollmentStep = step
	enrollment, requestErr := f.totpResetRequest(ctx, token, account, http.MethodPost, "/backend-api/accounts/mfa/enroll", map[string]any{"factor_type": "totp", "source": "settings"})
	f.resetCapture.enrollmentStep = ""
	if transport.IsRequestDefinitelyUnsent(requestErr) {
		out[step+"_dispatch"] = "proven_unsent"
	}
	if resetEnrollmentConflict(out, step) {
		out["reset_totp_phase"] = step + "_conflict"
		if step == "initial_enroll" {
			out["initial_enroll_result"] = "already_exists"
		}
		if err := f.saveTOTPResetJournal(path, out); err != nil {
			return false, errors.Join(requestErr, err)
		}
		return true, nil
	}
	preserveEnrollmentMaterial(out, enrollment, f.Options.Email, requestErr == nil)
	if requestErr != nil && restoreEnrollmentReceipt(out, f.Options.Email) {
		requestErr = nil
	}
	out["reset_totp_phase"] = "enrollment_unverified"
	if step == "initial_enroll" {
		out["reset_totp_phase"] = "initial_enrollment_unverified"
	}
	if out["enrollment_validated"] == true {
		out["reset_totp_phase"] = "enrolled"
		if step == "initial_enroll" {
			out["initial_enroll_result"] = "enrolled"
		}
	} else {
		f.explainEnrollment(step, enrollment)
	}
	if err := f.saveTOTPResetJournal(path, out); err != nil {
		return false, errors.Join(requestErr, err)
	}
	if isTOTPResetUncertain(requestErr) && out["enrollment_validated"] != true {
		return false, &totpResetError{message: "登记结果未知，缺少可靠的新密钥、登记会话或因子绑定；不会重复登记或激活。", uncertain: true, cause: requestErr}
	}
	if requestErr == nil && totpResponseRejected(enrollment) {
		return false, resetError("登记已被拒绝；未发送激活请求。", false)
	}
	return false, requestErr
}

// Quarantine also precedes enroll-first: a new remote credential must never be
// created without a durable operation binding and a place to retain its seed.
func (f *Flow) quarantineTOTPReset(ctx context.Context, path string, out map[string]any) error {
	if out["old_factor_invalidated"] == true {
		if resetInitialEnrollmentConflict(out) {
			return nil
		}
		return resetError("2FA 修改已开始，不能重复准备凭据或重复发送请求", true)
	}
	out["reset_totp_phase"], out["failure_site"] = "quarantine_intent", "quarantine_journal"
	if err := f.saveTOTPResetJournal(path, out); err != nil {
		return err
	}
	out["failure_site"] = "credential_quarantine"
	if err := f.Options.TOTPResetBeforeDisable(ctx); err != nil {
		return &totpResetError{message: "TOTP_RESET_NOT_SENT: 本地重置绑定尚未完成；未发送移除或设置请求", cause: err}
	}
	out["old_factor_invalidated"] = true // Local credential quarantine only.
	out["reset_totp_phase"] = "quarantine_committed"
	return f.saveTOTPResetJournal(path, out)
}

// The probe has its own immutable dispatch/receipt evidence. Only an explicit,
// complete already-enrolled refusal can lead to one subsequent removal and a
// separate enrollment. Network errors and partial responses never do so.
func (f *Flow) tryInitialTOTPEnrollment(ctx context.Context, token, account, path string, out map[string]any) (bool, error) {
	if !resetInitialEnrollmentStrategy(out) || out["initial_enroll_attempted"] != false || !resetRequestUnsent(out, "initial_enroll") {
		return false, resetError("初次设置请求缺少未发送证据，已停止", true)
	}
	if err := f.quarantineTOTPReset(ctx, path, out); err != nil {
		return false, err
	}
	conflict, err := f.sendResetEnrollment(ctx, token, account, path, out, "initial_enroll")
	if err != nil {
		return false, err
	}
	if conflict {
		f.emit("totp_reset_enroll_conflict", "running", "初次登记明确被拒，正在只读核对旧因子；只有匹配本轮已有的因子绑定才允许移除")
		f.decision("initial_enroll", "INITIAL_CONFLICT", "business_receipt")
		state, err := f.readTOTPAfterEnrollmentConflict(ctx, token, account)
		if err != nil {
			return false, err
		}
		if state.State == totpStateDisabled {
			out["post_conflict_totp_state"], out["enrollment_source"] = totpStateDisabled, "enroll"
			out["reset_totp_phase"] = "prepared"
			return true, f.saveTOTPResetJournal(path, out)
		}
		id := str(state.Factors[0]["id"])
		if !f.permitsResetTarget(id, true) {
			f.decision("conflict_read", "TARGET_NOT_AUTHORIZED", "state_read")
			return false, resetStop("TARGET_NOT_AUTHORIZED", "初次登记明确被拒，但当前可见因子未匹配本次登录的旧因子绑定；请在账号安全设置中核对旧验证器，未发送移除请求", false)
		}
		out["old_totp_factor_id"] = id
		f.decision("conflict_read", "TARGET_BOUND", "state_read")
		out["enrollment_source"], out["reset_totp_phase"] = "enroll", "prepared"
		out["post_conflict_totp_summary"] = state.Summary
		return false, f.saveTOTPResetJournal(path, out)
	}
	if _, _, _, valid := resetEnrollmentMaterial(out); !valid {
		return false, resetError("初次设置未返回完整有效的新密钥和验证器信息，也未明确确认已存在 2FA；已停止，未发送移除或重复设置请求", true)
	}
	return true, nil
}

func (f *Flow) readTOTPAfterEnrollmentConflict(ctx context.Context, token, account string) (totpLiveState, error) {
	ctx, cancel := context.WithTimeout(ctx, totpReadDeadline)
	defer cancel()
	var state totpLiveState
	for attempt := 0; attempt < totpStatusChecks; attempt++ {
		info, err := f.totpResetRequest(ctx, token, account, http.MethodGet, "/backend-api/accounts/mfa_info", nil)
		if err != nil && !transientTOTPStatusFailure(err) {
			return state, err
		}
		if err == nil {
			state = classifyTOTPState(info)
			if state.State == totpStateDisabled {
				f.decision("conflict_read", "CONFLICT_STATE_DISABLED", "state_read")
				return state, nil
			}
			if state.State == totpStateEnabled {
				return state, nil
			}
			if !totpInitialEnrollmentAllowed(info) {
				break
			}
		}
		if attempt == totpStatusChecks-1 {
			break
		}
		f.emit("totp_reset_status_pending", "running", fmt.Sprintf("官方已确认存在 2FA，等待验证器信息同步后再读取（%d/%d）", attempt+1, totpStatusChecks))
		f.readAgain("conflict_read", totpStatusChecks-attempt-1)
		if err := pauseTOTPStatusRetry(ctx, attempt); err != nil {
			return state, err
		}
	}
	f.decision("conflict_read", "TARGET_UNRESOLVED", "state_read")
	return state, resetError("登录已成功，登记提示已有 2FA，但复查未能确定普通验证器状态；复查后未发送移除请求或再次登记。"+strings.TrimSpace(state.Summary), false)
}
