package jobs

import (
	"fmt"
	"strings"
	"time"

	"tosub2-go-port/internal/logging"
)

// LogEntry identifies an event's action separately from reusable account state.
type LogEntry struct {
	At              time.Time `json:"at"`
	OperationAt     time.Time `json:"operationAt,omitempty"`
	Operation       string    `json:"operation,omitempty"`
	Stage           string    `json:"stage,omitempty"`
	Status          string    `json:"status,omitempty"`
	OperationStatus string    `json:"operationStatus,omitempty"`
	Level           string    `json:"level"`
	Message         string    `json:"message"`
}

func beginOperationState(j *Job) {
	j.OperationStatus, j.OperationError = "queued", ""
	j.Error, j.LastError = "", ""
	j.Stage, j.Prompt = "queued", "等待执行"
	j.StartedAt, j.FinishedAt = nil, nil
	// These describe an attempt, rather than durable account configuration.
	j.MailStatus, j.MailAPIError, j.SMSError, j.SMSStatus = "", "", "", ""
	j.TOTPSetupError, j.PasswordAddError = "", ""
	j.TOTPSetupSecret, j.TOTPSetupURI = "", ""
}

func operationStatusForEvent(status string) string {
	switch status {
	case StatusQueued:
		return "queued"
	case "awaiting_input", StatusWaitingInput, "email_otp", "password", "mfa_otp", "totp", "phone", "phone_otp", "sms_otp", "totp_setup_otp":
		return "waiting"
	case "succeeded", StatusSucceeded:
		return "succeeded"
	case StatusFailed, StatusCanceled, "interrupted":
		return status
	}
	if isTransientStatus(status) {
		return "running"
	}
	return ""
}

func restoreOperationOutcome(j *Job, savedStatus, savedOperationStatus string, recovered privateResultRecovery) {
	if j.Status == StatusQueued {
		j.OperationStatus, j.OperationError = "queued", ""
		return
	}
	if j.TOTPRecoveryPending || j.TOTPCredentialInvalidated {
		j.OperationStatus, j.OperationError = "failed", j.Error
		return
	}
	// Canceling a queued or active workflow intentionally interrupts its steps.
	// Preserve that user outcome instead of relabeling it as a server interruption.
	if savedOperationStatus == "canceled" {
		j.OperationStatus, j.OperationError = "canceled", ""
		return
	}
	if j.Operation == "relogin" && j.UploadAfterRelogin && j.ReloginAuthorizationSaved && j.ResultSaved && (j.ReloginUploadStatus == "succeeded" || j.ReloginUploadStatus == "failed" || j.ReloginUploadStatus == "unknown" || j.ReloginUploadStatus == "skipped") {
		// The durable new grant confirms authorization independently of a
		// server interruption while optional synchronization was in progress.
		j.Status, j.OperationStatus, j.OperationError, j.Error = StatusSucceeded, "succeeded", "", ""
		j.Stage, j.Prompt = "complete", operationCompletionPrompt(j)
		return
	}
	if j.TOTPWorkflowStatus == "failed" || j.TOTPWorkflowStatus == "interrupted" {
		j.OperationStatus, j.OperationError = j.TOTPWorkflowStatus, j.TOTPWorkflowError
		return
	}
	if recovered.password || recovered.totp {
		j.OperationStatus, j.OperationError = "succeeded", ""
		return
	}
	if savedOperationStatus == "queued" || savedOperationStatus == "running" || savedOperationStatus == "waiting" || (savedOperationStatus == "" && isTransientStatus(savedStatus)) {
		j.OperationStatus, j.OperationError = "interrupted", "服务重启中断了上次操作"
		return
	}
	if savedOperationStatus != "" {
		return
	}
	if j.Error != "" {
		j.OperationStatus, j.OperationError = "failed", j.Error
	} else if isSuboperation(j.Operation) && (j.TOTPSetupError != "" || j.PasswordAddError != "") {
		j.OperationStatus = "failed"
		j.OperationError = j.TOTPSetupError
		if j.OperationError == "" {
			j.OperationError = j.PasswordAddError
		}
	} else {
		j.OperationStatus = operationStatusForEvent(j.Status)
	}
}

func firstOperationErrorForLog(j *Job) string {
	if j.OperationError != "" {
		return j.OperationError
	}
	return j.Error
}

