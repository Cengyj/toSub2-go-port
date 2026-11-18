package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/protocol"
	"tosub2-go-port/internal/resetlog"
)

// A reset can invalidate the only usable factor. It requires a durable vault
// and must stop before network access when private recovery evidence remains.
func validateTOTPResetStart(root, id, email string, durable bool) error {
	if !durable {
		return &protocol.ResetStorageFailure{Stage: "preflight", Reason: "STORAGE_BLOCKED", Message: "TOTP_RESET_PERSISTENCE_REQUIRED: 重置 2FA 需要可用的系统安全凭据存储"}
	}
	return protocol.ValidateTOTPResetStart(filepath.Join(root, id, "totp-setup-result.json"), email)
}

func configureTOTPReset(flow *protocol.Flow, manager *jobs.Manager, vault credentials.Store, job *jobs.Job, resultPath string) {
	flow.Options.LogoutAllDevicesAfterTOTP = job.LogoutAllDevicesAfterTOTP
	flow.Options.TOTPEnrollment = func(ctx context.Context, secret, uri string) error {
		record, err := protocol.ReadTOTPResetIdentity(resultPath, job.Email)
		if err != nil {
			return err
		}
		b, err := resetEnrollmentBinding(record, job.Email, secret, uri)
		if err != nil {
			return err
		}
		current, err := credentials.LoadContext(ctx, vault, job.Email)
		if err != nil || !current.TOTPResetPending || current.TOTPResetOperationID != b.operationID {
			return errors.New("TOTP_RESET_PERSISTENCE_REQUIRED: 无法确认待保存凭据，未发送激活请求")
		}
		if current.PendingTOTPSecret != "" && current.PendingTOTPSecret != secret || current.PendingTOTPFactorID != "" && current.PendingTOTPFactorID != b.factorID {
			return errors.New("TOTP_RECOVERY_CONFLICT: 已保存的待激活密钥与登记结果不同，未覆盖原资料")
		}
		current.PendingTOTPSecret, current.PendingTOTPURI, current.PendingTOTPFactorID = secret, uri, b.factorID
		if err := credentials.SaveContext(ctx, vault, job.Email, current); err != nil {
			return &protocol.ResetStorageFailure{Stage: "pending", Reason: "STORAGE_WRITE_FAILED", Message: "TOTP_RESET_PERSISTENCE_REQUIRED: 新登记密钥保存失败，未发送激活请求"}
		}
		readback, err := credentials.LoadContext(ctx, vault, job.Email)
		if err != nil || !credentials.EqualReadback(current, readback) {
			return &protocol.ResetStorageFailure{Stage: "pending", Reason: "STORAGE_READBACK_FAILED", Message: "TOTP_RESET_PERSISTENCE_REQUIRED: 新登记密钥读回不一致，未发送激活请求"}
		}
		return manager.UpdateTOTPSetup(job.ID, secret, uri, "", false)
	}
	flow.Options.TOTPResetBeforeDisable = func(ctx context.Context) error {
		operationID, err := resetOperationID(resultPath, job.Email)
		if err != nil {
			return err
		}
		return manager.BeginTOTPReset(ctx, job.ID, operationID)
	}
	flow.Options.TOTPResetPersist = func(ctx context.Context, secret, uri string) error {
		record, err := protocol.ReadTOTPResetCompletion(resultPath, job.Email)
		if err != nil {
			return err
		}
		b, err := resetEnrollmentBinding(record, job.Email, secret, uri)
		if err != nil {
			return err
		}
		return commitActivatedTOTP(ctx, vault, manager, job, b)
	}
	flow.Options.TOTPResetBeforeLogout = func(ctx context.Context) error {
		operationID, err := resetOperationID(resultPath, job.Email)
		if err != nil {
			return err
		}
		if err := manager.MarkTOTPResetCommitted(ctx, job.ID, operationID); err != nil {
			return err
		}
		if job.TOTPWorkflowStatus != "" {
			return manager.StartTOTPWorkflowStep(ctx, job.ID, "logout")
		}
		return nil
	}
}

func resetOperationID(path, email string) (string, error) {
	record, err := protocol.ReadTOTPResetIdentity(path, email)
	if err != nil {
		return "", err
	}
	id, _ := record["operation_id"].(string)
	if id == "" {
		return "", errors.New("TOTP_RECOVERY_CONFLICT: 操作记录缺少操作身份")
	}
	return id, nil
}

