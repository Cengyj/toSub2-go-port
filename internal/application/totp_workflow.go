package application

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/jobs"
)

// Do not put the recovered value in persisted state: a panic may contain
// credentials or a private response. Callers still receive the original panic.
var errWorkflowPanic = errors.New("操作意外中断，结果待核实；不会自动重放，请先检查已完成步骤")

// All selected work remains inside one scheduler lease for the email. Re-login
// uses a fresh protocol session and reloads the newly committed factor.
func TOTPWorkflowRunner(manager *jobs.Manager, vault credentials.Store, operation, upload jobs.Runner) jobs.Runner {
	return func(ctx context.Context, job *jobs.Job, emit func(jobs.JobEvent)) (err error) {
		if job.TOTPWorkflowStatus == "" || (job.Operation != "reset-2fa" && job.Operation != "complete-2fa" && job.Operation != "setup-2fa" && job.Operation != "setup_2fa" && job.Operation != "setup-totp" && job.Operation != "totp_setup") {
			return operation(ctx, job, emit)
		}
		if job.TOTPWorkflowStatus != "pending" {
			return errors.New("2FA 流程不能自动重放，请重新选择要执行的操作")
		}
		defer func() {
			panicValue := recover()
			if panicValue != nil {
				err = errWorkflowPanic
			}
			if err != nil {
				values, _ := manager.InputCopy(job.ID)
				safe := jobs.SanitizeJobForPublic(&jobs.Job{Input: values, Error: err.Error()}).Error
				err = &totpWorkflowError{message: safe, cause: err}
			}
			err = errors.Join(err, manager.FinishTOTPWorkflowContext(ctx, job.ID, err))
			if panicValue != nil {
				panic(panicValue)
			}
		}()
		if job.UploadAfterTOTP {
			if !job.ReloginAfterTOTP {
				return errors.New("自动上传需要先完成重新授权")
			}
			values, _ := manager.InputCopy(job.ID)
			if err = validateWorkflowUploadSettings(values, job.AuthorizationMethod); err != nil {
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
			outcome := "退出所有设备尚未确认成功"
			switch current.LogoutAllDevicesStatus {
			case "unknown":
				outcome = "退出所有设备结果待核实"
			case "failed":
				outcome = "退出所有设备请求被拒绝"
			}
			if reason := strings.TrimSpace(jobs.SanitizeJobForPublic(current).LogoutAllDevicesError); reason != "" {
				outcome += "：" + reason
			}
			return errors.New("新 2FA 已保存；" + outcome + "；已停止重新授权和上传")
		}
		if job.ReloginAfterTOTP {
			// Setup's already-enabled response can hide an unresolved pending
			// factor. Follow-ups require a verified, durable active credential.
			saved, e := credentials.LoadContext(ctx, vault, job.Email)
			if e != nil || saved.TOTPResetPending || saved.TOTPSecret == "" || saved.PendingTOTPSecret != "" || saved.PendingTOTPFactorID != "" || !current.HasTOTPKey {
				return errors.New("新的 2FA 密钥尚未确认并安全保存，已停止重新授权和上传")
			}
			if err = manager.StartTOTPWorkflowStep(ctx, job.ID, "relogin"); err != nil {
				return err
			}
			current.Operation, current.ResetTOTP = "relogin", false
			if err = operation(ctx, current, emit); err != nil {
				return fmt.Errorf("新 2FA 已保存；后续重登授权失败：%w", err)
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
