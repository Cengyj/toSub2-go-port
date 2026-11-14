package console

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"tosub2-go-port/internal/application"
	"tosub2-go-port/internal/authmaterial"
	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/sub2api"
)

type preparedSub2APIUpload struct {
	id   string
	name string
	data []byte
}

// Uploads consume only a committed private grant. Legacy fallback is allowed
// only when the current export is absent, and shares every safety check.
func (s *Server) readCommittedUpload(ctx context.Context, job *jobs.Job) ([]byte, jobExport, string, error) {
	name := "sub2api-import-oauth.json"
	if _, err := os.Lstat(filepath.Join(s.cfg.OutputRoot, job.ID, name)); errors.Is(err, os.ErrNotExist) {
		name = "sub2api-data.json"
	}
	data, err := application.ReadCommittedAuthorizationExport(ctx, s.cfg.OutputRoot, job.ID, name, 16<<20)
	if err != nil {
		return nil, jobExport{}, "", errors.New("授权文件尚未提交、正在恢复或无法安全读取，已停止 Sub2API 上传")
	}
	if authmaterial.ContainsLoginSessionJSON(data) {
		return nil, jobExport{}, "", errors.New("导入文件含已移除的登录会话材料，已停止上传")
	}
	var doc jobExport
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if decoder.Decode(&doc) != nil || decoder.Decode(new(any)) != io.EOF || doc.Type != "sub2api-data" || len(doc.Accounts) != 1 {
		return nil, jobExport{}, "", errors.New("选中任务的导入文件格式不正确")
	}
	account, valid := doc.Accounts[0].(map[string]any)
	if !valid || sub2api.AccountEmail(account) != strings.ToLower(strings.TrimSpace(job.Email)) {
		return nil, jobExport{}, "", errors.New("导入文件与选中账号身份不一致，已停止上传")
	}
	return data, doc, name, nil
}

// Call only after the upload session owns the account and OS leases, including
// when optional model inference is disabled. Repeated mutation checks retain
// the same captured bytes and cannot silently switch to a newer grant.
func (s *Server) checkPreparedSub2APIUploads(ctx context.Context, prepared []preparedSub2APIUpload) error {
	for _, item := range prepared {
		if err := s.jobs.CheckTOTPExport(item.id); err != nil {
			return err
		}
		name := item.name
		if name == "" {
			name = "sub2api-import-oauth.json"
		}
		// A newly committed current export supersedes a prepared legacy one.
		if name == "sub2api-data.json" {
			if _, err := os.Lstat(filepath.Join(s.cfg.OutputRoot, item.id, "sub2api-import-oauth.json")); !errors.Is(err, os.ErrNotExist) {
				return &sub2api.AuthorizationGateError{Reason: "authorization_changed"}
			}
		}
		current, err := application.ReadCommittedAuthorizationExport(ctx, s.cfg.OutputRoot, item.id, name, 16<<20)
		if err != nil || !bytes.Equal(current, item.data) {
			return &sub2api.AuthorizationGateError{Reason: "authorization_changed"}
		}
	}
	return nil
}
