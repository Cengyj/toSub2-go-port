package console

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"tosub2-go-port/internal/accountquality"
	"tosub2-go-port/internal/application"
	"tosub2-go-port/internal/sub2api"
)

func (s *Server) initializeAccountQuality() {
	controller, err := application.NewAccountQualityController(s.cfg, s.jobs)
	if err != nil {
		s.jobs.SetAuthorizationSyncCheck(func(context.Context, string, map[string]any) error {
			return &sub2api.AuthorizationGateError{Reason: "settings"}
		})
		if s.logger != nil {
			s.logger.Warn("model quality storage unavailable; upload preflight cannot be confirmed")
		}
		return
	}
	s.quality = controller
	s.jobs.SetAuthorizationSyncCheck(controller.WorkflowUploadCheck)
}

func (s *Server) StartAccountQuality(ctx context.Context) {
	if s.quality != nil {
		s.quality.Service.Start(ctx)
		s.quality.StartManualProbes(ctx)
	}
}

func qualityAPIStatus(err error) int {
	switch {
	case errors.Is(err, accountquality.ErrConflict), errors.Is(err, accountquality.ErrDisabled), errors.Is(err, accountquality.ErrBusy), errors.Is(err, accountquality.ErrSuperseded):
		return http.StatusConflict
	case errors.Is(err, accountquality.ErrClosed), errors.Is(err, accountquality.ErrSettingsUnavailable), errors.Is(err, accountquality.ErrPersistence), errors.Is(err, accountquality.ErrPersistenceUncertain), errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return http.StatusServiceUnavailable
	default:
		return http.StatusBadRequest
	}
}

func qualityRequestIDs(raw []string) ([]string, bool) {
	if len(raw) > 1000 {
		return nil, false
	}
	ids := []string{}
	seen := map[string]bool{}
	for _, id := range raw {
		if strings.TrimSpace(id) != id || id == "" || len(id) > 128 || strings.ContainsAny(id, "/\\\r\n\x00") {
			return nil, false
		}
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	return ids, true
}

func (s *Server) accountQualityAPI(w http.ResponseWriter, r *http.Request) {
	if s.quality == nil || s.quality.Service == nil {
		writeError(w, http.StatusServiceUnavailable, "模型检测存储不可用，上传前检查已暂停，请核实本地存储")
		return
	}
	service := s.quality.Service
	path := strings.TrimPrefix(r.URL.Path, "/api/account-quality")
	switch {
	case path == "/models":
		s.accountQualityModelsAPI(w, r)
	case path == "/manual" || path == "/manual/cancel":
		s.accountQualityManualAPI(w, r, path)
	case path == "/settings" && r.Method == http.MethodGet:
		settings, err := service.SettingsContext(r.Context())
		if err != nil {
			writeJSON(w, qualityAPIStatus(err), map[string]any{"error": err.Error(), "settings": settings, "settingsAvailable": false})
			return
		}
		writeJSON(w, http.StatusOK, settings)
	case path == "/settings" && r.Method == http.MethodPut:
		var raw json.RawMessage
		if !decodeJSON(w, r, &raw) {
			return
		}
		req, err := accountquality.DecodeSettingsJSON(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "检测设置必须完整且各字段只能明确填写一次，请重新读取后保存")
			return
		}
		settings, err := service.Update(r.Context(), req)
		if err != nil {
			out := map[string]any{"error": err.Error(), "settings": settings, "outcome": "failed"}
			if errors.Is(err, accountquality.ErrPersistenceUncertain) {
				out["uncertain"] = true
				out["outcome"] = "unknown"
			}
			writeJSON(w, qualityAPIStatus(err), out)
			return
		}
		if !settings.Enabled {
			s.quality.CancelActiveManualProbe()
		}
		writeJSON(w, http.StatusOK, settings)
	case (path == "" || path == "/status") && r.Method == http.MethodGet:
		var raw []string
		if value := r.URL.Query().Get("ids"); value != "" {
			raw = strings.Split(value, ",")
		}
		ids, ok := qualityRequestIDs(raw)
		if !ok {
			writeError(w, http.StatusBadRequest, "检测账号标识不正确")
			return
		}
		if path == "/status" {
			writeJSON(w, http.StatusOK, s.quality.Status())
			return
		}
		snapshots, revision := s.quality.Snapshots(ids)
		writeJSON(w, http.StatusOK, map[string]any{"snapshots": snapshots, "status": s.quality.Status(), "revision": revision})
	case (path == "/refresh" || path == "/cancel") && r.Method == http.MethodPost:
		var req struct {
			IDs     []string        `json:"ids"`
			All     bool            `json:"all"`
			Options json.RawMessage `json:"options"`
		}
		if !decodeQualityProbeJSON(w, r, &req, "ids", "all", "options") {
			return
		}
		selected, optionsErr := accountquality.DecodeRunOptions(req.Options)
		if optionsErr != nil || path == "/cancel" && len(req.Options) != 0 {
			writeError(w, 400, "请选择完整有效的单次模型与推理强度")
			return
		}
		ids, ok := qualityRequestIDs(req.IDs)
		if !ok || req.All && len(ids) > 0 || !req.All && len(ids) == 0 || path == "/cancel" && req.All {
			writeError(w, http.StatusBadRequest, "请选择检测账号；全部检测不能同时指定账号")
			return
		}
		if path == "/cancel" {
			receipt, err := service.Cancel(r.Context(), ids)
			if err != nil {
				writeError(w, qualityAPIStatus(err), err.Error())
				return
			}
			writeJSON(w, http.StatusOK, receipt)
			return
		}
		// Admission and recycle actions share the console fence. Once a move
		// cancels old checks, another request cannot queue them again before
		// the account has been hidden or restored.
		if s.trashActionGate != nil {
			select {
			case s.trashActionGate <- struct{}{}:
				defer func() { <-s.trashActionGate }()
			case <-r.Context().Done():
				writeError(w, http.StatusServiceUnavailable, "等待账号回收操作结束超时，未提交检测")
				return
			}
		}
		receipt, err := service.RefreshWithOptions(r.Context(), ids, req.All, selected)
		if err != nil {
			writeError(w, qualityAPIStatus(err), err.Error())
			return
		}
		writeJSON(w, http.StatusAccepted, receipt)
	default:
		writeError(w, http.StatusNotFound, "unknown model quality route")
	}
}
