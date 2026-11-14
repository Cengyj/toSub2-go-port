package jobs

import (
	"errors"
	"strings"

	"tosub2-go-port/internal/logging"
)

// Keep the account-wide quarantine explicit while preserving why this attempt
// stopped. A generic recovery warning alone hides the phase that needs review.
// This text is used by both public operation state and the retained final log.
func totpRecoveryDiagnostic(job *Job, failure error) string {
	prefix := ErrTOTPRecoveryPending.Error()
	failureReason := ""
	if failure != nil {
		failureReason = failure.Error()
	}
	for _, candidate := range []string{job.TOTPResetError, failureReason} {
		reason := strings.TrimSpace(logging.RedactKnown(candidate, jobKnownSecrets(job)...))
		if reason == "" || strings.Contains(reason, prefix) || reason == "2FA 重置未完整完成，请手动核实并恢复有效密钥" {
			continue
		}
		return safeError(errors.New(prefix + "；具体原因：" + reason))
	}
	return prefix
}
