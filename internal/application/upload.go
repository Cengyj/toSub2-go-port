package application

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
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
		return &workflowUploadError{message: sub2api.SafeError(err), cause: err, uncertain: sub2api.IsMutationOutcomeUnknown(err)}
	}
	return nil
}
