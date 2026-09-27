package jobs

import (
	"regexp"

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
		for _, key := range []string{"error", "phoneError", "mailApiError"} {
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
	known := []string{}
	for key, value := range job.Input {
		if logging.SensitiveKey(key) {
			if s, ok := value.(string); ok && s != "" {
				known = append(known, s)
			}
		}
	}
	known = append(known, job.TOTPSetupSecret, job.TOTPSetupURI)
	for _, field := range []*string{&out.Error, &out.LastError, &out.Prompt, &out.MailAPIError, &out.PhoneError, &out.TOTPSetupError, &out.PasswordAddError, &out.SMSError, &out.AutoRepairBlockedReason, &out.AutoRepairLastError, &out.LastAuthAutomationReason} {
		*field = logging.RedactKnown(*field, known...)
	}
	for i, line := range out.Logs {
		out.Logs[i] = logging.RedactKnown(line, known...)
	}
	out.SMSServiceLabel = SanitizeSMSServiceLabel(out.SMSServiceLabel, known...)
	out.Input = nil
	out.MailAPIURL = ""
	out.TOTPSetupSecret = ""
	out.TOTPSetupURI = ""
	return out
}

// SafeJobError is a central helper for all externally sourced error text.
// Replace the manager's old key=value-only safeError with this function.
func SafeJobError(err error) string { return logging.SafeError(err) }
