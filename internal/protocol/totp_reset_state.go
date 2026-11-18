package protocol

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"

	"tosub2-go-port/internal/transport"
)

func mutationEvidenceVersion(out map[string]any) bool {
	switch v := out["mutation_schema"].(type) {
	case int:
		return v == 1
	case float64:
		return v == 1
	}
	return false
}

// validateResetMutationEvidence rejects contradictory new-schema records before
// issuing any request. Legacy records remain conservative.
func validateResetMutationEvidence(out map[string]any) error {
	// Removed reauthentication retry histories cannot authorize new work.
	for _, key := range []string{"disable_attempt_history", "enroll_attempt_history", "reauthentication_renewals"} {
		if v := out[key]; v != nil && len(slice(v)) > 0 {
			return errors.New("TOTP_RESET_LEGACY_REVIEW_REQUIRED: 旧重认证重试记录需要人工处理")
		}
	}
	if _, exists := out["mutation_schema"]; !exists {
		for key := range out {
			if key == "reset_totp_strategy" || key == "post_conflict_totp_state" || strings.HasPrefix(key, "initial_enroll_") || strings.HasPrefix(key, "retry_enroll_") || key == "enrollment_source" && out[key] != "enroll" {
				return errors.New("TOTP_RESET_JOURNAL_INVALID: 初次设置证据缺少有效版本")
			}
		}
		return nil
	}
	invalid := errors.New("TOTP_RESET_JOURNAL_INVALID: mutation dispatch evidence is incomplete or inconsistent")
	if !mutationEvidenceVersion(out) {
		return invalid
	}
	steps := []string{"disable", "enroll", "activation"}
	// A second enrollment without another removal must follow a complete
	// refusal for this operation and a fresh, explicitly disabled state.
	noRemovalRetry := false
	if state, present := out["post_conflict_totp_state"]; present {
		if state != totpStateDisabled || out["old_factor_invalidated"] != true {
			return invalid
		}
		if resetInitialEnrollmentStrategy(out) {
			noRemovalRetry = out["disable_dispatch"] == "not_sent" && str(out["old_totp_factor_id"]) == "" && resetInitialEnrollmentConflict(out) && out["enrollment_source"] == "enroll"
			if !noRemovalRetry {
				return invalid
			}
			if out["enroll_dispatch"] == "may_have_been_sent" && (str(out["enroll_attempt_id"]) == "" || out["enroll_attempt_id"] == out["initial_enroll_attempt_id"]) {
				return invalid
			}
		} else {
			if !(acknowledgedResetRemoval(out) || out["old_factor_removal_confirmed_by_read"] == true) || !resetEnrollmentConflict(out, "enroll") || out["enrollment_source"] != "retry_enroll" || out["retry_enroll_attempted"] != true {
				return invalid
			}
			id := str(out["retry_enroll_attempt_id"])
			if id != "" && id == str(out["enroll_attempt_id"]) || id == "" && out["retry_enroll_dispatch"] == "may_have_been_sent" || out["retry_enroll_receipt"] != nil && resetRequestUnsent(out, "retry_enroll") {
				return invalid
			}
			steps = append(steps, "retry_enroll")
		}
	}
	if out["enrollment_source"] != "retry_enroll" || out["post_conflict_totp_state"] == nil {
		for key := range out {
			if strings.HasPrefix(key, "retry_enroll_") || key == "enrollment_source" && out[key] == "retry_enroll" {
				return invalid
			}
		}
	}
	if _, exists := out["reset_totp_strategy"]; exists {
		if !resetInitialEnrollmentStrategy(out) {
			return invalid
		}
		if _, ok := out["initial_enroll_attempted"].(bool); !ok {
			return invalid
		}
		steps = append(steps, "initial_enroll")
	} else {
		for key := range out {
			if strings.HasPrefix(key, "initial_enroll_") || key == "enrollment_source" && out[key] == "initial_enroll" {
				return invalid
			}
		}
	}
	for _, step := range steps {
		switch state, ok := out[step+"_dispatch"].(string); {
		case !ok:
			return invalid
		case state == "not_sent" || state == "proven_unsent":
		case state == "may_have_been_sent":
			if step == "disable" {
				if str(out["reset_totp_attempted_at"]) == "" || out["old_factor_invalidated"] != true {
					return invalid
				}
			} else if out[step+"_attempted"] != true {
				return invalid
			}
		default:
			return invalid
		}
	}
	if resetRequestUnsent(out, "disable") && (out["old_factor_removal_acknowledged"] == true || out["old_factor_removal_confirmed_by_read"] == true || !noRemovalRetry && (out["enroll_attempted"] == true || out["activation_attempted"] == true && resetEnrollmentSource(out) != "initial_enroll")) {
		return invalid
	}
	if resetInitialEnrollmentStrategy(out) {
		if out["initial_enroll_attempted"] == false {
			if out["initial_enroll_dispatch"] != "not_sent" {
				return invalid
			}
			for _, key := range []string{"initial_enroll_attempt_id", "initial_enroll_attempted_at", "initial_enroll_receipt", "initial_enroll_result"} {
				if _, exists := out[key]; exists {
					return invalid
				}
			}
		} else if strings.TrimSpace(str(out["initial_enroll_attempted_at"])) == "" {
			return invalid
		}
		if out["initial_enroll_dispatch"] == "may_have_been_sent" && strings.TrimSpace(str(out["initial_enroll_attempt_id"])) == "" {
			return invalid
		}
		if out["initial_enroll_receipt"] != nil && resetRequestUnsent(out, "initial_enroll") {
			return invalid
		}
		if out["initial_enroll_attempted"] == true && out["old_factor_invalidated"] != true {
			return invalid
		}
		if !resetRequestUnsent(out, "disable") && !resetInitialEnrollmentConflict(out) {
			return invalid
		}
		if source := str(out["enrollment_source"]); source != "" && source != "initial_enroll" && source != "enroll" {
			return invalid
		}
	}
	if resetRequestUnsent(out, resetEnrollmentSource(out)) {
		for _, key := range []string{"secret", "session_id", "new_totp_factor_id", "enrollment_factor"} {
			if _, exists := out[key]; exists {
				return invalid
			}
		}
	}
	if resetRequestUnsent(out, "activation") && (out["activation_succeeded"] == true || out["activation_verified"] == true || out["credential_persisted"] == true) {
		return invalid
	}
	return nil
}

