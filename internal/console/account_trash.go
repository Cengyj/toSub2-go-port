package console

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"tosub2-go-port/internal/accountinsight"
	"tosub2-go-port/internal/accountquality"
	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/recyclebin"
)

func (s *Server) initializeRecycleBin() {
	s.trashActionGate = make(chan struct{}, 1)
	// Install resource hooks even if policy storage is unavailable. A damaged
	// settings file must never make a lower-level deletion skip private data.
	s.jobs.SetTrashPauseHook(func(ctx context.Context, selected []*jobs.Job) error {
		if err := s.pauseTrashQuality(ctx, selected); err != nil {
			return err
		}
		return s.pauseTrashSMS(ctx, selected)
	})
	s.jobs.SetTrashPurgeHook(func(ctx context.Context, entry *jobs.TrashEntry) error {
		if s.insights == nil {
			return jobs.ErrTrashInsightCleanup
		}
		ids := make([]string, 0, len(entry.Jobs))
		for _, j := range entry.Jobs {
			ids = append(ids, j.ID)
		}
		if err := s.insights.PurgeAccountIDs(ctx, ids); err != nil {
			if errors.Is(err, accountinsight.ErrCacheBusy) {
				return jobs.TrashCleanupFailure(jobs.ErrTrashBusy, err)
			}
			return jobs.TrashCleanupFailure(jobs.ErrTrashInsightCleanup, err)
		}
		if s.quality == nil || s.quality.Service == nil {
			return jobs.ErrTrashQualityCleanup
		}
		if err := s.quality.Service.PurgeAccountIDs(ctx, ids); err != nil {
			if errors.Is(err, accountquality.ErrCacheBusy) {
				return jobs.TrashCleanupFailure(jobs.ErrTrashBusy, err)
			}
			return jobs.TrashCleanupFailure(jobs.ErrTrashQualityCleanup, err)
		}
		if err := s.purgeTrashSMS(ctx, entry); err != nil {
			if errors.Is(err, errSMSTrashBusy) {
				return jobs.TrashCleanupFailure(jobs.ErrTrashBusy, err)
			}
			return jobs.TrashCleanupFailure(jobs.ErrTrashSMSCleanup, err)
		}
		return nil
	})
	service, err := recyclebin.New(s.cfg.OutputRoot, s.jobs)
	if err != nil {
		if s.logger != nil {
			s.logger.Error("recycle bin storage unavailable; deletion disabled")
		}
		return
	}
	s.recycleBin = service
}

// The manager holds the same-email deletion fence while invoking this hook,
// without holding its mutex. Cancel every retained ID before moving anything,
// and keep ownership until an in-flight probe has actually returned. HTTP
// admission is separately fenced by trashActionGate throughout the move.
func (s *Server) pauseTrashQuality(parent context.Context, selected []*jobs.Job) error {
	if s.quality == nil || s.quality.Service == nil || len(selected) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(parent, 3*time.Second)
	defer cancel()
	ids := make([]string, 0, len(selected))
	for _, job := range selected {
		ids = append(ids, job.ID)
	}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		for start := 0; start < len(ids); start += 1000 {
			end := min(start+1000, len(ids))
			if _, err := s.quality.Service.Cancel(ctx, ids[start:end]); err != nil {
				return errors.Join(errors.New("模型检测尚未退出，账号未移入回收站"), err)
			}
		}
		snapshots, _ := s.quality.Service.Snapshots(ids)
		busy := false
		for _, snapshot := range snapshots {
			if snapshot.State == "active" || snapshot.State == "queued" {
				busy = true
				break
			}
		}
		if !busy {
			return nil
		}
		select {
		case <-ctx.Done():
			return errors.Join(errors.New("模型检测尚未退出，账号未移入回收站"), ctx.Err())
		case <-ticker.C:
		}
	}
}
func (s *Server) StartRecycleBin(ctx context.Context) {
	if s.recycleBin != nil {
		s.recycleBin.Start(ctx)
	}
}

