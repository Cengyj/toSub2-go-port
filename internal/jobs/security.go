package jobs

import (
	"net/url"
	"regexp"
	"slices"
	"strings"

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
		if profile, ok := event.Data["profileCompletion"].(map[string]any); ok && profile["trigger"] == "about_you" {
			source, validSource := profile["source"].(string)
			outcome, validOutcome := profile["outcome"].(string)
			validSource = validSource && (source == "provided" || source == "generated_adult")
			switch outcome {
			case "required", "submitting", "not_sent", "rejected", "unknown", "succeeded":
			default:
				validOutcome = false
			}
			if validSource && validOutcome {
				data["profileCompletion"] = map[string]any{"trigger": "about_you", "source": source, "outcome": outcome}
			}
		}
		event.Data = data
	}
	return event
}

// SanitizeJobForPublic makes a sanitized clone for metadata/API boundaries.
// The live manager job retains private Input for protocol work.
func SanitizeJobForPublic(job *Job) *Job {
	return sanitizeJobForPublic(job, nil)
}

// projectLog is a metadata-only derived-text optimization. Callers must bind
// it to this job's exact identity and known-secret context; ordinary public
// projections always perform their own complete redaction.
func sanitizeJobForPublic(job *Job, projectLog func(string) string) *Job {
	if job == nil {
		return nil
	}
	known := jobKnownSecrets(job)
	return sanitizePublicJobSnapshot(clonePublicJob(job), known, projectLog)
}

// clonePublicJob removes private fields before copying the public state. The
// caller captures its known-secret context under the same ownership boundary.
func clonePublicJob(job *Job) *Job {
	// Public projections need the secret values only for redaction, never a
	// second mutable copy of the complete authentication/configuration input.
	source := *job
	source.Input, source.TOTPIntentReceipts = nil, nil
	source.recoveryCleanup = nil
	source.MailAPIURL, source.TOTPSetupSecret, source.TOTPSetupURI = "", "", ""
	return cloneJob(&source)
}

// sanitizePublicJobSnapshot only reads the detached snapshot and its captured
// secret values. It can run after the manager lock is released without reading
// a later credential edit or sharing a mutable observation/map/time pointer.
func sanitizePublicJobSnapshot(out *Job, known []string, projectLog func(string) string) *Job {
	if projectLog == nil {
		projectLog = func(text string) string { return logging.RedactKnown(text, known...) }
	}
	out.AccountRequestObservations = sanitizeAccountRequestObservations(out.AccountRequestObservations)
	for i := range out.AccountRequestObservations {
		out.AccountRequestObservations[i].Historical = out.AccountRequestObservations[i].StartedAt.Before(out.LastOperationAt)
	}
	out.PlanType = accountplan.Normalize(out.PlanType)
	if out.PasswordAddOutcome != PasswordOutcomeAdded && out.PasswordAddOutcome != PasswordOutcomeExisting {
		out.PasswordAddOutcome = ""
	}
	if out.UploadTarget != UploadTargetSub2API && out.UploadTarget != UploadTargetCPA {
		out.UploadTarget = ""
	}
	if out.AutoRepairQueuedPlanType != "" {
		out.AutoRepairQueuedPlanType = accountplan.Normalize(out.AutoRepairQueuedPlanType)
	}
	if out.AutoRepairSkippedReason != "" {
		out.AutoRepairSkippedReason = logging.RedactKnown(out.AutoRepairSkippedReason, known...)
	}
	out.AutoRepairQueuedRemotePlanTypes = normalizedRepairPlans(out.AutoRepairQueuedRemotePlanTypes)
	out.ManualExpiresAt, _ = NormalizeExpiry(out.ManualExpiresAt)
	out.SubscriptionExpiresAt, _ = NormalizeExpiry(out.SubscriptionExpiresAt)
	if !subscriptionPlanSource(out.SubscriptionPlanSource) || out.SubscriptionPlanCheckedAt == nil || out.SubscriptionPlanCheckedAt.IsZero() {
		out.SubscriptionPlanSource, out.SubscriptionPlanCheckedAt = "", nil
	}
	out.subscriptionPlanDigest, out.subscriptionPlanAccountID = "", ""
	for _, field := range []*string{&out.Error, &out.LastError, &out.OperationError, &out.Prompt, &out.MailAPIError, &out.PhoneError, &out.TOTPSetupError, &out.TOTPResetError, &out.LogoutAllDevicesError, &out.TOTPWorkflowError, &out.ReloginUploadError, &out.PasswordAddError, &out.SMSError, &out.AutoRepairBlockedReason, &out.AutoRepairLastError, &out.LastAuthAutomationReason} {
		// Empty text cannot contain a secret. Keep the complete protection
		// chain for every nonempty diagnostic, including ordinary plain text.
		if *field != "" {
			*field = logging.RedactKnown(*field, known...)
		}
	}
	for i, line := range out.Logs {
		out.Logs[i] = projectLog(line)
	}
	for i := range out.LogEntries {
		if d := out.LogEntries[i].Diagnostic; d != nil {
			out.LogEntries[i].Diagnostic = d.Safe()
		}
		out.LogEntries[i].Message = projectLog(out.LogEntries[i].Message)
	}
	if out.SMSServiceLabel != "" {
		out.SMSServiceLabel = SanitizeSMSServiceLabel(out.SMSServiceLabel, known...)
	}
	out.TOTPIntentReceipts = nil
	out.Input = nil
	out.MailAPIURL = ""
	out.TOTPSetupSecret = ""
	out.TOTPSetupURI = ""
	return out
}