func resetRequestUnsent(out map[string]any, step string) bool {
	if !mutationEvidenceVersion(out) {
		return false
	}
	s := str(out[step+"_dispatch"])
	return s == "not_sent" || s == "proven_unsent"
}

func resetFailureSummary(err error) string {
	var failure *totpResetError
	if errors.As(err, &failure) {
		return failure.message
	}
	if errors.Is(err, ErrResetStorageBlocked) {
		return "TOTP_RESET_STORAGE_BLOCKED: 回执尚未可靠保存，已停止后续修改"
	}
	var httpFailure *HTTPError
	if errors.As(err, &httpFailure) {
		if httpFailure.StatusCode == 401 {
			return "TOTP_RESET_SESSION_REJECTED: 官方要求重新认证；未重新登录"
		}
		if httpFailure.StatusCode == 403 {
			return "TOTP_RESET_ACCESS_DENIED: 官方拒绝访问或要求验证；未重新登录"
		}
		return fmt.Sprintf("TOTP_RESET_HTTP_ERROR: 官方读取返回 HTTP %d，尚未核实结果", httpFailure.StatusCode)
	}
	if transport.IsRetryableReadError(err) {
		return "TOTP_RESET_NETWORK_UNAVAILABLE: " + transport.ReadFailureMessage(err)
	}
	if errors.Is(err, context.Canceled) {
		return "TOTP_RESET_CANCELED: 操作已取消，已保存的操作证据保留"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "TOTP_RESET_TIMEOUT: 操作超时，不能将超时当作远端拒绝"
	}
	if errors.Is(err, os.ErrPermission) {
		return "TOTP_RESET_STORAGE_DENIED: 本地操作证据保存或读取权限不足"
	}
	return "TOTP_RESET_LOCAL_STATE_ERROR: 本地操作证据保存或校验未完成，已保留最近可靠记录"
}

// The caller owns live control-flow evidence when returning before Request.
// A crash after publishing may_have_been_sent remains conservatively unknown.
func (f *Flow) resetDispatch(ctx context.Context, path string, out map[string]any, step string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	out[step+"_dispatch"] = "may_have_been_sent"
	out[step+"_attempt_id"] = randomUUID()
	out["failure_site"] = step + "_dispatch_journal"
	if err := f.saveTOTPResetJournal(path, out); err != nil {
		out[step+"_dispatch"] = "proven_unsent"
		return err
	}
	if _, err := ReadTOTPResetIdentity(path, f.Options.Email); err != nil {
		out[step+"_dispatch"] = "proven_unsent"
		return err
	}
	if err := ctx.Err(); err != nil {
		out[step+"_dispatch"] = "proven_unsent"
		return err
	}
	out["failure_site"] = step + "_request"
	return nil
}

