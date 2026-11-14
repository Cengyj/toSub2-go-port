package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"time"

	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/protocol"
)

// A reset can invalidate the only usable factor. It requires a durable vault
// and must stop before network access when private recovery evidence remains.
func validateTOTPResetStart(root, id, email string, durable bool) error {
	if !durable {
		return errors.New("TOTP_RESET_PERSISTENCE_REQUIRED: 重置 2FA 需要可用的系统安全凭据存储")
	}
	return protocol.ValidateTOTPResetStart(filepath.Join(root, id, "totp-setup-result.json"), email)
}

func configureTOTPReset(flow *protocol.Flow, manager *jobs.Manager, vault credentials.Store, job *jobs.Job, resultPath string) {
	flow.Options.LogoutAllDevicesAfterTOTP = job.LogoutAllDevicesAfterTOTP
	flow.Options.TOTPEnrollment = func(ctx context.Context, secret, uri string) error {
		operationID, err := resetOperationID(resultPath, job.Email)
		if err != nil {
			return err
		}
		current, err := credentials.LoadContext(ctx, vault, job.Email)
		if err != nil || !current.TOTPResetPending || current.TOTPResetOperationID != operationID {
			return errors.New("TOTP_RESET_PERSISTENCE_REQUIRED: 无法确认待保存凭据，未发送激活请求")
		}
		if current.PendingTOTPSecret != "" && current.PendingTOTPSecret != secret {
			return errors.New("TOTP_RECOVERY_CONFLICT: 已保存的待激活密钥与登记结果不同，未覆盖原资料")
		}
		current.PendingTOTPSecret, current.PendingTOTPURI = secret, uri
		if err := credentials.SaveContext(ctx, vault, job.Email, current); err != nil {
			return errors.New("TOTP_RESET_PERSISTENCE_REQUIRED: 新登记密钥保存失败，未发送激活请求")
		}
		readback, err := credentials.LoadContext(ctx, vault, job.Email)
		if err != nil || !reflect.DeepEqual(current, readback) {
			return errors.New("TOTP_RESET_PERSISTENCE_REQUIRED: 新登记密钥读回不一致，未发送激活请求")
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
		operationID, err := resetOperationID(resultPath, job.Email)
		if err != nil {
			return err
		}
		current, err := credentials.LoadContext(ctx, vault, job.Email)
		if err != nil {
			return errors.New("TOTP_RESET_PERSISTENCE_REQUIRED: 2FA 重置持久化状态不可用")
		}
		if current.TOTPResetOperationID != operationID || (current.PendingTOTPSecret != "" && current.PendingTOTPSecret != secret) {
			return errors.New("TOTP_RECOVERY_CONFLICT: 当前凭据属于另一轮重置或密钥不同，未覆盖操作证据")
		}
		if !current.TOTPResetPending {
			// A crash may follow a successful vault save but precede metadata
			// commit. Only the identical already-saved seed may finish that commit.
			if current.TOTPSecret != secret || current.PendingTOTPSecret != "" || current.PendingTOTPURI != "" {
				return errors.New("TOTP_RECOVERY_CONFLICT: 当前有效凭据与本次激活结果不一致，未覆盖凭据")
			}
			return manager.CommitTOTPReset(ctx, job.ID, current)
		}
		if current.PendingTOTPSecret != secret || current.PendingTOTPURI != uri {
			return errors.New("TOTP_COMPLETION_CONFLICT: 待保存凭据与本次登记材料不一致")
		}
		current.TOTPSecret = secret
		current.PendingTOTPSecret, current.PendingTOTPURI = "", ""
		current.TOTPResetPending = false
		if err := credentials.SaveContext(ctx, vault, job.Email, current); err != nil {
			return errors.New("TOTP_RESET_PERSISTENCE_REQUIRED: 新密钥未能安全保存，操作证据已保留")
		}
		saved, err := credentials.LoadContext(ctx, vault, job.Email)
		if err != nil || !reflect.DeepEqual(saved, current) {
			return errors.New("TOTP_RESET_PERSISTENCE_REQUIRED: 新密钥保存后读回校验失败，操作证据已保留")
		}
		if err := manager.CommitTOTPReset(ctx, job.ID, saved); err != nil {
			return err
		}
		return nil
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
	secret, _ := result["secret"].(string)
	uri, _ := result["otpauth_uri"].(string)
	operationID, _ := result["operation_id"].(string)
	if secret == "" || operationID == "" || job.TOTPResetOperationID != operationID {
		return errors.New("TOTP_COMPLETION_CONFLICT: 本地完成记录与任务身份不一致，未覆盖凭据")
	}
	current, err := credentials.LoadContext(ctx, vault, job.Email)
	if err != nil {
		return errors.New("TOTP_RESET_PERSISTENCE_REQUIRED: 无法读取已确认激活的凭据，证据已保留")
	}
	if current.TOTPResetOperationID != operationID || (current.PendingTOTPSecret != "" && current.PendingTOTPSecret != secret) {
		return errors.New("TOTP_COMPLETION_CONFLICT: 当前凭据不属于本次操作，未覆盖凭据")
	}
	if current.TOTPResetPending {
		if current.PendingTOTPSecret != secret || current.PendingTOTPURI != uri {
			return errors.New("TOTP_COMPLETION_CONFLICT: 缺少与激活证据匹配的待保存密钥")
		}
		current.TOTPSecret, current.PendingTOTPSecret, current.PendingTOTPURI = secret, "", ""
		current.TOTPResetPending = false
		if err := credentials.SaveContext(ctx, vault, job.Email, current); err != nil {
			return errors.New("TOTP_RESET_PERSISTENCE_REQUIRED: 已确认激活的新密钥尚未安全保存，证据已保留")
		}
		readback, err := credentials.LoadContext(ctx, vault, job.Email)
		if err != nil || !reflect.DeepEqual(current, readback) {
			return errors.New("TOTP_RESET_PERSISTENCE_REQUIRED: 新密钥保存后读回失败，证据已保留")
		}
	}
	if current.TOTPSecret != secret || current.PendingTOTPSecret != "" || current.PendingTOTPURI != "" {
		return errors.New("TOTP_COMPLETION_CONFLICT: 已保存的有效密钥与本次激活证据不同")
	}
	if err := manager.CommitTOTPReset(ctx, job.ID, current); err != nil {
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

func preserveAccountCredentialBaseline(ctx context.Context, vault credentials.Store, email, proxy string) error {
	current, err := credentials.LoadContext(ctx, vault, email)
	missing := errors.Is(err, os.ErrNotExist)
	if err != nil && !missing {
		return errors.New("TOTP_RESET_PERSISTENCE_REQUIRED: 无法保存账号凭据的代理绑定")
	}
	// A durable provider may have no old password or factor for an account
	// whose original web session is still usable. Establish and read back its
	// empty factor baseline before any reset request, including direct access.
	if missing || current.ProxyURL != proxy {
		current.ProxyURL = proxy
		if err := credentials.SaveContext(ctx, vault, email, current); err != nil {
			return errors.New("TOTP_RESET_PERSISTENCE_REQUIRED: 无法保存账号凭据的代理绑定")
		}
	}
	readback, err := credentials.LoadContext(ctx, vault, email)
	if err != nil || !reflect.DeepEqual(readback, current) {
		return errors.New("TOTP_RESET_PERSISTENCE_REQUIRED: 账号凭据的代理绑定读回失败")
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
