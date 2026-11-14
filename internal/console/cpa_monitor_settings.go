package console

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
)

type cpaMonitorSettingsRequest struct {
	Revision          *int64    `json:"revision"`
	Enabled           *bool     `json:"enabled"`
	ExcludedPlanTypes *[]string `json:"excludedPlanTypes"`
	IntervalMinutes   *int      `json:"intervalMinutes"`
	CooldownMinutes   *int      `json:"cooldownMinutes"`
}

func (r *cpaMonitorSettingsRequest) UnmarshalJSON(data []byte) error {
	// This endpoint never accepts destination, credential or upload parameters.
	// Reject them instead of silently treating a misplaced service save as valid.
	type body cpaMonitorSettingsRequest
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	return decoder.Decode((*body)(r))
}

func (s *Server) saveCPAMonitorSettings(w http.ResponseWriter, r *http.Request) {
	if s.poolMonitorGate.Load() != nil {
		writeError(w, http.StatusConflict, "巡检规则已统一，请在号池监控中选择目标并保存")
		return
	}
	var req cpaMonitorSettingsRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Revision == nil || *req.Revision < 0 || req.Enabled == nil || req.ExcludedPlanTypes == nil || req.IntervalMinutes == nil || req.CooldownMinutes == nil {
		writeCPASettingsError(w, http.StatusBadRequest, errors.New("CPA 巡检配置需要当前版本、开关、套餐规则及执行间隔"))
		return
	}
	s.cpaMu.Lock()
	base, revision, loadError := s.cpa.BaseURL, s.cpa.Revision, s.cpa.loadError
	hasKey := s.cpa.key != ""
	s.cpaMu.Unlock()
	if *req.Revision != revision {
		writeCPASettingsError(w, http.StatusConflict, errors.New("CPA 配置已更新，请重新读取后保存"))
		return
	}
	if loadError != "" || base != "" && !hasKey || *req.Enabled && (base == "" || !hasKey) {
		writeCPASettingsError(w, http.StatusBadRequest, errors.New("请先在服务配置中保存有效的 CPA 管理地址和管理密钥"))
		return
	}
	// commitCPASettings checks this same revision under the lock. A connection
	// edit between the snapshot and commit cannot be overwritten by these rules.
	status, err := s.commitCPASettings(r.Context(), base, "", *req.Revision, cpaSchedulingUpdate{
		MonitorEnabled: req.Enabled, ExcludedPlanTypes: req.ExcludedPlanTypes,
		IntervalMinutes: req.IntervalMinutes, CooldownMinutes: req.CooldownMinutes,
	})
	if err != nil {
		writeCPASettingsError(w, status, err)
		return
	}
	writeJSON(w, http.StatusOK, s.publicCPAMonitorState())
}
