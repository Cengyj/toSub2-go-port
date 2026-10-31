package application

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/sub2api"
	"tosub2-go-port/internal/transport"
)

// UploadSavedAuthorization uploads only the committed grant matching this job.
func UploadSavedAuthorization(ctx context.Context, root string, manager *jobs.Manager, job *jobs.Job, onRetry func(transport.ReadRetryEvent)) (err error) {
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
	cfg, options, err := sub2api.ParseUploadSettings(values["sub2apiConfig"])
	if err != nil {
		return err
	}
	cfg.OnRetry = onRetry
	data, err := readCommittedAuthorization(ctx, root, job.ID, 16<<20)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		if errors.Is(err, ErrInsightRecoveryPending) {
			return errors.New("新的授权刷新结果尚未恢复保存，已停止上传")
		}
		return errors.New("新授权文件不存在、权限不安全或不可读取，已停止上传")
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
	payload, err := sub2api.BuildUploadPayload(document.Accounts, options)
	if err != nil {
		return err
	}
	_, err = sub2api.New(cfg).Upload(ctx, payload)
	if err != nil {
		return &workflowUploadError{message: sub2api.SafeError(err), cause: err, uncertain: sub2api.IsMutationOutcomeUnknown(err)}
	}
	return nil
}
