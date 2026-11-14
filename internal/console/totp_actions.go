package console

import (
	"fmt"
	"tosub2-go-port/internal/application"
	"tosub2-go-port/internal/jobs"
)

// TOTP security options are explicit booleans. Null/string/number inputs must
// never turn an ordinary enrollment into a destructive factor replacement.
func normalizeTOTPAction(action string, values map[string]any) (string, error) {
	if err := rejectRemovedAuthenticationInputs(values); err != nil {
		return "", err
	}
	if err := jobs.ValidateTOTPIntent(values); err != nil {
		return "", err
	}
	if action == "recover-2fa" || action == "inspect-2fa" {
		return "", fmt.Errorf("旧版 2FA 恢复与核实操作已移除")
	}
	if action == "complete-2fa" {
		allowed := 0
		if _, ok := values["totpIntentId"]; ok {
			allowed = 1
		}
		if len(values) != allowed {
			return "", fmt.Errorf("完成本地保存只接受操作意图标识，不接受登录资料、代理或后续操作选项")
		}
		return action, nil
	}
	if _, err := jobs.AuthorizationMethodForValues("", values); err != nil {
		return "", err
	}
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
		return "", fmt.Errorf("授权后上传必须同时启用重新登录授权")
	}
	if _, selected := values["authorizationMethod"]; selected && action != "relogin" && action != "regenerate" && action != "reauthorize" && !(action == "setup-2fa" && relogin) {
		return "", fmt.Errorf("授权方式仅用于登录授权或 2FA 完成后的重登授权")
	}
	if upload {
		if err := application.ValidateUploadSettings(values); err != nil {
			return "", err
		}
	} else if !uploadAfterRelogin {
		delete(values, "sub2apiConfig")
		delete(values, "cpaConfig")
	}
	if reset {
		return "reset-2fa", nil
	}
	return action, nil
}

func totpRecoveryBlocked(j *jobs.Job) bool {
	return j.TOTPRecoveryPending || j.TOTPCredentialInvalidated
}

// A terminal reset can admit another explicit operation after saving the new
// seed, including completed metadata from before per-operation commit evidence.
// Any uncertain mutation keeps its isolation gate.
func canRestartCompletedTOTPReset(j *jobs.Job) bool {
	if totpRecoveryBlocked(j) || !j.ResetTOTP {
		return false
	}
	switch j.TOTPResetPhase {
	case "credential_persisted":
		if !j.HasTOTPKey || (!j.TOTPResetCommitted && !canRestartLegacyCompletedTOTPReset(j)) {
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

// This only admits a new user request. It does not invent activation evidence
// or mark an old operation committed. Journal, vault and account gates still
// apply before admission, and the new run must confirm its own live factor.
func canRestartLegacyCompletedTOTPReset(j *jobs.Job) bool {
	if j.TOTPResetOperationID != "" || j.TOTPActivationVerified || j.TOTPResetCommitted ||
		j.Status != jobs.StatusSucceeded || j.OperationStatus != "succeeded" || j.Stage != "complete" ||
		!j.TOTPKnownEnabled || j.TOTPResetStatus != "activated" || j.TOTPWorkflowStatus != "succeeded" ||
		j.TOTPResetError != "" || j.TOTPSetupError != "" || j.TOTPWorkflowError != "" {
		return false
	}
	return (!j.LogoutAllDevicesAfterTOTP || j.LogoutAllDevicesStatus == "succeeded") &&
		(!j.ReloginAfterTOTP || j.TOTPReloginStatus == "succeeded") &&
		(!j.UploadAfterTOTP || j.TOTPUploadStatus == "succeeded")
}

// Each new reset performs an ordinary login and live identity checks. A failed
// login does not require a historical grant when the factor is still known.
// Unfinished mutation evidence remains protected.
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
		return "2FA 修改尚未完成，请先完成本地保存；结果未知时需人工处理"
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
			return "当前 2FA 重置尚未结束或结果未确认，请等待完成或人工处理"
		}
		switch j.TOTPResetPhase {
		case "", "initial", "credential_persisted":
		default:
			return "2FA 修改结果尚未确认，需人工处理"
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
	out["totpExportBlocked"], out["totpExportUnavailableReason"] = false, ""
	setupReason, resetReason := s.totpUnavailableReason(j, false), s.totpUnavailableReason(j, true)
	completionPending := s.jobs != nil && s.jobs.TOTPResetCompletionPending(j.ID)
	canComplete := false
	completionReason := "当前任务没有可完成本地保存的 2FA 成功记录"
	if completionPending {
		setupReason, resetReason = "新 2FA 已确认成功，请先完成本地保存", "新 2FA 已确认成功，请先完成本地保存"
		if j.Status == jobs.StatusQueued || isActiveStatus(j.Status) {
			completionReason = "请等待当前任务结束后完成本地保存"
		} else if err := s.jobs.CanCompleteTOTPReset(j.ID); err == nil {
			canComplete, completionReason = true, ""
		} else {
			completionReason = err.Error()
		}
	}
	blocked := totpRecoveryBlocked(j) || completionPending || s.jobs != nil && s.jobs.TOTPExportPending(j.ID)
	if blocked {
		out["canDownload"], out["totpExportBlocked"] = false, true
		out["totpExportUnavailableReason"] = jobs.ErrTOTPRecoveryPending.Error()
		if !completionPending {
			setupReason, resetReason = "2FA 修改结果未确认，需人工处理；不能再次修改或恢复远端会话", "2FA 修改结果未确认，需人工处理；不能再次修改或恢复远端会话"
		}
		for _, key := range []string{"canRetry", "canResume", "canRegenerate", "canForceRelogin", "canAddPassword", "autoRepairEligible"} {
			out[key] = false
		}
		out["autoRepairEligibilityReason"] = resetReason
	}
	out["canCompleteTotp"], out["totpCompletionUnavailableReason"] = canComplete, completionReason
	out["totpCompletionPending"] = completionPending
	out["totpCompletionHint"] = "仅使用本次激活成功证据完成本地密钥保存及收尾；不连接远端、不登录、不修改 2FA"
	out["totpActivationVerified"] = j.TOTPActivationVerified
	out["totpResetCommitted"] = j.TOTPResetCommitted
	out["totpIntentId"] = j.TOTPIntentID
	out["totpKnownEnabled"] = j.TOTPKnownEnabled
	out["canSetupTotp"], out["canResetTotp"] = setupReason == "", resetReason == ""
	out["totpSetupUnavailableReason"], out["totpResetUnavailableReason"] = setupReason, resetReason
	out["resetTotp"], out["totpResetStatus"], out["totpResetPhase"] = j.ResetTOTP, j.TOTPResetStatus, j.TOTPResetPhase
	out["totpResetError"], out["totpRecoveryPending"], out["totpCredentialInvalidated"] = j.TOTPResetError, j.TOTPRecoveryPending, j.TOTPCredentialInvalidated
	out["logoutAllDevicesAfterTotp"], out["logoutAllDevicesStatus"], out["logoutAllDevicesError"] = j.LogoutAllDevicesAfterTOTP, j.LogoutAllDevicesStatus, j.LogoutAllDevicesError
	out["reloginAfterTotp"], out["uploadAfterTotp"] = j.ReloginAfterTOTP, j.UploadAfterTOTP
	out["totpWorkflowStatus"], out["totpWorkflowStep"], out["totpWorkflowError"] = j.TOTPWorkflowStatus, j.TOTPWorkflowStep, j.TOTPWorkflowError
	out["totpReloginStatus"], out["totpUploadStatus"] = j.TOTPReloginStatus, j.TOTPUploadStatus
	return out
}
