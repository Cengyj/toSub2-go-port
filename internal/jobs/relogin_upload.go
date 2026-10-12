package jobs

import (
	"context"
	"errors"
	"time"
)

func initializeReloginUpload(j *Job, values map[string]any) {
	j.UploadAfterRelogin, j.ReloginAuthorizationSaved = false, false
	j.ReloginUploadStatus, j.ReloginUploadError = "", ""
	if j.Operation != "relogin" {
		return
	}
	j.UploadAfterRelogin, _ = values["uploadAfterRelogin"].(bool)
	if j.UploadAfterRelogin {
		j.ReloginUploadStatus = "pending"
	}
}

func interruptReloginUpload(j *Job, message string) {
	if !j.UploadAfterRelogin {
		return
	}
	switch j.ReloginUploadStatus {
	case "pending":
		j.ReloginUploadStatus, j.ReloginUploadError = "skipped", message
	case "running":
		j.ReloginUploadStatus, j.ReloginUploadError = "unknown", message
	default:
		return
	}
	delete(j.Input, "sub2apiConfig")
}

// StartReloginUpload acknowledges the new grant before starting a remote
// mutation. A previously saved grant alone is insufficient for this step.
func (m *Manager) StartReloginUpload(ctx context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	j, ok := m.jobs[id]
	if !ok {
		return osErrNotFound(id)
	}
	if j.Operation != "relogin" || !j.UploadAfterRelogin || j.ReloginUploadStatus != "pending" || !j.ReloginAuthorizationSaved || !j.ResultSaved || j.TOTPRecoveryPending || j.TOTPCredentialInvalidated {
		return errors.New("自动上传需要本次重新授权成功并保存新授权文件")
	}
	j.ReloginUploadStatus, j.ReloginUploadError = "running", ""
	j.UpdatedAt = time.Now().UTC()
	m.emitLocked(JobEvent{JobID: id, Stage: "relogin_upload", Status: StatusRunning, Prompt: "新授权已保存，正在同步到 Sub2API", At: j.UpdatedAt})
	m.persistLocked()
	if m.store == nil || m.lastPersistError != nil {
		return errors.New("自动上传阶段无法可靠保存，已停止上传")
	}
	return nil
}

// FinishReloginUpload keeps the authorization result separate from its optional
// sync outcome and removes the process-local integration key at the boundary.
func (m *Manager) FinishReloginUpload(id string, uploadErr error, uncertain bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id]
	if !ok {
		return osErrNotFound(id)
	}
	if !j.UploadAfterRelogin || (j.ReloginUploadStatus != "pending" && j.ReloginUploadStatus != "running") {
		return errors.New("重新授权后的上传流程已结束")
	}
	if uploadErr == nil && j.ReloginUploadStatus != "running" {
		return errors.New("自动上传尚未执行")
	}
	status, prompt := "succeeded", "新授权已同步到 Sub2API"
	if uploadErr != nil {
		j.ReloginUploadError = safeError(uploadErr)
		j.ReloginUploadError = SanitizeJobForPublic(j).ReloginUploadError
		status, prompt = "failed", "新授权已保存，Sub2API 同步失败"
		if j.ReloginUploadStatus == "pending" {
			status, prompt = "skipped", "Sub2API 同步未执行"
		} else if uncertain {
			status, prompt = "unknown", "新授权已保存，Sub2API 同步结果待核实"
		}
	}
	j.ReloginUploadStatus = status
	j.UpdatedAt = time.Now().UTC()
	data := map[string]any{}
	if j.ReloginUploadError != "" {
		data["mailApiError"] = j.ReloginUploadError
	}
	m.appendOperationLogLocked(j, JobEvent{JobID: id, Stage: "relogin_upload", Status: status, Prompt: prompt, Data: data, At: j.UpdatedAt})
	delete(j.Input, "sub2apiConfig")
	m.persistLocked()
	return m.lastPersistError
}