// removePreparedTOTP sends the first removal only from verified unsent evidence.
func (f *Flow) removePreparedTOTP(ctx context.Context, token, account, path string, out map[string]any) error {
	if !resetRequestUnsent(out, "disable") || out["enroll_attempted"] != false || out["activation_attempted"] != false {
		return resetError("TOTP_RESET_DISPATCH_UNKNOWN: 缺少未发送证据，不能再次移除旧因子", true)
	}
	if strings.TrimSpace(str(out["old_totp_factor_id"])) == "" {
		return resetError("未取得明确可移除的旧验证器，未发送移除请求", false)
	}
	if out["initial_enroll_attempted"] == true && !resetInitialEnrollmentConflict(out) {
		return resetError("初次设置结果尚未确认，不能移除旧验证器", true)
	}
	if err := f.quarantineTOTPReset(ctx, path, out); err != nil {
		return err
	}
	out["reset_totp_status"], out["reset_totp_phase"] = "disabling", "disable_intent"
	out["reset_totp_attempted_at"] = nowRFC3339()
	if err := f.resetDispatch(ctx, path, out, "disable"); err != nil {
		return err
	}
	f.emit("totp_reset_disable", "running", "正在原会话中首次移除已核实的旧因子；未知结果不会重放")
	disabled, err := f.totpResetRequest(ctx, token, account, http.MethodPost, "/backend-api/accounts/mfa/user/disable_in_house", map[string]any{"factor_id": str(out["old_totp_factor_id"])})
	if err != nil && acknowledgedResetRemoval(out) {
		// The failure concerned publishing a complete receipt locally, not the
		// remote outcome. Retry that barrier before proceeding, never a GET.
		if saveErr := f.saveTOTPResetJournal(path, out); saveErr != nil {
			return errors.Join(err, saveErr)
		}
		disabled, _ = resetReceiptBody(out, "disable")
		err = nil
	}
	if err == nil {
		switch classifyTOTPMutation("disable", disabled, str(out["old_totp_factor_id"]), "") {
		case totpMutationRejected:
			err = resetError("Removal was explicitly refused; no new factor was enrolled.", false)
		case totpMutationUnknown:
			err = resetError("Removal response did not confirm the selected factor; checking its live state without repeating removal.", true)
		}
	}
	if err != nil {
		if transport.IsRequestDefinitelyUnsent(err) {
			out["disable_dispatch"] = "proven_unsent"
			return &totpResetError{message: "TOTP_RESET_NOT_SENT: 传输确认移除请求未发送，操作证据已保留，需要人工处理", cause: err}
		}
		if !isTOTPResetUncertain(err) {
			return err
		}
		out["disable_response_error"], out["reset_totp_phase"] = err.Error(), "disable_unconfirmed"
	} else {
		out["old_factor_removal_acknowledged"], out["reset_totp_phase"] = true, "disable_acknowledged"
		out["disable_confirmation_source"] = "business_receipt"
		out["reset_totp_status"] = "disabled"
	}
	out["failure_site"] = "disable_response_journal"
	if saveErr := f.saveTOTPResetJournal(path, out); saveErr != nil {
		return errors.Join(err, saveErr)
	}
	if err == nil {
		f.emit("totp_reset_disabled", "running", "官方已确认所选旧 2FA 因子移除成功，接下来设置新密钥")
		f.decision("disable", "REMOVAL_ACCEPTED", "business_receipt")
		// A complete successful removal response is sufficient to continue in
		// this session. A redundant status read must not strand the account
		// between removing its old factor and enrolling the replacement.
		return nil
	}
	out["failure_site"] = "disable_state_read"
	f.emit("totp_reset_verify_disabled", "running", "移除响应尚未确认，正在原会话中查询网页使用的 2FA 状态；不会重复移除因子")
	if _, err := f.waitForTOTPResetState(ctx, token, account, totpStateDisabled, "", str(out["old_totp_factor_id"])); err != nil {
		return &totpResetError{message: "旧 2FA 停用状态未能核实；尚未创建新密钥，操作证据已保留。" + resetFailureSummary(err), uncertain: true, cause: err}
	}
	f.emit("totp_reset_disabled", "running", "本次查询已返回未启用 2FA 的状态，接下来设置新密钥")
	f.decision("verify_disable", "REMOVAL_READ_MATCH", "state_read")
	out["old_factor_removal_confirmed_by_read"] = true
	out["disable_confirmation_source"] = "state_read"
	out["reset_totp_status"], out["reset_totp_phase"] = "disabled", "disabled_verified"
	return f.saveTOTPResetJournal(path, out)
}

func wrapTOTPResetFailure(code string, out map[string]any, err error) error {
	return &totpResetError{message: fmt.Sprintf("%s: %s", code, str(out["reset_totp_error"])), uncertain: out["reset_totp_status"] == "unknown", cause: publicResetCause(err)}
}

// Expose classifications needed by callers, not an arbitrary provider, URL,
// filesystem or injected transport error deeper in the public error chain.
func publicResetCause(err error) error {
	var causes []error
	for _, s := range []error{context.Canceled, context.DeadlineExceeded, ErrBrowserChallenge, ErrSessionExpired, ErrResetStorageBlocked, os.ErrPermission} {
		if errors.Is(err, s) {
			causes = append(causes, s)
		}
	}
	var t *transport.TransportError
	if errors.As(err, &t) {
		causes = append(causes, t)
	}
	var s *ResetStorageFailure
	if errors.As(err, &s) {
		causes = append(causes, &ResetStorageFailure{Stage: s.Stage, Reason: s.Reason, Message: "本地存储屏障未通过", matches: s.matches})
	}
	var h *HTTPError
	if errors.As(err, &h) {
		causes = append(causes, &HTTPError{StatusCode: h.StatusCode})
	}
	return errors.Join(causes...)
}
