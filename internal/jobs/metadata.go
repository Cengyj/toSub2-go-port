package jobs

import (
	"context"
	"time"
)

// MetadataJSONLimit includes active account history and retained trash. The
// general settings limit is too small for 1,000 accounts with 200 log lines.
// Keep both restoration and synchronous commit bounded by the same budget;
// excess data fails before replacement without trimming business evidence.
const MetadataJSONLimit int64 = 256 << 20

func (m *Manager) persistLocked() {
	if m.store == nil {
		m.lastPersistError = nil
		return
	}
	projection := m.metadataProjection.begin(len(m.jobs) + len(m.trash))
	data := make([]map[string]any, 0, len(m.jobs))
	for _, j := range m.jobs {
		data = append(data, projection.job(j))
	}
	trash := make([]map[string]any, 0, len(m.trash))
	for _, entry := range m.trash {
		trash = append(trash, projection.trash(entry))
	}
	projection.previous = nil
	m.metadataProjection = projection
	// Write while holding the manager lock so a response cannot race a stale
	// asynchronous write and overwrite newer metadata during shutdown. The
	// store itself performs temp-file+fsync+rename atomically.
	write := func(ctx context.Context, name string, value any) error {
		return m.store.WriteJSONWithLimit(ctx, name, value, MetadataJSONLimit)
	}
	if m.metadataWriter != nil {
		write = m.metadataWriter
	}
	m.lastPersistError = write(context.Background(), "job-meta.json", map[string]any{"version": 2, "jobs": data, "trash": trash, "updated_at": time.Now().UTC()})
	if m.lastPersistError != nil && m.logger != nil {
		m.logger.Error("job metadata save failed", "error", safeError(m.lastPersistError))
	}
}

func jobMetadata(j *Job) map[string]any {
	return jobMetadataWithLogProjection(j, nil)
}

