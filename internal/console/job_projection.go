package console

import (
	"fmt"

	"tosub2-go-port/internal/accountplan"
	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/sub2api"
)

func (s *Server) publicJob(j *jobs.Job) map[string]any {
	if j == nil {
		return nil
	}
	autoEmailOTP := j.MailAPIURL != ""
	password, _ := j.Input["password"].(string)
	totp, _ := j.Input["totpSecret"].(string)
	j = jobs.SanitizeJobForPublic(j)
	autoRepairEligible, autoRepairReason := repairEligibility(j, password != "", autoEmailOTP, totp != "")
	planExcluded := false
	if j.UploadTarget == jobs.UploadTargetCPA {
		s.cpaMu.Lock()
		planExcluded = accountplan.Excluded(j.PlanType, s.cpa.ExcludedPlanTypes)
		s.cpaMu.Unlock()
	} else if s.monitor != nil {
		planExcluded = sub2api.PlanExcluded(j.PlanType, s.monitor.Configuration().ExcludedPlanTypes)
	}
	if planExcluded {
		autoRepairEligible, autoRepairReason = false, "当前套餐已列入自动重登排除范围；仍可手动重新授权或同步"
	}
	if j.AutoRepairSyncPaused || j.RecoveryPaused {
		autoRepairEligible, autoRepairReason = false, "上次自动上传或授权恢复尚待核实，自动重登已暂停"
	}
	canRetry := j.Status == jobs.StatusImported || j.Status == jobs.StatusFailed || j.Status == jobs.StatusCanceled || j.Status == "reauth_required" || j.Status == "resume_available"
	completed := j.Status == jobs.StatusSucceeded
	position := s.jobs.QueuePosition(j.ID)
	if j.Status == jobs.StatusQueued {
		position = max(position, 1)
		j.Prompt = fmt.Sprintf("排队中，前方还有 %d 条任务", position-1)
	}
	if j.LoginMode == "" {
		j.LoginMode = "email_otp"
	}
	operationAt := j.LastOperationAt
	if operationAt.IsZero() {
		operationAt = j.UpdatedAt
	}
	// Separate the latest action outcome from reusable authorization state.
	// For example, a canceled 2FA setup may leave an old output downloadable.
	p := s.publicTOTPState(j, map[string]any{"autoRepairEligible": autoRepairEligible, "autoRepairEligibilityReason": autoRepairReason, "queuePosition": position, "completedAt": j.FinishedAt, "restartRequired": j.Status == "reauth_required", "id": j.ID, "email": j.Email, "status": j.Status, "prompt": j.Prompt, "createdAt": j.CreatedAt, "updatedAt": j.UpdatedAt, "lastOperationAt": operationAt, "lastOperationType": publicOperationType(j), "lastError": firstNonEmpty(j.LastError, j.Error), "canDownload": j.ResultSaved, "planType": j.PlanType, "loginMode": j.LoginMode, "hasTotpKey": j.HasTOTPKey, "autoEmailOtp": autoEmailOTP, "mailStatus": j.MailStatus, "mailApiError": j.MailAPIError, "currentPhone": j.CurrentPhone, "phoneError": j.PhoneError, "phoneRetryRequired": j.PhoneRetryRequired, "totpSetupError": j.TOTPSetupError, "passwordAddError": j.PasswordAddError, "passwordAddedAt": j.PasswordAddedAt, "smsProviderId": j.SMSProviderID, "smsProviderName": j.SMSProviderName, "smsServiceLabel": j.SMSServiceLabel, "smsStatus": j.SMSStatus, "smsError": j.SMSError, "securityCheckRequired": j.SecurityCheckRequired, "canRetry": canRetry, "canResume": j.Status == "resume_available", "canRegenerate": (completed || (canRetry && j.Stage == "refresh_output_pending")) && j.ResultSaved, "canForceRelogin": completed || canRetry, "canSetupTotp": !j.HasTOTPKey && !j.TOTPKnownEnabled && s.canSecurityOperation(j), "canAddPassword": !j.HasPassword && s.canSecurityOperation(j), "proxyConfigured": j.ProxyConfigured, "autoRepairBlocked": j.AutoRepairBlocked, "autoRepairBlockedReason": j.AutoRepairBlockedReason, "autoRepairPendingAccountIds": j.AutoRepairPendingAccountIDs, "autoRepairLastAttemptAt": j.AutoRepairLastAttemptAt, "autoRepairLastSuccessAt": j.AutoRepairLastSuccessAt, "autoRepairLastError": j.AutoRepairLastError, "attempt": j.Attempts})
	p["stage"], p["startedAt"], p["operationStatus"] = j.Stage, j.StartedAt, j.OperationStatus
	p["accountRequestObservations"] = j.AccountRequestObservations
	p["authorizationMethod"], _ = jobs.NormalizeAuthorizationMethod(j.AuthorizationMethod)
	p["autoRepairSkippedReason"] = j.AutoRepairSkippedReason
	p["autoRepairSyncPaused"], p["recoveryPaused"] = j.AutoRepairSyncPaused, j.RecoveryPaused
	p["autoRepairQueuedPlanType"] = j.AutoRepairQueuedPlanType
	p["autoRepairQueuedRemotePlanTypes"] = j.AutoRepairQueuedRemotePlanTypes
	p["operationError"], p["credentialWarning"] = firstNonEmpty(j.OperationError, j.Error), j.LastError
	p["manualExpiresAt"], p["subscriptionExpiresAt"] = j.ManualExpiresAt, j.SubscriptionExpiresAt
	s.addSubscriptionRefreshState(p, j)
	p["expiresAt"], p["expiresAtSource"] = jobs.EffectiveExpiry(j)
	return publicReloginUpload(j, p)
}
func publicOperationType(j *jobs.Job) string {
	op := j.Operation
	if j.LastOperationType != "" {
		op = j.LastOperationType
	}
	switch op {
	case "regenerate", "refresh", "reauthorize":
		return "reauthorize"
	case "reset-2fa", "reset_2fa":
		return "reset_2fa"
	case "complete-2fa", "complete_2fa":
		return "complete_2fa"
	case "setup-2fa", "setup_2fa", "setup-totp", "totp_setup":
		return "setup_2fa"
	case "add-password", "add_password", "password_add":
		return "add_password"
	case "relogin":
		if len(j.AutoRepairPendingAccountIDs) > 0 {
			return "automatic_relogin"
		}
		return "relogin"
	case "", "full":
		if j.Attempts > 1 {
			return "reauthorize"
		}
		return "initial_authorization"
	default:
		return op
	}
}

func (s *Server) publicSelectionJob(j *jobs.Job) map[string]any {
	p := s.publicJob(j)
	for _, k := range []string{"prompt", "updatedAt", "lastError", "logs"} {
		delete(p, k)
	}
	return p
}
func (s *Server) canSecurityOperation(j *jobs.Job) bool {
	if totpRecoveryBlocked(j) {
		return false
	}
	if j.Status == jobs.StatusImported {
		return true
	}
	if j.Status == jobs.StatusSucceeded && j.ResultSaved {
		return true
	}
	if j.Status == jobs.StatusQueued {
		return false
	}
	switch j.Status {
	case "phone", "phone_otp", "sms_otp", "resume_available", "reauth_required", jobs.StatusFailed, jobs.StatusCanceled:
		// Ordinary failed logins can begin a new security operation without a
		// historical session file. Incomplete security workflows retain their
		// separate evidence and admission gates.
		if j.ResetTOTP || j.TOTPWorkflowStatus != "" {
			return false
		}
		switch j.Operation {
		case "", "login", "full", "relogin":
			return true
		}
		return false
	}
	return false
}