// Only complete activation evidence bound to this account and operation can
// finish the pending vault write. This entry point performs no network work.
func finalizeTOTPResetLocally(ctx context.Context, path string, manager *jobs.Manager, vault credentials.Store, job *jobs.Job) error {
	result, err := protocol.ReadTOTPResetCompletion(path, job.Email)
	if err != nil {
		return err
	}
	return finishBoundTOTPCompletion(ctx, path, manager, vault, job, result)
}

func finishBoundTOTPCompletion(ctx context.Context, path string, manager *jobs.Manager, vault credentials.Store, job *jobs.Job, result map[string]any) error {
	secret, _ := result["secret"].(string)
	uri, _ := result["otpauth_uri"].(string)
	factorID, _ := result["new_totp_factor_id"].(string)
	operationID, _ := result["operation_id"].(string)
	if operationID == "" || job.TOTPResetOperationID != operationID {
		return errors.New("TOTP_COMPLETION_CONFLICT: 本地完成记录与任务身份不一致，未覆盖凭据")
	}
	b := resetCredentialBinding{operationID: operationID, factorID: factorID, secret: secret, uri: uri}
	if err := commitActivatedTOTP(ctx, vault, manager, job, b); err != nil {
		return err
	}
	// Historical operation evidence stays byte-for-byte intact; its embedded
	// session is never restored. Vault readback and the final metadata commit
	// record actual local completion.
	if !protocol.TOTPResetJournalIsReadOnly(result) {
		if err := protocol.MarkTOTPResetCredentialPersisted(path, job.Email, operationID); err != nil {
			return err
		}
	}
	result["credential_persisted"], result["reset_totp_phase"] = true, "credential_persisted"
	if err := manager.UpdateTOTPResetResult(job.ID, result, nil); err != nil {
		return err
	}
	return finishTOTPResetLocally(ctx, manager, job, operationID)
}

func emitLocalResetDiagnostic(job *jobs.Job, emit func(jobs.JobEvent), record map[string]any, err error, committed bool) {
	if emit == nil {
		return
	}
	id := ""
	seq := 1
	if job.Operation == "complete-2fa" {
		id = job.TOTPResetOperationID
		ref := resetlog.OperationRef(id)
		for _, e := range job.LogEntries {
			if d := e.Diagnostic; d != nil && d.OpRef == ref && d.Seq >= seq {
				seq = d.Seq + 1
			}
		}
		// If no display history survives, the protected checkpoint still
		// identifies the operation. Do not invent a sequence from wall time.
		if n, ok := record["support_seq"].(float64); ok && n >= 0 && n < 1e7 {
			seq = max(seq, int(n)+1)
		}
	}
	d := protocol.LocalResetLog(id, seq, record, err, committed)
	emit(jobs.JobEvent{JobID: job.ID, Stage: "totp_reset_diagnostic", At: time.Now().UTC(), Prompt: d.Message(), Data: map[string]any{resetlog.DataKey: d}})
}

func preserveAccountCredentialBaseline(ctx context.Context, vault credentials.Store, email, proxy string) error {
	current, err := credentials.LoadContext(ctx, vault, email)
	missing := errors.Is(err, os.ErrNotExist)
	if err != nil && !missing {
		return &protocol.ResetStorageFailure{Stage: "preflight", Reason: "STORAGE_BLOCKED", Message: "TOTP_RESET_PERSISTENCE_REQUIRED: 无法读取账号凭据的代理绑定"}
	}
	// A durable provider may have no old password or factor for an account
	// whose original web session is still usable. Establish and read back its
	// empty factor baseline before any reset request, including direct access.
	if missing || current.ProxyURL != proxy {
		current.ProxyURL = proxy
		if err := credentials.SaveContext(ctx, vault, email, current); err != nil {
			return &protocol.ResetStorageFailure{Stage: "preflight", Reason: "STORAGE_WRITE_FAILED", Message: "TOTP_RESET_PERSISTENCE_REQUIRED: 无法保存账号凭据的代理绑定"}
		}
	}
	readback, err := credentials.LoadContext(ctx, vault, email)
	if err != nil || !credentials.EqualReadback(readback, current) {
		return &protocol.ResetStorageFailure{Stage: "preflight", Reason: "STORAGE_READBACK_FAILED", Message: "TOTP_RESET_PERSISTENCE_REQUIRED: 账号凭据的代理绑定读回失败"}
	}
	return nil
}

// Complete only durable local credential and metadata work. A cancelled
// caller cannot undo an activation that already has reliable success evidence.
func finishTOTPResetLocally(ctx context.Context, manager *jobs.Manager, job *jobs.Job, operationID string) error {
	localCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	return manager.MarkTOTPResetCommitted(localCtx, job.ID, operationID)
}