func jobMetadataWithLogProjection(j *Job, projectLog func(string) string) map[string]any {
	if j == nil {
		return map[string]any{}
	}
	subscriptionDigest, subscriptionAccountID := j.subscriptionPlanDigest, j.subscriptionPlanAccountID
	receipts, operationID, oauthResultID := j.TOTPIntentReceipts, j.TOTPResetOperationID, j.OAuthResultID
	j = sanitizeJobForPublic(j, projectLog)

	m := map[string]any{"id": j.ID, "operation": j.Operation, "last_operation_type": j.Operation, "queued_at": j.QueuedAt, "logs": j.Logs, "mail_api_error": j.MailAPIError, "totp_setup_error": j.TOTPSetupError, "password_add_error": j.PasswordAddError, "last_auth_automated": j.LastAuthAutomated, "last_auth_automation_reason": j.LastAuthAutomationReason, "last_auth_automated_at": j.LastAuthAutomatedAt, "last_auth_requirements": j.LastAuthRequirements, "email": j.Email, "status": j.Status, "stage": j.Stage, "prompt": j.Prompt, "last_error": j.Error, "result_saved": j.ResultSaved, "created_at": j.CreatedAt, "updated_at": j.UpdatedAt, "attempt": j.Attempts, "login_mode": j.LoginMode, "mail_status": j.MailStatus, "proxy_configured": j.ProxyConfigured, "has_password": j.HasPassword, "has_totp_key": j.HasTOTPKey, "totp_known_enabled": j.TOTPKnownEnabled, "current_phone": j.CurrentPhone, "phone_error": j.PhoneError, "phone_retry_required": j.PhoneRetryRequired, "sms_provider_id": j.SMSProviderID, "sms_provider_name": j.SMSProviderName, "sms_service_label": j.SMSServiceLabel, "sms_status": j.SMSStatus, "sms_error": j.SMSError, "security_check_required": j.SecurityCheckRequired, "auto_repair_blocked": j.AutoRepairBlocked, "auto_repair_blocked_reason": j.AutoRepairBlockedReason, "auto_repair_pending_account_ids": j.AutoRepairPendingAccountIDs, "auto_repair_backend": j.AutoRepairBackend, "auto_repair_last_error": j.AutoRepairLastError}
	m["totp_intent_receipts"] = receipts
	m["password_add_outcome"] = j.PasswordAddOutcome
	m["account_request_observations"] = j.AccountRequestObservations
	m["totp_reset_operation_id"] = operationID
	m["oauth_result_id"] = oauthResultID
	m["totp_intent_id"] = j.TOTPIntentID
	m["totp_reset_committed"] = j.TOTPResetCommitted
	m["totp_activation_verified"] = j.TOTPActivationVerified
	m["plan_type"] = j.PlanType
	if j.SubscriptionPlanCheckedAt != nil {
		m["subscription_plan_checked_at"] = j.SubscriptionPlanCheckedAt
	}
	if j.SubscriptionPlanSource != "" {
		m["subscription_plan_source"] = j.SubscriptionPlanSource
	}
	m["subscription_plan_stale"] = j.SubscriptionPlanStale
	if subscriptionDigestValid(subscriptionDigest) && subscriptionAccountID != "" {
		m["subscription_plan_binding"] = map[string]string{"authorizationDigest": subscriptionDigest, "accountId": subscriptionAccountID}
	}
	m["auto_repair_sync_paused"], m["recovery_paused"] = j.AutoRepairSyncPaused, j.RecoveryPaused
	m["auto_repair_skipped_reason"] = j.AutoRepairSkippedReason
	m["auto_repair_queued_plan_type"] = j.AutoRepairQueuedPlanType
	m["auto_repair_queued_remote_plan_types"] = j.AutoRepairQueuedRemotePlanTypes
	if j.ManualExpiresAt != "" {
		m["manual_expires_at"] = j.ManualExpiresAt
	}
	if j.SubscriptionExpiresAt != "" {
		m["subscription_expires_at"] = j.SubscriptionExpiresAt
	}
	m["authorization_method"] = j.AuthorizationMethod
	m["operation_status"], m["operation_error"], m["log_entries"], m["credential_warning"] = j.OperationStatus, j.OperationError, j.LogEntries, j.LastError
	if !j.LastOperationAt.IsZero() {
		m["last_operation_at"] = j.LastOperationAt
	}
	if j.QueuedAt.IsZero() {
		m["queued_at"] = nil
	}
	if j.SecurityActionReturnStatus != "" {
		m["security_action_return_status"] = j.SecurityActionReturnStatus
	}
	for key, value := range map[string]any{
		"reset_totp": j.ResetTOTP, "totp_reset_status": j.TOTPResetStatus, "totp_reset_phase": j.TOTPResetPhase,
		"totp_reset_error": j.TOTPResetError, "totp_recovery_pending": j.TOTPRecoveryPending,
		"totp_credential_invalidated":   j.TOTPCredentialInvalidated,
		"logout_all_devices_after_totp": j.LogoutAllDevicesAfterTOTP, "logout_all_devices_status": j.LogoutAllDevicesStatus,
		"logout_all_devices_error": j.LogoutAllDevicesError,
		"relogin_after_totp":       j.ReloginAfterTOTP, "upload_after_totp": j.UploadAfterTOTP,
		"totp_workflow_status": j.TOTPWorkflowStatus, "totp_workflow_step": j.TOTPWorkflowStep,
		"totp_workflow_error": j.TOTPWorkflowError, "totp_relogin_status": j.TOTPReloginStatus,
		"totp_upload_status":   j.TOTPUploadStatus,
		"upload_target":        j.UploadTarget,
		"upload_after_relogin": j.UploadAfterRelogin, "relogin_authorization_saved": j.ReloginAuthorizationSaved,
		"relogin_upload_status": j.ReloginUploadStatus, "relogin_upload_error": j.ReloginUploadError,
	} {
		m[key] = value
	}
	if j.StartedAt != nil {
		m["started_at"] = j.StartedAt
	}
	if j.FinishedAt != nil {
		m["finished_at"] = j.FinishedAt
	}
	if j.PasswordAddedAt != nil {
		m["password_added_at"] = j.PasswordAddedAt
	}
	if j.LastOperationType != "" {
		m["last_operation_type"] = j.LastOperationType
	}
	if j.AutoRepairLastAttemptAt != nil {
		m["auto_repair_last_attempt_at"] = j.AutoRepairLastAttemptAt
	}
	if j.AutoRepairLastSuccessAt != nil {
		m["auto_repair_last_success_at"] = j.AutoRepairLastSuccessAt
	}
	return m
}
