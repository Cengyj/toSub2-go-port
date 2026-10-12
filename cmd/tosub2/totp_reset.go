package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"

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

func configureTOTPReset(flow *protocol.Flow, manager *jobs.Manager, vault credentials.Store, job *jobs.Job) {
	flow.Options.LogoutAllDevicesAfterTOTP = job.LogoutAllDevicesAfterTOTP
	var original credentials.Credentials
	var captured bool
	flow.Options.TOTPResetBeforeDisable = func(ctx context.Context) error {
		previous, err := credentials.LoadContext(ctx, vault, job.Email)
		if err != nil && !errors.Is(err, os.ErrNotExist) || previous.TOTPResetPending {
			return errors.New("无法确认重置前的安全凭据，未发送 2FA 修改请求")
		}
		original, captured = previous, true
		return manager.BeginTOTPReset(ctx, job.ID)
	}
	flow.Options.TOTPResetAbortBeforeSend = func(ctx context.Context) error {
		if !captured {
			return errors.New("重置前的安全凭据不可用，恢复隔离已保留")
		}
		return manager.AbortTOTPResetBeforeSend(ctx, job.ID, original)
	}
	flow.Options.TOTPResetPersist = func(ctx context.Context, secret, uri string) error {
		current, err := credentials.LoadContext(ctx, vault, job.Email)
		if err != nil || !current.TOTPResetPending {
			return errors.New("TOTP_RESET_PERSISTENCE_REQUIRED: 2FA 重置持久化状态不可用")
		}
		current.TOTPSecret = secret
		current.PendingTOTPSecret, current.PendingTOTPURI = "", ""
		current.TOTPResetPending = false
		if err := credentials.SaveContext(ctx, vault, job.Email, current); err != nil {
			return errors.New("TOTP_RESET_PERSISTENCE_REQUIRED: 新密钥未能安全保存，恢复资料已保留")
		}
		saved, err := credentials.LoadContext(ctx, vault, job.Email)
		if err != nil || saved.TOTPSecret != secret || saved.TOTPResetPending || saved.PendingTOTPSecret != "" {
			return errors.New("TOTP_RESET_PERSISTENCE_REQUIRED: 新密钥保存后读回校验失败，恢复资料已保留")
		}
		if err := manager.CommitTOTPReset(ctx, job.ID, saved); err != nil {
			return err
		}
		if job.LogoutAllDevicesAfterTOTP && job.TOTPWorkflowStatus != "" {
			return manager.StartTOTPWorkflowStep(ctx, job.ID, "logout")
		}
		return nil
	}
}
