package jobs

import (
	"regexp"

	"tosub2-go-port/internal/accountplan"
	"tosub2-go-port/internal/logging"
)

var eventIdentifier = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]{0,63}$`)

// SanitizeJobEvent must be applied at the beginning of emitLocked, before both
// state mutation and subscriber delivery. It does not mutate caller-owned Data.
func SanitizeJobEvent(event JobEvent) JobEvent {
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
	out.PlanType = accountplan.Normalize(out.PlanType)
	known := jobKnownSecrets(job)
	for _, field := range []*string{&out.Error, &out.LastError, &out.OperationError, &out.Prompt, &out.MailAPIError, &out.PhoneError, &out.TOTPSetupError, &out.TOTPResetError, &out.LogoutAllDevicesError, &out.TOTPWorkflowError, &out.ReloginUploadError, &out.PasswordAddError, &out.SMSError, &out.AutoRepairBlockedReason, &out.AutoRepairLastError, &out.LastAuthAutomationReason} {
		*field = logging.RedactKnown(*field, known...)
	}
	for i, line := range out.Logs {
		out.Logs[i] = logging.RedactKnown(line, known...)
	}
	for i := range out.LogEntries {
		out.LogEntries[i].Message = logging.RedactKnown(out.LogEntries[i].Message, known...)
	}
	out.SMSServiceLabel = SanitizeSMSServiceLabel(out.SMSServiceLabel, known...)
	out.Input = nil
	out.MailAPIURL = ""
	out.TOTPSetupSecret = ""
	out.TOTPSetupURI = ""
	return out
}

func jobKnownSecrets(job *Job) []string {
	known := []string{job.TOTPSetupSecret, job.TOTPSetupURI}
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