func jobKnownSecrets(job *Job) []string {
	known := []string{job.TOTPSetupSecret, job.TOTPSetupURI, job.Email, job.MailAPIURL}
	known = credentialURLSecrets(known, job.MailAPIURL)
	var collect func(map[string]any, int)
	collect = func(values map[string]any, depth int) {
		if depth > 12 {
			return
		}
		for key, value := range values {
			if logging.SensitiveKey(key) {
				if secret, ok := value.(string); ok && secret != "" {
					known = append(known, secret)
					// URL credentials may be reported separately by a transport.
					// Only these defined configuration fields have URL semantics.
					if strings.EqualFold(key, "proxyUrl") || strings.EqualFold(key, "mailApiUrl") {
						known = credentialURLSecrets(known, secret)
					}
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
	// Map iteration must not decide how overlapping account secrets are
	// hidden. Replace a complete longer value before any substring, then
	// stabilize equal-length entries for public, event and metadata paths.
	slices.SortFunc(known, func(a, b string) int {
		if len(a) != len(b) {
			return len(b) - len(a)
		}
		if a < b {
			return -1
		}
		if a > b {
			return 1
		}
		return 0
	})
	known = slices.Compact(known)
	if len(known) > 0 && known[len(known)-1] == "" {
		known = known[:len(known)-1]
	}
	return known
}

// credentialURLSecrets preserves the complete URL elsewhere in the known set
// and also hides separately rendered passwords and sensitive query values.
// Parse only explicit credential URL fields; arbitrary text and ordinary query
// values must not silently acquire a different meaning at this boundary.
func credentialURLSecrets(known []string, raw string) []string {
	if raw == "" {
		return known
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return known
	}
	if password, present := u.User.Password(); present && password != "" {
		known = append(known, password)
		if _, encoded, ok := strings.Cut(u.User.String(), ":"); ok && encoded != password {
			known = append(known, encoded)
		}
		// Userinfo.String normalizes percent-escape spelling. A transport
		// may quote the original encoded password, so retain that exact
		// component from the already parsed authority as well.
		authority := raw
		if u.Scheme != "" {
			_, authority, _ = strings.Cut(raw, ":")
		}
		if strings.HasPrefix(authority, "//") {
			authority = authority[2:]
			if end := strings.IndexAny(authority, "/?#"); end >= 0 {
				authority = authority[:end]
			}
			if end := strings.LastIndexByte(authority, '@'); end >= 0 {
				if _, encoded, ok := strings.Cut(authority[:end], ":"); ok && encoded != "" {
					known = append(known, encoded)
				}
			}
		}
	}
	for _, pair := range strings.Split(u.RawQuery, "&") {
		rawKey, rawValue, present := strings.Cut(pair, "=")
		if !present || rawValue == "" {
			continue
		}
		key, err := url.QueryUnescape(rawKey)
		if err != nil || !logging.SensitiveKey(key) {
			continue
		}
		known = append(known, rawValue)
		if value, err := url.QueryUnescape(rawValue); err == nil && value != "" && value != rawValue {
			known = append(known, value)
		}
	}
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
