package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"tosub2-go-port/internal/console"
	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/sub2api"
	"tosub2-go-port/internal/transport"
)

// All selected work remains inside one scheduler lease for the email. Re-login
// uses a fresh protocol session and reloads the newly committed factor.
func makeTOTPWorkflowRunner(manager *jobs.Manager, vault credentials.Store, operation, upload jobs.Runner) jobs.Runner {
	return func(ctx context.Context, job *jobs.Job, emit func(jobs.JobEvent)) (err error) {
		if job.TOTPWorkflowStatus == "" || (job.Operation != "reset-2fa" && job.Operation != "setup-2fa" && job.Operation != "setup_2fa" && job.Operation != "setup-totp" && job.Operation != "totp_setup") {
			return operation(ctx, job, emit)
		}
		if job.TOTPWorkflowStatus != "pending" {
			return errors.New("2FA 流程不能自动重放，请重新选择要执行的操作")
		}
		defer func() {
			if err != nil {
				values, _ := manager.InputCopy(job.ID)
				safe := jobs.SanitizeJobForPublic(&jobs.Job{Input: values, Error: err.Error()}).Error
				err = &totpWorkflowError{message: safe, cause: err}
			}
			err = errors.Join(err, manager.FinishTOTPWorkflowContext(ctx, job.ID, err))
		}()
		if job.UploadAfterTOTP {
			if !job.ReloginAfterTOTP {
				return errors.New("自动上传需要先完成重新授权")
			}
			values, _ := manager.InputCopy(job.ID)
			if _, _, err = console.ParseTOTPSub2APIConfig(values["sub2apiConfig"]); err != nil {
				return err
			}
		}
		if err = manager.StartTOTPWorkflowStep(ctx, job.ID, "totp"); err != nil {
			return err
		}
		if err = operation(ctx, job, emit); err != nil {
			return err
		}
		if err = ctx.Err(); err != nil {
			return err
		}
		current, ok := manager.Get(job.ID)
		if !ok {
			return errors.New("2FA 流程账号不可用")
		}
		if current.TOTPRecoveryPending || current.TOTPCredentialInvalidated {
			return jobs.ErrTOTPRecoveryRequired
		}
		if current.LogoutAllDevicesAfterTOTP && current.LogoutAllDevicesStatus != "succeeded" {
			reason := strings.TrimSpace(jobs.SanitizeJobForPublic(current).LogoutAllDevicesError)
			if reason != "" {
				return errors.New("新 2FA 已保存；退出所有设备未完成：" + reason + "；已停止重新授权和上传")
			}
			return errors.New("新 2FA 已保存，但退出所有设备尚未确认成功，已停止重新授权和上传")
		}
		if job.ReloginAfterTOTP {
			// Setup's already-enabled response can hide an unresolved pending
			// factor. Follow-ups require a verified, durable active credential.
			saved, e := credentials.LoadContext(ctx, vault, job.Email)
			if e != nil || saved.TOTPResetPending || saved.TOTPSecret == "" || saved.PendingTOTPSecret != "" || !current.HasTOTPKey {
				return errors.New("新的 2FA 密钥尚未确认并安全保存，已停止重新授权和上传")
			}
			if err = manager.StartTOTPWorkflowStep(ctx, job.ID, "relogin"); err != nil {
				return err
			}
			current.Operation, current.ResetTOTP = "relogin", false
			if err = operation(ctx, current, emit); err != nil {
				return err
			}
			fresh, exists := manager.Get(job.ID)
			if !exists || !fresh.ResultSaved {
				return errors.New("重新授权未生成可用的新授权文件，已停止上传")
			}
			if err = manager.CompleteTOTPWorkflowStep(ctx, job.ID, "relogin"); err != nil {
				return err
			}
		}
		if job.UploadAfterTOTP {
			if !job.ReloginAfterTOTP {
				return errors.New("自动上传需要先完成重新授权")
			}
			if err = manager.StartTOTPWorkflowStep(ctx, job.ID, "upload"); err != nil {
				return err
			}
			if err = upload(ctx, job, emit); err != nil {
				return err
			}
			if err = manager.CompleteTOTPWorkflowStep(ctx, job.ID, "upload"); err != nil {
				return err
			}
		}
		return ctx.Err()
	}
}

type totpWorkflowError struct {
	message string
	cause   error
}

func (e *totpWorkflowError) Error() string { return e.message }
func (e *totpWorkflowError) Unwrap() error { return e.cause }

func uploadTOTPWorkflow(ctx context.Context, root string, manager *jobs.Manager, job *jobs.Job, reporters ...func(jobs.JobEvent)) (err error) {
	defer func() {
		var outcome interface{ UploadOutcomeUncertain() bool }
		if err != nil && !errors.As(err, &outcome) {
			err = &workflowUploadError{message: err.Error(), cause: err}
		}
	}()
	values, ok := manager.InputCopy(job.ID)
	if !ok {
		return errors.New("Sub2API 上传配置已失效，请重新配置后手动上传")
	}
	cfg, options, err := console.ParseTOTPSub2APIConfig(values["sub2apiConfig"])
	if err != nil {
		return err
	}
	if len(reporters) > 0 && reporters[0] != nil {
		cfg.OnRetry = networkRetryReporter(ctx, job.ID, "Sub2API 上传：", reporters[0])
	}
	path := filepath.Join(root, job.ID, "sub2api-import-oauth.json")
	stat, err := os.Lstat(path)
	if err != nil || !stat.Mode().IsRegular() || stat.Size() > 16<<20 {
		return errors.New("新授权文件不存在或不可读取，已停止上传")
	}
	file, err := os.Open(path)
	if err != nil {
		return errors.New("新授权文件不可读取，已停止上传")
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(stat, opened) {
		return errors.New("新授权文件状态发生变化，已停止上传")
	}
	data, err := io.ReadAll(io.LimitReader(file, (16<<20)+1))
	var document struct {
		Type     string           `json:"type"`
		Accounts []map[string]any `json:"accounts"`
	}
	if err != nil || len(data) > 16<<20 || json.Unmarshal(data, &document) != nil || document.Type != "sub2api-data" || len(document.Accounts) != 1 {
		return errors.New("新授权文件格式不正确，已停止上传")
	}
	creds, _ := document.Accounts[0]["credentials"].(map[string]any)
	email, _ := creds["email"].(string)
	if !strings.EqualFold(strings.TrimSpace(email), strings.TrimSpace(job.Email)) {
		return errors.New("新授权文件与当前账号不一致，已停止上传")
	}
	payload, err := sub2api.BuildUploadPayload(document.Accounts, options)
	if err != nil {
		return err
	}
	_, err = sub2api.New(cfg).Upload(ctx, payload)
	if err != nil {
		uncertain := !transport.IsRequestDefinitelyUnsent(err)
		var remote *sub2api.RemoteError
		if errors.As(err, &remote) && remote.StatusCode >= 400 && remote.StatusCode < 500 {
			uncertain = false
		}
		return &workflowUploadError{message: sub2api.SafeError(err), cause: err, uncertain: uncertain}
	}
	return nil
}
