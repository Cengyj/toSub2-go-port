package jobs

import (
	"context"
	"errors"
	"time"
)

func totpOperation(op string) bool {
	return op == "reset-2fa" || op == "setup-2fa" || op == "setup_2fa" || op == "setup-totp" || op == "totp_setup"
}

func initializeTOTPWorkflow(j *Job, values map[string]any) {
	initializeReloginUpload(j, values)
	if !totpOperation(j.Operation) {
		if !j.UploadAfterRelogin {
			delete(j.Input, "sub2apiConfig")
		}
		j.ResetTOTP = false
		j.ReloginAfterTOTP, j.UploadAfterTOTP = false, false
		j.TOTPWorkflowStatus, j.TOTPWorkflowStep, j.TOTPWorkflowError = "", "", ""
		j.TOTPReloginStatus, j.TOTPUploadStatus = "", ""
		j.LogoutAllDevicesAfterTOTP = false
		j.LogoutAllDevicesStatus, j.LogoutAllDevicesError = "", ""
		return
	}
	j.ReloginAfterTOTP, _ = values["reloginAfterTotp"].(bool)
	j.UploadAfterTOTP, _ = values["uploadAfterTotp"].(bool)
	j.TOTPWorkflowStatus, j.TOTPWorkflowStep, j.TOTPWorkflowError = "pending", "totp", ""
	j.TOTPReloginStatus, j.TOTPUploadStatus = "not_requested", "not_requested"
	if j.ReloginAfterTOTP {
		j.TOTPReloginStatus = "pending"
	}
	if j.UploadAfterTOTP {
		j.TOTPUploadStatus = "pending"
	} else {
		delete(j.Input, "sub2apiConfig")
	}
}

func interruptTOTPWorkflow(j *Job, message string) {
	interruptReloginUpload(j, message)
	if j.TOTPWorkflowStatus != "running" && j.TOTPWorkflowStatus != "pending" {
		return
	}
	j.TOTPWorkflowStatus, j.TOTPWorkflowError = "interrupted", message
	if j.TOTPReloginStatus == "running" {
		j.TOTPReloginStatus = "unknown"
	} else if j.TOTPReloginStatus == "pending" {
		j.TOTPReloginStatus = "skipped"
	}
	if j.TOTPUploadStatus == "running" {
		j.TOTPUploadStatus = "unknown"
	} else if j.TOTPUploadStatus == "pending" {
		j.TOTPUploadStatus = "skipped"
	}
	if j.LogoutAllDevicesStatus == "pending" {
		j.LogoutAllDevicesStatus = "skipped"
	}
	delete(j.Input, "sub2apiConfig")
}

// StartTOTPWorkflowStep persists the stage before its external work begins.
// A restart can show that work as uncertain but never automatically replay it.
func (m *Manager) StartTOTPWorkflowStep(ctx context.Context, id, step string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	j, ok := m.jobs[id]
	if !ok {
		return osErrNotFound(id)
	}
	if !totpOperation(j.Operation) || (j.TOTPWorkflowStatus != "pending" && j.TOTPWorkflowStatus != "running") {
		return errors.New("2FA 流程已结束，需要重新选择操作")
	}
	switch step {
	case "totp":
		if j.TOTPWorkflowStatus != "pending" {
			return errors.New("2FA 设置已开始，不能重复执行")
		}
	case "logout":
		if j.TOTPWorkflowStatus != "running" || j.TOTPWorkflowStep != "totp" || !j.ResetTOTP || !j.LogoutAllDevicesAfterTOTP || j.LogoutAllDevicesStatus != "pending" || j.TOTPResetPhase != "credential_persisted" || j.TOTPRecoveryPending || j.TOTPCredentialInvalidated {
			return errors.New("退出所有设备需要先完成新 2FA 密钥的安全保存")
		}
		// Once logout can be submitted, the old grant cannot be presented as
		// usable, including when the remote outcome remains uncertain.
		j.ResultSaved = false
	case "relogin":
		if j.TOTPWorkflowStatus != "running" || (j.TOTPWorkflowStep != "totp" && j.TOTPWorkflowStep != "logout") || !j.ReloginAfterTOTP || j.TOTPReloginStatus != "pending" || !j.HasTOTPKey || (!j.ResetTOTP && j.TOTPSetupError != "") || j.TOTPRecoveryPending || j.TOTPCredentialInvalidated || (j.ResetTOTP && j.TOTPResetPhase != "credential_persisted") || (j.LogoutAllDevicesAfterTOTP && j.LogoutAllDevicesStatus != "succeeded") {
			return errors.New("重新授权需要先完成 2FA 设置和所选的退出操作")
		}
		j.TOTPReloginStatus = "running"
		j.ResultSaved = false
	case "upload":
		if j.TOTPWorkflowStatus != "running" || j.TOTPWorkflowStep != "relogin" || !j.UploadAfterTOTP || !j.ReloginAfterTOTP || j.TOTPReloginStatus != "succeeded" || j.TOTPUploadStatus != "pending" || !j.ResultSaved || j.TOTPRecoveryPending || j.TOTPCredentialInvalidated {
			return errors.New("自动上传需要先完成重新授权并保存新授权文件")
		}
		j.TOTPUploadStatus = "running"
	default:
		return errors.New("invalid 2FA workflow step")
	}
	j.TOTPWorkflowStatus, j.TOTPWorkflowStep, j.TOTPWorkflowError = "running", step, ""
	j.Stage, j.UpdatedAt = "totp_workflow_"+step, time.Now().UTC()
	prompts := map[string]string{"totp": "正在设置 2FA", "logout": "正在退出所有设备", "relogin": "正在重新登录并授权", "upload": "正在同步新授权到 Sub2API"}
	if j.ResetTOTP && step == "totp" {
		prompts["totp"] = "正在重置 2FA"
	}
	m.emitLocked(JobEvent{JobID: id, Stage: j.Stage, Status: StatusRunning, Prompt: prompts[step], At: j.UpdatedAt})
	m.persistLocked()
	if m.store == nil || m.lastPersistError != nil {
		return errors.New("2FA 流程阶段无法可靠保存，已停止后续操作")
	}
	return nil
}

