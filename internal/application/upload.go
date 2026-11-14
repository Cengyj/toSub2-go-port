package application

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"tosub2-go-port/internal/accountlock"
	"tosub2-go-port/internal/accountplan"
	"tosub2-go-port/internal/accountquality"
	"tosub2-go-port/internal/authmaterial"
	"tosub2-go-port/internal/cpa"
	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/sub2api"
	"tosub2-go-port/internal/transport"
)

// UploadSavedAuthorizationAtOrigin owns the cross-process account lease for the
// complete optional sync. Authentication's lease has already ended when the
// workflow reaches this entry; scheduler ownership alone cannot protect it.
func UploadSavedAuthorizationAtOrigin(ctx context.Context, root, origin string, manager *jobs.Manager, job *jobs.Job, onRetry func(transport.ReadRetryEvent)) (err error) {
	defer func() {
		var outcome interface{ UploadOutcomeUncertain() bool }
		if err != nil && !errors.As(err, &outcome) {
			err = &workflowUploadError{message: err.Error(), cause: err}
		}
	}()
	if err := ctx.Err(); err != nil {
		return err
	}
	if origin == "" {
		origin = "https://chatgpt.com"
	}
	release, err := accountlock.Acquire(origin, job.Email)
	if err != nil {
		message := "无法取得本机账号互斥锁，已停止上传，请稍后重试"
		if errors.Is(err, accountlock.ErrBusy) {
			message = accountlock.ErrBusy.Error()
		}
		return &workflowUploadError{message: message, cause: err}
	}
	defer func() {
		if releaseErr := release(); releaseErr != nil {
			var outcome interface{ UploadOutcomeUncertain() bool }
			uncertain := errors.As(err, &outcome) && outcome.UploadOutcomeUncertain()
			// File-close errors may include private cache paths. Keep their cause
			// without displaying it or changing the confirmed remote outcome.
			err = errors.Join(err, &workflowUploadError{message: "本机账号互斥锁释放未确认，请稍后重试", cause: releaseErr, uncertain: uncertain})
		}
	}()
	// Another instance may have changed recovery state after authentication
	// released its lock. Re-read it only after this upload owns the OS lease.
	if err := manager.CheckTOTPWorkflowExport(job.ID); err != nil {
		return err
	}
	values, ok := manager.InputCopy(job.ID)
	if !ok {
		return errors.New("上传配置已失效，请重新配置后手动上传")
	}
	settings, err := parseWorkflowUploadSettings(values)
	if err != nil {
		return err
	}
	checkAuthorization := func(ctx context.Context, account map[string]any) error {
		if err := manager.CheckTOTPWorkflowExport(job.ID); err != nil {
			return err
		}
		configured, err := manager.CheckAuthorizationSync(ctx, job.ID, account)
		if configured {
			if err != nil {
				return err
			}
			return manager.CheckTOTPWorkflowExport(job.ID)
		}
		// Standalone callers must not silently bypass a policy saved by the
		// console. They may keep the legacy path only when gating is disabled.
		settings, err := accountquality.ReadSettings(root)
		if err != nil || settings.Enabled && settings.BeforeUpload {
			return &sub2api.AuthorizationGateError{Reason: "settings"}
		}
		return manager.CheckTOTPWorkflowExport(job.ID)
	}
	data, err := ReadCommittedAuthorization(ctx, root, job.ID, 16<<20)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		if errors.Is(err, ErrInsightRecoveryPending) {
			return errors.New("新的授权刷新结果尚未恢复保存，已停止上传")
		}
		return errors.New("新授权文件不存在、权限不安全或不可读取，已停止上传")
	}
	if authmaterial.ContainsLoginSessionJSON(data) {
		return errors.New("授权文件含旧登录会话材料，已停止上传")
	}
	var document struct {
		Type     string           `json:"type"`
		Accounts []map[string]any `json:"accounts"`
	}
	// Unknown vendor configuration is round-tripped during authorization sync.
	// Preserve its JSON numbers instead of silently rounding through float64.
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if decoder.Decode(&document) != nil || decoder.Decode(new(any)) != io.EOF || document.Type != "sub2api-data" || len(document.Accounts) != 1 {
		return errors.New("新授权文件格式不正确，已停止上传")
	}
	creds, _ := document.Accounts[0]["credentials"].(map[string]any)
	email, _ := creds["email"].(string)
	if !strings.EqualFold(strings.TrimSpace(email), strings.TrimSpace(job.Email)) {
		return errors.New("新授权文件与当前账号不一致，已停止上传")
	}
	if settings.target == jobs.UploadTargetCPA {
		file, err := cpa.BuildAuthFile(document.Accounts[0])
		if err != nil {
			return err
		}
		var identity struct {
			AccountID string `json:"account_id"`
		}
		if err := json.Unmarshal(file.Data, &identity); err != nil {
			return errors.New("CPA 授权身份不可读取，已停止上传")
		}
		if err := manager.CheckCPAAutoRepairUpload(job.ID, accountplan.FromAccount(document.Accounts[0]), identity.AccountID); err != nil {
			return err
		}
		client := cpa.New(settings.cpa)
		var currentRemotePlans []string
		if job.Operation == "relogin" && job.LastOperationType == "automatic_relogin" {
			files, err := client.ListAuthFiles(ctx)
			if err != nil {
				return manager.PauseCPAAutoRepairUpload(job.ID, "上传前无法确认 CPA 远端状态，已保留新授权："+cpa.SafeListError(err))
			}
			matched := false
			for _, remote := range files {
				if remote.Filename != file.Filename || !remote.MatchesIdentity(job.Email, identity.AccountID) || !remote.NeedsRelogin {
					continue
				}
				currentRemotePlans = []string{remote.PlanType}
				matched = true
				break
			}
			if !matched {
				return manager.PauseCPAAutoRepairUpload(job.ID, "CPA 原授权文件已不存在、身份不符或不再需要重登，已保留新授权并停止自动覆盖")
			}
		}
		if err := checkAuthorization(ctx, document.Accounts[0]); err != nil {
			return err
		}
		if err := manager.CheckCPAAutoRepairUpload(job.ID, accountplan.FromAccount(document.Accounts[0]), identity.AccountID, currentRemotePlans...); err != nil {
			return err
		}
		if err := client.Upload(ctx, file); err != nil {
			return &workflowUploadError{message: cpa.SafeError(err), cause: err, uncertain: cpa.IsMutationOutcomeUnknown(err)}
		}
		return nil
	}
	cfg := settings.sub2
	cfg.OnRetry, cfg.BeforeAuthorizationSync = onRetry, checkAuthorization
	payload, err := sub2api.BuildUploadPayload(document.Accounts, settings.options)
	if err != nil {
		return err
	}
	_, err = sub2api.New(cfg).Upload(ctx, payload)
	if err != nil {
		return &workflowUploadError{message: sub2api.SafeError(err), cause: err, uncertain: sub2api.IsMutationOutcomeUnknown(err)}
	}
	return nil
}
