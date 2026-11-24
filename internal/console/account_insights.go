package console

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"tosub2-go-port/internal/accountinsight"
	"tosub2-go-port/internal/application"
)

func (s *Server) initializeAccountInsights() {
	s.insightController = application.NewAccountInsightController(s.cfg, s.jobs)
	service, err := accountinsight.New(s.cfg.OutputRoot, s.insightController.Provider(func() string {
		s.monitorMu.Lock()
		defer s.monitorMu.Unlock()
		return s.monitorConfig.OpenAIRequestTimezone
	}))
	if err != nil {
		if s.logger != nil {
			s.logger.Warn("account query storage unavailable")
		}
		return
	}
	s.insights = service
}
func (s *Server) StartAccountInsights(ctx context.Context) {
	if s.insights != nil {
		s.insights.Start(ctx)
	}
}
func (s *Server) accountInsightsAPI(w http.ResponseWriter, r *http.Request) {
	if s.insights == nil {
		writeError(w, http.StatusServiceUnavailable, "账号查询存储不可用")
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/account-insights")
	switch {
	case path == "/settings" && r.Method == http.MethodGet:
		writeJSON(w, 200, s.insights.Settings())
	case path == "/settings" && r.Method == http.MethodPut:
		var c accountinsight.Settings
		if !decodeJSON(w, r, &c) {
			return
		}
		settings, err := s.insights.Update(r.Context(), c)
		if err != nil {
			status := 400
			outcome := "failed"
			uncertain := errors.Is(err, accountinsight.ErrPersistenceUncertain)
			if errors.Is(err, accountinsight.ErrConflict) {
				status = 409
			}
			if uncertain || errors.Is(err, accountinsight.ErrPersistence) || errors.Is(err, accountinsight.ErrClosed) {
				status = 503
			}
			if uncertain {
				outcome = "unknown"
			}
			writeJSON(w, status, map[string]any{"error": err.Error(), "settings": settings, "uncertain": uncertain, "outcome": outcome})
			return
		}
		writeJSON(w, 200, settings)
	case path == "/status" && r.Method == http.MethodGet:
		query, err := url.ParseQuery(r.URL.RawQuery)
		if err != nil {
			writeError(w, http.StatusBadRequest, "查询账号列表格式无效")
			return
		}
		values, selected := query["ids"]
		if !selected {
			writeJSON(w, http.StatusOK, s.insights.Status())
			return
		}
		if len(values) != 1 {
			writeError(w, http.StatusBadRequest, "查询账号列表只能指定一次")
			return
		}
		ids := strings.Split(values[0], ",")
		if len(ids) > 1000 {
			writeError(w, http.StatusBadRequest, "一次最多读取1000个账号的刷新状态")
			return
		}
		for _, id := range ids {
			valid := len(id) > 0 && len(id) <= 128 && id != "." && id != ".."
			for _, r := range id {
				if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.') {
					valid = false
					break
				}
			}
			if !valid {
				writeError(w, http.StatusBadRequest, "查询账号列表包含无效标识")
				return
			}
		}
		writeJSON(w, http.StatusOK, s.insights.StatusForSelection(ids))
	case path == "/refresh" && r.Method == http.MethodPost:
		var body struct {
			IDs json.RawMessage `json:"ids"`
			All bool            `json:"all"`
		}
		if !decodeJSON(w, r, &body) {
			return
		}
		if body.All && body.IDs != nil {
			writeError(w, 400, "查询全部账号时不能同时指定账号列表")
			return
		}
		var receipt accountinsight.Receipt
		var err error
		if body.All {
			receipt, err = s.insights.RefreshAll()
		} else {
			var ids []string
			if body.IDs != nil && json.Unmarshal(body.IDs, &ids) != nil {
				writeError(w, 400, "账号列表格式无效")
				return
			}
			receipt, err = s.insights.Refresh(ids)
		}
		if err != nil {
			status := 400
			if errors.Is(err, accountinsight.ErrDisabled) || errors.Is(err, accountinsight.ErrClosed) {
				status = 409
			}
			writeError(w, status, err.Error())
			return
		}
		writeJSON(w, 202, receipt)
	case strings.HasPrefix(path, "/queries/") && strings.HasSuffix(path, "/cancel") && r.Method == http.MethodPost:
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/queries/"), "/cancel")
		if id == "" || strings.Contains(id, "/") {
			writeError(w, 400, "invalid query identifier")
			return
		}
		writeJSON(w, 200, map[string]bool{"cancelled": s.insights.Cancel(id)})
	case path == "" && r.Method == http.MethodGet:
		ids := strings.Split(r.URL.Query().Get("ids"), ",")
		if len(ids) > 100 {
			writeError(w, 400, "一次最多读取100个账号")
			return
		}
		out, revision := s.insights.Snapshots(ids)
		writeJSON(w, 200, map[string]any{"snapshots": out, "revision": revision})
	case strings.HasPrefix(path, "/") && len(path) > 1 && !strings.Contains(path[1:], "/") && r.Method == http.MethodGet:
		v, ok := s.insights.Snapshot(path[1:])
		if !ok {
			writeError(w, 404, "账号不存在")
			return
		}
		writeJSON(w, 200, v)
	default:
		writeError(w, 404, "unknown account query route")
	}
}