func (m *Manager) CompleteTOTPWorkflowStep(ctx context.Context, id, step string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	j, ok := m.jobs[id]
	if !ok {
		return osErrNotFound(id)
	}
	if !totpOperation(j.Operation) || j.TOTPWorkflowStatus != "running" || j.TOTPWorkflowStep != step {
		return errors.New("2FA 流程步骤不可完成")
	}
	switch step {
	case "relogin":
		if j.TOTPReloginStatus != "running" || !j.ResultSaved {
			return errors.New("新的重新授权结果尚未保存")
		}
		j.TOTPReloginStatus = "succeeded"
	case "upload":
		if j.TOTPUploadStatus != "running" {
			return errors.New("自动上传尚未执行")
		}
		j.TOTPUploadStatus = "succeeded"
	default:
		return errors.New("invalid 2FA workflow completion step")
	}
	j.UpdatedAt = time.Now().UTC()
	m.appendOperationLogLocked(j, JobEvent{JobID: id, Stage: "totp_workflow_" + step, Status: "succeeded", Prompt: map[string]string{"relogin": "新授权已保存", "upload": "新授权已同步到 Sub2API"}[step], At: j.UpdatedAt})
	m.persistLocked()
	if m.lastPersistError != nil {
		return errors.New("2FA 流程结果无法可靠保存，已停止后续操作")
	}
	return nil
}

// FinishTOTPWorkflow records the outcome even when the operation context was
// canceled. The integration key is process-local and removed at this boundary.
func (m *Manager) FinishTOTPWorkflow(id string, operationErr error) error {
	interrupted := errors.Is(operationErr, context.Canceled) || errors.Is(operationErr, context.DeadlineExceeded)
	return m.finishTOTPWorkflow(id, operationErr, interrupted)
}

// FinishTOTPWorkflowContext distinguishes the job's cancellation or deadline
// from a shorter HTTP/provider deadline. A timed-out request can fail while
// its job context remains active; that is a failure, not an interruption.
func (m *Manager) FinishTOTPWorkflowContext(ctx context.Context, id string, operationErr error) error {
	interrupted := ctx != nil && ctx.Err() != nil
	if operationErr == nil && interrupted {
		operationErr = ctx.Err()
	}
	return m.finishTOTPWorkflow(id, operationErr, interrupted)
}

func (m *Manager) finishTOTPWorkflow(id string, operationErr error, interrupted bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id]
	if !ok {
		return osErrNotFound(id)
	}
	var completionErr error
	if operationErr == nil && (j.TOTPWorkflowStatus != "running" || (j.ResetTOTP && j.TOTPResetPhase != "credential_persisted") || (j.LogoutAllDevicesAfterTOTP && j.LogoutAllDevicesStatus != "succeeded") || (j.ReloginAfterTOTP && j.TOTPReloginStatus != "succeeded") || (j.UploadAfterTOTP && j.TOTPUploadStatus != "succeeded")) {
		completionErr = errors.New("2FA 流程尚未完成全部所选步骤")
		operationErr = completionErr
	}
	if operationErr != nil {
		j.TOTPWorkflowStatus, j.TOTPWorkflowError = "failed", safeError(operationErr)
		if interrupted {
			j.TOTPWorkflowStatus = "interrupted"
		}
		if j.TOTPReloginStatus == "running" {
			j.TOTPReloginStatus = "failed"
		} else if j.TOTPReloginStatus == "pending" {
			j.TOTPReloginStatus = "skipped"
		}
		if j.TOTPUploadStatus == "running" {
			// A remote write might already have been accepted. Keep the
			// uncertainty visible and never retry it in this workflow.
			j.TOTPUploadStatus = "unknown"
		} else if j.TOTPUploadStatus == "pending" {
			j.TOTPUploadStatus = "skipped"
		}
		if j.LogoutAllDevicesStatus == "pending" {
			j.LogoutAllDevicesStatus = "skipped"
		}
	} else {
		j.TOTPWorkflowStatus, j.TOTPWorkflowError = "succeeded", ""
	}
	j.TOTPWorkflowError = SanitizeJobForPublic(j).TOTPWorkflowError
	delete(j.Input, "sub2apiConfig")
	j.UpdatedAt = time.Now().UTC()
	m.persistLocked()
	return errors.Join(completionErr, m.lastPersistError)
}
