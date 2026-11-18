package jobs

import (
	"regexp"

	"tosub2-go-port/internal/accountplan"
	"tosub2-go-port/internal/logging"
	"tosub2-go-port/internal/resetlog"
)

var eventIdentifier = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]{0,63}$`)

// SanitizeJobEvent must be applied at the beginning of emitLocked, before both
// state mutation and subscriber delivery. It does not mutate caller-owned Data.
func SanitizeJobEvent(event JobEvent) JobEvent {
	if raw, present := event.Data[resetlog.DataKey]; present {
		// A diagnostic has one typed payload. Neither its prompt nor arbitrary
		// sibling keys are public, even when the payload itself is malformed.
		event.Stage, event.Status, event.Prompt, event.Data = "", "", "", nil
		if d, ok := raw.(*resetlog.Detail); ok && d != nil {
			if safe := d.Safe(); safe != nil {
				event.Stage, event.Prompt = "totp_reset_diagnostic", safe.Message()
				event.Data = map[string]any{resetlog.DataKey: safe}
			}
		}
		return event
	}
	event.Prompt = logging.RedactText(event.Prompt)
	if event.Stage != "" && !eventIdentifier.MatchString(event.Stage) {
		event.Stage = "event"
	}
	if event.Status != "" && !eventIdentifier.MatchString(event.Status) {
		event.Status = "event"
	}
	if event.Data != nil {
		// Structured metadata is an explicit public contract, not a passthrough
		// for arbitrary provider responses or authentication material.
		data := make(map[string]any)
		for _, key := range []string{"error", "phoneError", "mailApiError", "networkRetry"} {
			if value, ok := event.Data[key].(string); ok {
				safe := []rune(logging.RedactText(value))
				if len(safe) > 512 {
					safe = safe[:512]
				}
				data[key] = string(safe)
			}
		}
		if phone, ok := event.Data["currentPhone"].(string); ok {
			data["currentPhone"] = logging.RedactText(phone)
		}
		if required, ok := event.Data["securityCheckRequired"].(bool); ok {
			data["securityCheckRequired"] = required
		}
		if required, ok := event.Data["phoneRetryRequired"].(bool); ok {
			data["phoneRetryRequired"] = required
		}
		if status, ok := event.Data["mailStatus"].(string); ok {
			switch status {
			case "ready", "polling", "found", "error", "timeout":
				data["mailStatus"] = status
			}
		}
		event.Data = data
	}
	return event
}

// SanitizeJobForPublic makes a sanitized clone for metadata/API boundaries.
// The live manager job retains private Input for protocol work.
func SanitizeJobForPublic(job *Job) *Job {
	if job == nil {
		return nil
	}
	out := cloneJob(job)
	out.AccountRequestObservations = sanitizeAccountRequestObservations(out.AccountRequestObservations)
	for i := range out.AccountRequestObservations {
		out.AccountRequestObservations[i].Historical = out.AccountRequestObservations[i].StartedAt.Before(job.LastOperationAt)
	}
	out.PlanType = accountplan.Normalize(out.PlanType)
	if out.UploadTarget != UploadTargetSub2API && out.UploadTarget != UploadTargetCPA {
		out.UploadTarget = ""
	}
	if out.AutoRepairQueuedPlanType != "" {
		out.AutoRepairQueuedPlanType = accountplan.Normalize(out.AutoRepairQueuedPlanType)
	}
	out.AutoRepairSkippedReason = logging.RedactKnown(out.AutoRepairSkippedReason, jobKnownSecrets(job)...)
	out.AutoRepairQueuedRemotePlanTypes = normalizedRepairPlans(out.AutoRepairQueuedRemotePlanTypes)
	out.ManualExpiresAt, _ = NormalizeExpiry(out.ManualExpiresAt)
	out.SubscriptionExpiresAt, _ = NormalizeExpiry(out.SubscriptionExpiresAt)
	if !subscriptionPlanSource(out.SubscriptionPlanSource) || out.SubscriptionPlanCheckedAt == nil || out.SubscriptionPlanCheckedAt.IsZero() {
		out.SubscriptionPlanSource, out.SubscriptionPlanCheckedAt = "", nil
	}
	out.subscriptionPlanDigest, out.subscriptionPlanAccountID = "", ""
	known := jobKnownSecrets(job)
	for _, field := range []*string{&out.Error, &out.LastError, &out.OperationError, &out.Prompt, &out.MailAPIError, &out.PhoneError, &out.TOTPSetupError, &out.TOTPResetError, &out.LogoutAllDevicesError, &out.TOTPWorkflowError, &out.ReloginUploadError, &out.PasswordAddError, &out.SMSError, &out.AutoRepairBlockedReason, &out.AutoRepairLastError, &out.LastAuthAutomationReason} {
		*field = logging.RedactKnown(*field, known...)
	}
	for i, line := range out.Logs {
		out.Logs[i] = logging.RedactKnown(line, known...)
	}
	for i := range out.LogEntries {
		if d := out.LogEntries[i].Diagnostic; d != nil {
			out.LogEntries[i].Diagnostic = d.Safe()
		}
		out.LogEntries[i].Message = logging.RedactKnown(out.LogEntries[i].Message, known...)
	}
	out.SMSServiceLabel = SanitizeSMSServiceLabel(out.SMSServiceLabel, known...)
	out.TOTPIntentReceipts = nil
	out.Input = nil
	out.MailAPIURL = ""
	out.TOTPSetupSecret = ""
	out.TOTPSetupURI = ""
	return out
}

func jobKnownSecrets(job *Job) []string {
	known := []string{job.TOTPSetupSecret, job.TOTPSetupURI, job.Email}
	var collect func(map[string]any, int)
	collect = func(values map[string]any, depth int) {
		if depth > 12 {
			return
		}
		for key, value := range values {
			if logging.SensitiveKey(key) {
				if secret, ok := value.(string); ok && secret != "" {
					known = append(known, secret)
				}
			}
			switch nested := value.(type) {
			case map[string]any:
				collect(nested, depth+1)
			case map[string]string:
				for _, secret := range nested {
					if secret != "" && logging.SensitiveKey(key) {
						known = append(known, secret)
					}
				}
			}
		}
	}
	collect(job.Input, 0)
	return known
}

func sanitizeEventForJob(event JobEvent, job *Job) JobEvent {
	known := jobKnownSecrets(job)
	event.Prompt = logging.RedactKnown(event.Prompt, known...)
	for _, key := range []string{"error", "phoneError", "mailApiError", "networkRetry"} {
		if value, ok := event.Data[key].(string); ok {
			event.Data[key] = logging.RedactKnown(value, known...)
		}
	}
	return event
}

// SafeJobError is a central helper for all externally sourced error text.
// Replace the manager's old key=value-only safeError with this function.
func SafeJobError(err error) string { return logging.SafeError(err) }
