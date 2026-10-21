package console

import (
	"context"
	"errors"
	"net/http"
	"time"

	"tosub2-go-port/internal/accountexpiry"
	"tosub2-go-port/internal/jobs"
)

// setAccountExpiry changes local display metadata without touching credentials,
// authentication, queue membership, or any remote account. Only an explicit
// empty string clears the manual override; missing/null fields are rejected.
func (s *Server) setAccountExpiry(w http.ResponseWriter, r *http.Request, id string) {
	w.Header().Set("Cache-Control", "no-store")
	var body struct {
		ExpiresAt *string `json:"expiresAt"`
		TimeZone  string  `json:"timeZone"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.ExpiresAt == nil {
		writeError(w, http.StatusBadRequest, "请填写到期时间；清除手动设置时请传入空字符串")
		return
	}
	if _, exists := s.jobs.Get(id); !exists {
		writeError(w, http.StatusNotFound, "job not found")
		return
	}
	var expires *time.Time
	if *body.ExpiresAt != "" {
		value, err := accountexpiry.ParseManual(*body.ExpiresAt, body.TimeZone, time.Now())
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		expires = &value
	}
	updated, err := s.jobs.SetManualExpiryContext(r.Context(), id, expires)
	if err != nil {
		if errors.Is(err, jobs.ErrAdmissionUncertain) {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "到期时间已写入，但未能确认持久保存；请重新读取并核实，避免重复提交", "uncertain": true})
			return
		}
		message := "到期时间保存失败，请检查本地存储并重试"
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			message = "请求已取消或超时，请重新读取到期时间后核实"
		}
		writeError(w, http.StatusConflict, message)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"job": s.publicJob(updated)})
}
