package console

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"tosub2-go-port/internal/application"
	"tosub2-go-port/internal/authmaterial"
	"tosub2-go-port/internal/cpa"
	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/logging"
	"tosub2-go-port/internal/sub2api"
)

type cpaPreparedUpload struct {
	id      string
	data    []byte
	account map[string]any
	file    cpa.AuthFile
}

func (s *Server) prepareCPAAccount(ctx context.Context, job *jobs.Job) (cpaPreparedUpload, error) {
	if err := s.jobs.CheckTOTPExportContext(ctx, job.ID); err != nil {
		return cpaPreparedUpload{}, err
	}
	if isActiveStatus(job.Status) || job.Status == jobs.StatusQueued || job.TOTPWorkflowStatus == "running" || job.TOTPWorkflowStatus == "pending" || totpRecoveryBlocked(job) {
		return cpaPreparedUpload{}, errors.New("请等待账号当前流程结束后再推送授权")
	}
	if !job.ResultSaved {
		return cpaPreparedUpload{}, errors.New("选中的账号尚无已保存的授权，请先完成授权")
	}
	data, err := application.ReadCommittedAuthorization(ctx, s.cfg.OutputRoot, job.ID, 16<<20)
	if err != nil {
		return cpaPreparedUpload{}, errors.New("授权文件尚未提交、正在恢复或无法安全读取，已停止 CPA 推送")
	}
	if authmaterial.ContainsLoginSessionJSON(data) {
		return cpaPreparedUpload{}, errors.New("授权文件含已移除的登录会话材料，已停止 CPA 推送")
	}
	var document struct {
		Type     string           `json:"type"`
		Accounts []map[string]any `json:"accounts"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if decoder.Decode(&document) != nil || decoder.Decode(new(any)) != io.EOF || document.Type != "sub2api-data" || len(document.Accounts) != 1 {
		return cpaPreparedUpload{}, errors.New("授权文件格式不正确，已停止 CPA 推送")
	}
	account := document.Accounts[0]
	if sub2api.AccountEmail(account) != strings.ToLower(strings.TrimSpace(job.Email)) {
		return cpaPreparedUpload{}, errors.New("授权文件与选中账号不一致，已停止 CPA 推送")
	}
	file, err := cpa.BuildAuthFile(account)
	if err != nil {
		return cpaPreparedUpload{}, err
	}
	return cpaPreparedUpload{id: job.ID, data: data, account: account, file: file}, nil
}

func (s *Server) uploadCPA(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IDs []string `json:"ids"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	selected, ok := s.resolveSelectedJobs(w, req.IDs)
	if !ok {
		return
	}
	cfg, err := s.configuredCPA()
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	prepared := make([]cpaPreparedUpload, 0, len(selected))
	blocked := func(err error) {
		var gate *sub2api.AuthorizationGateError
		writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "error": logging.SafeError(err), "outcome": "blocked", "uncertain": false,
			"qualityBlocked": errors.As(err, &gate), "selected": len(selected), "uploaded": 0, "skipped": len(selected),
			"result": map[string]any{"success": 0, "created": 0, "updated": 0}})
	}
	// Convert the whole selection before any inference or management request.
	// One BPS/incomplete/unsafe account cannot cause a partly submitted batch.
	for _, job := range selected {
		item, err := s.prepareCPAAccount(r.Context(), job)
		if err != nil {
			blocked(err)
			return
		}
		prepared = append(prepared, item)
	}
	if s.quality == nil {
		blocked(errors.New("账号推送校验服务不可用"))
		return
	}
	ctx, check, release := s.quality.NewUploadSession(r.Context(), req.IDs)
	defer release()
	for _, item := range prepared {
		if err := check(ctx, item.account); err != nil {
			blocked(err)
			return
		}
		// Admission acquires the account and OS leases. Verify the bytes again
		// after acquiring ownership, even when the optional quality gate is off.
		current, err := application.ReadCommittedAuthorization(ctx, s.cfg.OutputRoot, item.id, 16<<20)
		if err != nil || !bytes.Equal(current, item.data) {
			blocked(errors.New("授权在推送准备期间已变化，请刷新后重新选择"))
			return
		}
	}
	client := cpa.New(cfg)
	results := make([]map[string]any, 0, len(prepared))
	confirmed, attempted, failed := 0, 0, false
	uncertain, message := false, ""
	for _, item := range prepared {
		if failed {
			results = append(results, map[string]any{"id": item.id, "outcome": "skipped"})
			continue
		}
		// A newer operation cancels this lease, and external file replacement
		// must not send a stale grant that the preflight did not authorize.
		// Recheck policy before each mutation as it may change during an
		// earlier account's request. The session retains its acquired leases.
		err = check(ctx, item.account)
		if err == nil {
			current, readErr := application.ReadCommittedAuthorization(ctx, s.cfg.OutputRoot, item.id, 16<<20)
			if readErr != nil || !bytes.Equal(current, item.data) {
				err = errors.New("账号授权已变化或推送已取消，后续账号未发送")
			} else {
				err = s.jobs.CheckTOTPExportContext(ctx, item.id)
			}
		}
		message = logging.SafeError(err)
		if err == nil {
			attempted++
			err = client.Upload(ctx, item.file)
			message = cpa.SafeError(err)
		}
		if err != nil {
			failed, uncertain = true, cpa.IsMutationOutcomeUnknown(err)
			outcome := "failed"
			if uncertain {
				outcome = "unknown"
			}
			results = append(results, map[string]any{"id": item.id, "outcome": outcome, "error": message})
			continue
		}
		confirmed++
		results = append(results, map[string]any{"id": item.id, "outcome": "succeeded"})
	}
	outcome, status := "succeeded", http.StatusOK
	if failed {
		outcome, status = "failed", http.StatusBadGateway
		if uncertain {
			outcome = "unknown"
		}
		if confirmed > 0 {
			outcome = "partial"
		}
	}
	writeJSON(w, status, map[string]any{"ok": !failed, "target": "cpa", "outcome": outcome, "uncertain": uncertain, "error": message,
		"selected": len(selected), "uploaded": confirmed, "attempted": attempted, "skipped": len(selected) - attempted,
		"result": map[string]any{"success": confirmed, "created": 0, "updated": 0}, "results": results})
}
