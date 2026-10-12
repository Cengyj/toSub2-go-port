package console

import (
	"fmt"
	"tosub2-go-port/internal/jobs"
)

// TOTP security options are explicit booleans. Null/string/number inputs must
// never turn an ordinary enrollment into a destructive factor replacement.
func normalizeTOTPAction(action string, values map[string]any) (string, error) {
	uploadAfterRelogin, err := normalizeReloginUpload(action, values)
	if err != nil {
		return "", err
	}
	reset, logout, relogin, upload := false, false, false, false
	for key, dst := range map[string]*bool{"resetTotp": &reset, "logoutAllDevicesAfterTotp": &logout, "reloginAfterTotp": &relogin, "uploadAfterTotp": &upload} {
		if value, present := values[key]; present {
			b, ok := value.(bool)
			if !ok {
				return "", fmt.Errorf("%s 必须是布尔值", key)
			}
			*dst = b
		}
	}
	if (reset || logout || relogin || upload) && action != "setup-2fa" {
		return "", fmt.Errorf("2FA 重置选项只能用于设置或重置 2FA")
	}
	if logout && !reset {
		return "", fmt.Errorf("本功能仅支持重置 2FA 成功后退出所有设备")
	}
	if upload && !relogin {
		return "", fmt.Errorf("授权后上传 Sub2API 必须同时启用重新登录授权")
	}
	if upload {
		if _, _, err := ParseTOTPSub2APIConfig(values["sub2apiConfig"]); err != nil {
			return "", err
		}
	} else if !uploadAfterRelogin {
		delete(values, "sub2apiConfig")
	}
	if reset {
		return "reset-2fa", nil
	}
	return action, nil
}

func totpRecoveryBlocked(j *jobs.Job) bool {
	return j.TOTPRecoveryPending || j.TOTPCredentialInvalidated
}

// A terminal reset can log in afresh when it either saved the new seed or
// stopped before acknowledging factor removal. Missing old authorization is
// not a reset quota; any uncertain mutation remains blocked.
func canRestartCompletedTOTPReset(j *jobs.Job) bool {
	if totpRecoveryBlocked(j) || !j.ResetTOTP {
		return false
	}
	switch j.TOTPResetPhase {
	case "credential_persisted":
		if !j.HasTOTPKey {
			return false
		}
	case "initial":
		if (!j.HasTOTPKey && !j.TOTPKnownEnabled) || j.TOTPWorkflowStep != "totp" || (j.TOTPResetStatus != "pending" && j.TOTPResetStatus != "failed") {
			return false
		}
	default:
		return false
	}
	switch j.TOTPWorkflowStatus {
	case "succeeded", "failed", "interrupted":
	default:
		return false
	}
	switch j.Status {
	case jobs.StatusImported, jobs.StatusSucceeded, jobs.StatusFailed, jobs.StatusCanceled, "interrupted", "reauth_required", "resume_available":
		return true
	}
	return false
}

// Starting a new reset authenticates afresh. A failed login does not require
// an old grant or checkpoint when the account's current factor is still known.
// Historical mutation evidence and unfinished workflows remain protected.
func canStartFreshTOTPReset(j *jobs.Job) bool {
	if totpRecoveryBlocked(j) || j.ResetTOTP || (!j.HasTOTPKey && !j.TOTPKnownEnabled) || j.TOTPWorkflowStatus != "" {
		return false
	}
	switch j.Operation {
	case "", "login", "full", "relogin":
	default:
		return false
	}
	switch j.TOTPResetPhase {
	case "", "initial", "credential_persisted":
	default:
		return false
	}
	if j.TOTPResetStatus == "unknown" || j.TOTPResetStatus == "disabling" || j.TOTPResetStatus == "disabled" {
		return false
	}
	switch j.Status {
	case jobs.StatusSucceeded, jobs.StatusFailed, jobs.StatusCanceled, "interrupted", "reauth_required", "resume_available":
		return true
	}
	return false
}

func (s *Server) totpUnavailableReason(j *jobs.Job, reset bool) string {
	if totpRecoveryBlocked(j) {
		return "2FA 重置状态尚未确认，请先核实并恢复有效密钥"
	}
	if reset {
		if !j.HasTOTPKey && !j.TOTPKnownEnabled {
			return "尚未确认账号已启用 2FA，请使用首次设置"
		}
		if canRestartCompletedTOTPReset(j) || canStartFreshTOTPReset(j) {
			return ""
		}
		// A retained grant must not override unresolved reset evidence, even
		// when a restored record lacks its usual recovery flags.
		if j.ResetTOTP || j.TOTPWorkflowStatus == "pending" || j.TOTPWorkflowStatus == "running" || j.TOTPWorkflowStatus == "unknown" || j.TOTPResetStatus == "unknown" || j.TOTPResetStatus == "disabling" || j.TOTPResetStatus == "disabled" {
			return "当前 2FA 重置尚未结束或结果未确认，请等待完成或核实有效密钥"
		}
		switch j.TOTPResetPhase {
		case "", "initial", "credential_persisted":
		default:
			return "2FA 修改结果尚未确认，请先核实并恢复有效密钥"
		}
	} else if j.HasTOTPKey || j.TOTPKnownEnabled {
		return "账号已启用 2FA；更换密钥请使用重置 2FA"
	}
	if !s.canSecurityOperation(j) {
		return "请等待当前任务结束，或先完成登录后再操作 2FA"
	}
	return ""
}

func (s *Server) publicTOTPState(j *jobs.Job, out map[string]any) map[string]any {
	setupReason, resetReason := s.totpUnavailableReason(j, false), s.totpUnavailableReason(j, true)
	out["totpKnownEnabled"] = j.TOTPKnownEnabled
	out["canSetupTotp"], out["canResetTotp"] = setupReason == "", resetReason == ""
	out["totpSetupUnavailableReason"], out["totpResetUnavailableReason"] = setupReason, resetReason
	out["resetTotp"], out["totpResetStatus"], out["totpResetPhase"] = j.ResetTOTP, j.TOTPResetStatus, j.TOTPResetPhase
	out["totpResetError"], out["totpRecoveryPending"], out["totpCredentialInvalidated"] = j.TOTPResetError, j.TOTPRecoveryPending, j.TOTPCredentialInvalidated
	out["logoutAllDevicesAfterTotp"], out["logoutAllDevicesStatus"], out["logoutAllDevicesError"] = j.LogoutAllDevicesAfterTOTP, j.LogoutAllDevicesStatus, j.LogoutAllDevicesError
	out["reloginAfterTotp"], out["uploadAfterTotp"] = j.ReloginAfterTOTP, j.UploadAfterTOTP
	out["totpWorkflowStatus"], out["totpWorkflowStep"], out["totpWorkflowError"] = j.TOTPWorkflowStatus, j.TOTPWorkflowStep, j.TOTPWorkflowError
	out["totpReloginStatus"], out["totpUploadStatus"] = j.TOTPReloginStatus, j.TOTPUploadStatus
	if totpRecoveryBlocked(j) {
		for _, key := range []string{"canRetry", "canResume", "canRegenerate", "canForceRelogin", "canAddPassword", "autoRepairEligible"} {
			out[key] = false
		}
		out["autoRepairEligibilityReason"] = "2FA 重置恢复尚未完成"
	}
	return out
}