func (s *Server) accountTrashAPI(w http.ResponseWriter, r *http.Request) {
	if s.recycleBin == nil {
		writeError(w, http.StatusServiceUnavailable, "回收站存储不可用，删除已暂停；请检查本地存储后重启")
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/account-trash")
	switch {
	case path == "" && r.Method == http.MethodGet:
		policy := s.recycleBin.Settings()
		entries := []map[string]any{}
		for _, entry := range s.jobs.ListTrash() {
			history := []map[string]any{}
			for _, j := range entry.Jobs {
				history = append(history, map[string]any{"id": j.ID, "status": j.Status, "operation": j.Operation, "lastOperationType": j.LastOperationType, "createdAt": j.CreatedAt, "updatedAt": j.UpdatedAt, "hasLogs": len(j.Logs)+len(j.LogEntries) > 0})
			}
			dto := map[string]any{"id": entry.ID, "email": entry.Email, "deletedAt": entry.DeletedAt, "state": entry.State, "recordCount": len(history), "jobs": history, "canRestore": entry.State == jobs.TrashStateTrashed}
			if entry.PurgeStartedAt != nil {
				dto["purgeStartedAt"] = entry.PurgeStartedAt
			}
			if entry.LastError != "" {
				dto["lastError"] = entry.LastError
			}
			if at := recyclebin.ExpiresAt(entry, policy.RetentionDays); at != nil {
				dto["expiresAt"] = at
			}
			entries = append(entries, dto)
		}
		writeJSON(w, 200, map[string]any{"entries": entries, "settings": policy, "status": s.recycleBin.Status()})
	case path == "/settings" && r.Method == http.MethodGet:
		writeJSON(w, 200, s.recycleBin.Settings())
	case path == "/settings" && r.Method == http.MethodPut:
		var req recyclebin.Settings
		if !decodeJSON(w, r, &req) {
			return
		}
		value, err := s.recycleBin.Update(r.Context(), req)
		if err != nil {
			code := http.StatusBadRequest
			if errors.Is(err, recyclebin.ErrConflict) {
				code = http.StatusConflict
			}
			if errors.Is(err, recyclebin.ErrClosed) || errors.Is(err, recyclebin.ErrPersistence) || errors.Is(err, recyclebin.ErrUncertain) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				code = http.StatusServiceUnavailable
			}
			out := map[string]any{"settings": value, "error": err.Error(), "outcome": "failed"}
			if errors.Is(err, recyclebin.ErrUncertain) {
				out["uncertain"] = true
				out["outcome"] = "unknown"
			}
			writeJSON(w, code, out)
			return
		}
		writeJSON(w, 200, value)
	case (path == "/move" || path == "/restore" || path == "/purge") && r.Method == http.MethodPost:
		var req struct {
			IDs              []string
			ConfirmPermanent bool
		}
		if !decodeJSON(w, r, &req) {
			return
		}
		if len(req.IDs) == 0 || len(req.IDs) > 1000 {
			writeError(w, 400, "请选择 1–1000 个账号")
			return
		}
		seen := map[string]bool{}
		ids := make([]string, 0, len(req.IDs))
		for _, id := range req.IDs {
			if strings.TrimSpace(id) == "" {
				writeError(w, 400, "账号标识不能为空")
				return
			}
			if !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
		if path == "/move" {
			s.moveAccountsToTrash(w, r, ids)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		if !s.acquireTrashAction(w, ctx) {
			return
		}
		defer func() { <-s.trashActionGate }()
		if path == "/purge" && !req.ConfirmPermanent {
			writeError(w, 400, "彻底删除必须明确确认，无法撤销")
			return
		}
		before := map[string][]string{}
		for _, id := range ids {
			entry, ok := s.jobs.GetTrash(id)
			if !ok {
				writeError(w, 404, "回收记录已变化，请刷新后核对")
				return
			}
			for _, j := range entry.Jobs {
				before[id] = append(before[id], j.ID)
			}
		}
		var count int
		var err error
		key := "restored"
		if path == "/restore" {
			count, err = s.jobs.RestoreTrash(ctx, ids)
		} else {
			key = "purged"
			count, err = s.jobs.PurgeTrash(ctx, ids)
		}
		affected := []string{}
		purgePending := false
		for id, jobIDs := range before {
			if entry, ok := s.jobs.GetTrash(id); !ok {
				affected = append(affected, jobIDs...)
			} else if path == "/purge" && entry.State == jobs.TrashStatePurging {
				purgePending = true
			}
		}
		if path == "/restore" {
			s.resumeTrashSMS(affected)
		}
		s.writeTrashOutcome(w, key, count, affected, err, purgePending)
	case strings.HasSuffix(path, "/logs") && r.Method == http.MethodGet:
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/"), "/logs")
		entry, ok := s.jobs.GetTrash(id)
		if !ok {
			writeError(w, 404, "回收记录不存在")
			return
		}
		jobID := r.URL.Query().Get("jobId")
		for _, j := range entry.Jobs {
			if j.ID == jobID {
				s.sendRetainedLogs(w, r, j)
				return
			}
		}
		writeError(w, 404, "历史记录不属于该回收账号")
	default:
		writeError(w, 404, "unknown recycle bin route")
	}
}

func (s *Server) moveAccountsToTrash(w http.ResponseWriter, r *http.Request, ids []string) {
	if s.recycleBin == nil {
		writeError(w, 503, "回收站存储不可用，未删除任何账号")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if !s.acquireTrashAction(w, ctx) {
		return
	}
	defer func() { <-s.trashActionGate }()
	emails := map[string]bool{}
	for _, id := range ids {
		job, ok := s.jobs.Get(id)
		if !ok {
			writeError(w, 404, "账号状态已变化，请刷新后核对")
			return
		}
		emails[strings.ToLower(strings.TrimSpace(job.Email))] = true
	}
	targetIDs := []string{}
	for _, job := range s.jobs.List() {
		if emails[strings.ToLower(strings.TrimSpace(job.Email))] {
			targetIDs = append(targetIDs, job.ID)
		}
	}
	// Keep the SMS admission gate until the jobs transaction has completed,
	// including failure paths. This only unlocks, never resumes remote work.
	defer func() {
		// A same-email record may have been imported between the initial
		// snapshot and acquiring the manager's account gate. Include it too.
		all := append([]string(nil), targetIDs...)
		for _, j := range s.jobs.List() {
			if emails[strings.ToLower(strings.TrimSpace(j.Email))] {
				all = append(all, j.ID)
			}
		}
		for _, entry := range s.jobs.ListTrash() {
			if emails[strings.ToLower(strings.TrimSpace(entry.Email))] {
				for _, j := range entry.Jobs {
					all = append(all, j.ID)
				}
			}
		}
		s.resumeTrashSMS(all)
	}()
	count, err := s.jobs.TrashAccounts(ctx, ids)
	affected := []string{}
	for _, entry := range s.jobs.ListTrash() {
		if emails[strings.ToLower(strings.TrimSpace(entry.Email))] {
			for _, job := range entry.Jobs {
				affected = append(affected, job.ID)
				if s.insights != nil {
					s.insights.Cancel(job.ID)
				}
			}
		}
	}
	s.writeTrashOutcome(w, "moved", count, affected, err)
}

// Serialize only explicit console recycle actions, including SMS admission
// fence release. Background cleanup retains the jobs-level account gate.
func (s *Server) acquireTrashAction(w http.ResponseWriter, ctx context.Context) bool {
	select {
	case s.trashActionGate <- struct{}{}:
		if ctx.Err() == nil {
			return true
		}
		<-s.trashActionGate
	case <-ctx.Done():
	}
	writeJSON(w, 503, map[string]any{"outcome": "failed", "error": "等待回收站操作结束超时，本次请求尚未执行"})
	return false
}
func (s *Server) writeTrashOutcome(w http.ResponseWriter, key string, count int, ids []string, err error, pending ...bool) {
	sort.Strings(ids)
	out := map[string]any{key: count, "affectedIds": ids, "outcome": "completed"}
	// Keep the old delete-batch receipt usable by already-open console pages.
	if key == "moved" {
		out["deleted"] = count
	}
	if err == nil {
		writeJSON(w, 200, out)
		return
	}
	code := http.StatusConflict
	out["outcome"] = "failed"
	if count > 0 || len(ids) > 0 {
		out["outcome"] = "partial"
	}
	message := "回收站操作未完成，数据仍有保留；请刷新核对状态后处理"
	if key == "purged" && len(pending) > 0 && pending[0] {
		out["outcome"] = "partial"
		out["purgePending"] = true
		message = "永久清理已开始但尚未完成，相关账号不可恢复；请检查存储和凭据服务后重试完成清理"
	}
	switch {
	case errors.Is(err, jobs.ErrCancellationPersistence):
		code = 503
		out["outcome"] = "unknown"
		out["uncertain"] = true
		message = jobs.ErrCancellationPersistence.Error()
	case errors.Is(err, jobs.ErrTrashUncertain):
		code = 503
		out["outcome"] = "unknown"
		out["uncertain"] = true
		message = jobs.ErrTrashUncertain.Error()
	case errors.Is(err, jobs.ErrTrashPersistence):
		code = 503
		message = jobs.ErrTrashPersistence.Error()
	case errors.Is(err, jobs.ErrTrashPurging):
		message = jobs.ErrTrashPurging.Error()
	case errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled):
		code = 503
		out["outcome"] = "unknown"
		message = "操作响应已中断，请刷新核对；不要自动重复提交"
	default:
		for _, stage := range []error{jobs.ErrTrashBusy, jobs.ErrTrashNotFound, jobs.ErrTrashConflict,
			jobs.ErrTrashInsightCleanup, jobs.ErrTrashQualityCleanup, jobs.ErrTrashSMSCleanup,
			jobs.ErrTrashInspection, jobs.ErrTrashDirectoryCleanup, jobs.ErrTrashCredentialCleanup, jobs.ErrTrashLocalCleanup} {
			if errors.Is(err, stage) {
				message = stage.Error()
				if out["purgePending"] == true {
					message += "；永久清理已开始，相关账号不可恢复"
				}
				break
			}
		}
	}
	out["error"] = message
	writeJSON(w, code, out)
}
func (s *Server) sendRetainedLogs(w http.ResponseWriter, r *http.Request, j *jobs.Job) {
	page, err := s.jobs.ReadLogPage(r.Context(), j, r.URL.Query().Get("before"))
	if err != nil {
		writeError(w, 503, "完整日志暂不可读取，历史文件仍保留；请检查本地存储后重试")
		return
	}
	writeJSON(w, 200, page)
}