func operationCompletionPrompt(j *Job) string {
	if totpOperation(j.Operation) {
		if j.UploadAfterTOTP && j.TOTPUploadStatus == "succeeded" {
			return "2FA、重新授权和 Sub2API 同步已完成"
		}
		if j.ReloginAfterTOTP && j.TOTPReloginStatus == "succeeded" {
			return "2FA 与重新授权流程已完成"
		}
		if j.ResetTOTP {
			if j.Status == "reauth_required" {
				return "2FA 已重置，新密钥已安全保存；原登录会话已失效，请重新登录获取授权"
			}
			return "2FA 已重置，新密钥已安全保存"
		}
		return "2FA 设置已完成"
	}
	switch j.Operation {
	case "add-password", "add_password", "password_add":
		return "账号密码已添加"
	case "relogin":
		if j.UploadAfterRelogin {
			switch j.ReloginUploadStatus {
			case "succeeded":
				return "重新登录授权与 Sub2API 同步已完成"
			case "failed":
				return "新授权已保存，Sub2API 同步失败"
			case "unknown":
				return "新授权已保存，Sub2API 同步结果待核实"
			case "skipped":
				return "新授权已保存，Sub2API 同步未执行"
			}
		}
		return "重新登录授权已完成"
	case "regenerate", "refresh", "reauthorize":
		return "授权已刷新，结果已保存"
	}
	if j.ResultSaved {
		return "授权已完成，结果已保存"
	}
	return "操作已完成"
}

func (m *Manager) appendOperationLogLocked(j *Job, e JobEvent) {
	if e.Stage == "" && e.Status == "" && len(e.Data) == 0 {
		return
	}
	if e.At.IsZero() {
		e.At = time.Now().UTC()
	}
	if e.Stage == "" && e.Status == "" {
		if retry, _ := e.Data["networkRetry"].(string); retry != "" {
			// Log metadata never changes the active input, prompt or operation.
			e.Stage, e.Status, e.Prompt = "network_retry", "retrying", retry
		} else if mailStatus, _ := e.Data["mailStatus"].(string); mailStatus != "" {
			e.Stage, e.Status = "mail_status", mailStatus
			e.Prompt = map[string]string{"ready": "邮箱自动收码已就绪", "polling": "正在自动读取邮箱验证码", "found": "已读取邮箱验证码", "error": "邮箱自动收码发生错误", "timeout": "邮箱自动收码等待超时"}[mailStatus]
		} else {
			mailError, _ := e.Data["mailApiError"].(string)
			phoneError, _ := e.Data["phoneError"].(string)
			eventError, _ := e.Data["error"].(string)
			if mailError == "" && phoneError == "" && eventError == "" {
				return
			}
		}
	}
	message, level := e.Prompt, "info"
	if j.OperationStatus == "canceled" || j.OperationStatus == "interrupted" {
		level = "warning"
		if message == "" {
			message = "操作已取消"
			if j.OperationStatus == "interrupted" {
				message = "操作已中断"
			}
		}
	} else if failure, _ := e.Data["error"].(string); failure != "" {
		message, level = failure, "error"
	} else if e.Status == StatusFailed {
		level = "error"
	} else if e.Status == StatusCanceled || e.Status == "interrupted" || strings.HasSuffix(e.Stage, "_rejected") {
		level = "warning"
	}
	if level == "info" {
		for _, key := range []string{"phoneError", "mailApiError"} {
			if failure, _ := e.Data[key].(string); failure != "" {
				message, level = failure, "warning"
				break
			}
		}
	}
	if message == "" {
		message = strings.TrimSpace(strings.Join([]string{e.Stage, e.Status}, " · "))
	}
	message = logging.RedactKnown(message, jobKnownSecrets(j)...)
	runes := []rune(message)
	if len(runes) > 512 {
		message = string(runes[:512])
	}
	entry := LogEntry{At: e.At, OperationAt: j.LastOperationAt, Operation: j.LastOperationType, Stage: e.Stage, Status: e.Status, OperationStatus: j.OperationStatus, Level: level, Message: message}
	if entry.Operation == "" {
		entry.Operation = j.Operation
		if entry.Operation == "" || entry.Operation == "full" {
			entry.Operation = "initial_authorization"
		}
	}
	if count := len(j.LogEntries); count > 0 {
		previous := j.LogEntries[count-1]
		if previous.OperationAt.Equal(entry.OperationAt) && previous.Operation == entry.Operation && previous.Stage == entry.Stage && previous.Status == entry.Status && previous.OperationStatus == entry.OperationStatus && previous.Message == entry.Message && previous.Level == entry.Level {
			return
		}
	}
	j.LogEntries = append(j.LogEntries, entry)
	j.Logs = append(j.Logs, fmt.Sprintf("%s [%s] %s", e.At.Format(time.RFC3339), level, message))
	if len(j.LogEntries) > 200 {
		j.LogEntries = append([]LogEntry(nil), j.LogEntries[len(j.LogEntries)-200:]...)
	}
	if len(j.Logs) > 200 {
		j.Logs = append([]string(nil), j.Logs[len(j.Logs)-200:]...)
	}
}
