package console

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"tosub2-go-port/internal/privatefile"
	"tosub2-go-port/internal/store"
	"tosub2-go-port/internal/sub2api"
)

// This file is the only authority for automatic monitoring rules. The two
// integrations retain connection settings and their own check history.
type poolMonitorSettings struct {
	Version           int      `json:"version"`
	Revision          int64    `json:"revision"`
	Target            string   `json:"target"`
	Enabled           bool     `json:"enabled"`
	ExcludedPlanTypes []string `json:"excludedPlanTypes"`
	IntervalMinutes   int      `json:"intervalMinutes"`
	CooldownMinutes   int      `json:"cooldownMinutes"`
}

var (
	errPoolMonitorInactive = errors.New("所选服务尚未启用或连接不可用，请先保存巡检配置")
	errPoolMonitorBusy     = errors.New("巡检正在进行，请稍后查看结果")
)

type poolMonitorGate struct {
	Target  string
	Enabled bool
}

// An existing authoritative file must contain the complete schema. Defaults
// are reserved for a missing-file migration, never for partial/corrupt JSON.
func (p *poolMonitorSettings) UnmarshalJSON(data []byte) error {
	var value struct {
		Version           *int      `json:"version"`
		Revision          *int64    `json:"revision"`
		Target            *string   `json:"target"`
		Enabled           *bool     `json:"enabled"`
		ExcludedPlanTypes *[]string `json:"excludedPlanTypes"`
		IntervalMinutes   *int      `json:"intervalMinutes"`
		CooldownMinutes   *int      `json:"cooldownMinutes"`
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&value); err != nil {
		return err
	}
	if value.Version == nil || value.Revision == nil || value.Target == nil || value.Enabled == nil || value.ExcludedPlanTypes == nil || value.IntervalMinutes == nil || value.CooldownMinutes == nil {
		return errors.New("incomplete shared pool monitor configuration")
	}
	*p = poolMonitorSettings{Version: *value.Version, Revision: *value.Revision, Target: *value.Target, Enabled: *value.Enabled, ExcludedPlanTypes: *value.ExcludedPlanTypes, IntervalMinutes: *value.IntervalMinutes, CooldownMinutes: *value.CooldownMinutes}
	return nil
}

func (p poolMonitorSettings) clone() poolMonitorSettings {
	p.ExcludedPlanTypes = append([]string{}, p.ExcludedPlanTypes...)
	return p
}
func (p *poolMonitorSettings) normalize() error {
	if p.Version != 1 || p.Revision < 0 || p.Revision >= 1<<53-1 || p.Target != "sub2api" && p.Target != "cpa" {
		return errors.New("号池监控配置无效，请重新读取并保存")
	}
	options := cpaMonitorOptions{ExcludedPlanTypes: p.ExcludedPlanTypes, IntervalMinutes: p.IntervalMinutes, CooldownMinutes: p.CooldownMinutes}
	if err := options.normalize(); err != nil {
		return errors.New("巡检间隔和重试冷却须为 1 到 1440 之间的整数分钟，排除套餐须为有效选项")
	}
	p.ExcludedPlanTypes = options.ExcludedPlanTypes
	return nil
}
func (s *Server) poolMonitorFile() string {
	return filepath.Join(s.cfg.OutputRoot, "pool-monitor.json")
}
func (s *Server) poolMonitorAllows(target string) bool {
	gate := s.poolMonitorGate.Load()
	// A nil gate is only used by standalone integration fixtures. NewServer
	// installs a closed gate before loading either integration or its job policy.
	return gate == nil || gate.Enabled && gate.Target == target
}
func (s *Server) initializePoolMonitor() {
	s.poolMonitorError, s.poolMonitorUncertain = "", false
	p := poolMonitorSettings{Version: 1, Target: "sub2api", ExcludedPlanTypes: []string{}, IntervalMinutes: 5, CooldownMinutes: 5}
	var saved poolMonitorSettings
	err := (&store.Store{Root: s.cfg.OutputRoot}).ReadJSON(context.Background(), "pool-monitor.json", &saved)
	if err == nil {
		p = saved
	}
	if s.cfg.OutputRoot == "" || errors.Is(err, os.ErrNotExist) {
		// Prefer the sole previously enabled integration. If both were enabled,
		// retain Sub2API as the deterministic migration target and stop CPA.
		mc := s.monitor.Configuration()
		p.Enabled, p.ExcludedPlanTypes = mc.Enabled, append([]string{}, mc.ExcludedPlanTypes...)
		p.IntervalMinutes, p.CooldownMinutes = int(monitorPublicMinutes(mc.Interval)), int(monitorPublicMinutes(mc.Cooldown))
		if s.cpa.MonitorEnabled && !mc.Enabled {
			p.Target, p.Enabled = "cpa", true
			p.ExcludedPlanTypes = append([]string{}, s.cpa.ExcludedPlanTypes...)
			p.IntervalMinutes, p.CooldownMinutes = s.cpa.IntervalMinutes, s.cpa.CooldownMinutes
		}
		// Legacy environment durations allowed seconds, zero cooldown and
		// arbitrarily long timers; the shared editor uses integer minutes.
		p.IntervalMinutes = max(1, min(1440, p.IntervalMinutes))
		p.CooldownMinutes = max(1, min(1440, p.CooldownMinutes))
		err = p.normalize()
		if err == nil && s.cfg.OutputRoot != "" {
			err = s.writePoolMonitor(context.Background(), p)
		}
	} else if err == nil {
		err = p.normalize()
	}
	if err != nil {
		p = poolMonitorSettings{Version: 1, Target: "sub2api", ExcludedPlanTypes: []string{}, IntervalMinutes: 5, CooldownMinutes: 5}
		s.poolMonitorError = "号池监控配置无法确认，自动巡检已暂停；请核对后重新保存"
		s.poolMonitorUncertain = errors.Is(err, privatefile.ErrDurabilityUncertain)
	}
	s.poolMonitor = p
	_ = s.applyPoolMonitorLocked()
}
func (s *Server) writePoolMonitor(ctx context.Context, p poolMonitorSettings) error {
	if s.cfg.OutputRoot == "" {
		return errors.New("missing output root")
	}
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	write := s.poolMonitorWrite
	if write == nil {
		write = privatefile.Write
	}
	return write(ctx, s.poolMonitorFile(), append(data, '\n'))
}

// poolMonitorMu serializes policy commits and service connection commits.
// Admission closes before either engine changes, so no intermediate policy
// permits both destinations or a stale automatic upload.
func (s *Server) applyPoolMonitorLocked() error {
	s.monitorMu.Lock()
	defer s.monitorMu.Unlock()
	s.cpaMu.Lock()
	defer s.cpaMu.Unlock()
	if s.monitor.Running() || s.monitorCheckActive || s.cpaMonitor.running {
		return errors.New("巡检正在进行，请等待结束后保存配置")
	}
	s.poolMonitorGate.Store(&poolMonitorGate{})
	s.updateAutoRepairPolicy()
	s.updateCPAAutoRepairPolicyLocked()
	p := s.poolMonitor
	enabled := p.Enabled && s.poolMonitorError == "" && !s.poolMonitorUncertain
	mc := s.monitor.Configuration()
	cfg := s.monitorConfig
	cfg.ExcludedPlanTypes = append([]string{}, p.ExcludedPlanTypes...)
	mc.Enabled = enabled && p.Target == "sub2api" && cfg.BaseURL != "" && cfg.APIKey != "" && !s.monitorUncertain
	mc.ExcludedPlanTypes = append([]string{}, p.ExcludedPlanTypes...)
	mc.Interval, mc.Cooldown = time.Duration(p.IntervalMinutes)*time.Minute, time.Duration(p.CooldownMinutes)*time.Minute
	mc.CooldownConfigured = true
	if err := s.monitor.ConfigureRuntime(sub2api.New(cfg), mc, s.monitorHooks(cfg)); err != nil {
		return err
	}
	s.monitorConfig = cfg
	s.cpa.cpaMonitorOptions = cpaMonitorOptions{MonitorEnabled: enabled && p.Target == "cpa", ExcludedPlanTypes: append([]string{}, p.ExcludedPlanTypes...), IntervalMinutes: p.IntervalMinutes, CooldownMinutes: p.CooldownMinutes}
	s.monitorNextCheckAt, s.cpaMonitor.nextCheckAt = nil, nil
	s.poolMonitorGate.Store(&poolMonitorGate{Target: p.Target, Enabled: enabled})
	s.updateAutoRepairPolicy()
	s.updateCPAAutoRepairPolicyLocked()
	s.jobs.InvalidateQueuedAutoRepairs()
	select {
	case s.monitorWake <- struct{}{}:
	default:
	}
	select {
	case s.cpaMonitor.wake <- struct{}{}:
	default:
	}
	return nil
}
func (s *Server) publicPoolMonitorState() map[string]any {
	s.poolMonitorMu.Lock()
	defer s.poolMonitorMu.Unlock()
	return s.publicPoolMonitorStateLocked()
}
func (s *Server) publicPoolMonitorStateLocked() map[string]any {
	p := s.poolMonitor.clone()
	state := s.publicMonitorState()
	cpaState := s.publicCPAMonitorState()
	sub2Configured := state["configured"]
	if p.Target == "cpa" {
		state = cpaState
	}
	state["target"], state["revision"], state["enabled"] = p.Target, p.Revision, p.Enabled
	state["excludedPlanTypes"], state["intervalMinutes"], state["cooldownMinutes"] = p.ExcludedPlanTypes, p.IntervalMinutes, p.CooldownMinutes
	state["cpaRevision"] = cpaState["revision"]
	state["targets"] = map[string]any{"sub2api": map[string]any{"configured": sub2Configured}, "cpa": map[string]any{"configured": cpaState["configured"]}}
	if _, ok := state["configurationError"]; !ok {
		state["configurationError"] = ""
	}
	if s.poolMonitorError != "" {
		state["configurationError"] = s.poolMonitorError
		state["nextCheckAt"] = nil
	}
	if s.poolMonitorChecking {
		state["running"] = true
	}
	state["uncertain"] = s.poolMonitorUncertain || state["uncertain"] == true
	return state
}

type poolMonitorRequest struct {
	Revision          *int64    `json:"revision"`
	Target            *string   `json:"target"`
	Enabled           *bool     `json:"enabled"`
	ExcludedPlanTypes *[]string `json:"excludedPlanTypes"`
	IntervalMinutes   *int      `json:"intervalMinutes"`
	CooldownMinutes   *int      `json:"cooldownMinutes"`
}

func (p *poolMonitorRequest) UnmarshalJSON(data []byte) error {
	type body poolMonitorRequest
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	return d.Decode((*body)(p))
}
func (s *Server) poolMonitorAPI(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/api/pool-monitor":
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, http.StatusOK, s.publicPoolMonitorState())
		case http.MethodPost:
			s.savePoolMonitor(w, r)
		default:
			w.Header().Set("Allow", "GET, POST")
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}
	case "/api/pool-monitor/check":
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		s.checkPoolMonitor(w, r)
	default:
		writeError(w, http.StatusNotFound, "unknown pool monitor route")
	}
}
func (s *Server) savePoolMonitor(w http.ResponseWriter, r *http.Request) {
	var req poolMonitorRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Revision == nil || req.Target == nil || req.Enabled == nil || req.ExcludedPlanTypes == nil || req.IntervalMinutes == nil || req.CooldownMinutes == nil {
		writeError(w, http.StatusBadRequest, "号池监控需要当前版本、目标、开关、套餐规则及执行间隔")
		return
	}
	s.poolMonitorMu.Lock()
	defer s.poolMonitorMu.Unlock()
	fail := func(status int, msg string, unknown bool) {
		state := s.publicPoolMonitorStateLocked()
		state["error"], state["outcome"], state["uncertain"] = msg, "failed", unknown
		if unknown {
			state["outcome"] = "unknown"
		}
		writeJSON(w, status, state)
	}
	if s.poolMonitorChecking {
		fail(http.StatusConflict, "巡检正在进行，请等待结束后保存配置", false)
		return
	}
	if *req.Revision != s.poolMonitor.Revision {
		fail(http.StatusConflict, "号池监控配置已更新，请重新读取后保存", false)
		return
	}
	p := poolMonitorSettings{Version: 1, Revision: *req.Revision + 1, Target: *req.Target, Enabled: *req.Enabled, ExcludedPlanTypes: *req.ExcludedPlanTypes, IntervalMinutes: *req.IntervalMinutes, CooldownMinutes: *req.CooldownMinutes}
	if err := p.normalize(); err != nil {
		fail(http.StatusBadRequest, err.Error(), false)
		return
	}
	previousGate := s.poolMonitorGate.Load()
	s.poolMonitorGate.Store(&poolMonitorGate{})
	s.monitorMu.Lock()
	s.cpaMu.Lock()
	running := s.monitor.Running() || s.monitorCheckActive || s.cpaMonitor.running
	configured := s.monitorConfig.BaseURL != "" && s.monitorConfig.APIKey != "" && !s.monitorUncertain
	if p.Target == "cpa" {
		configured = s.cpaConfiguredLocked()
	}
	s.cpaMu.Unlock()
	s.monitorMu.Unlock()
	if running {
		s.poolMonitorGate.Store(previousGate)
		fail(http.StatusConflict, "巡检正在进行，请等待结束后保存配置", false)
		return
	}
	if p.Enabled && !configured {
		s.poolMonitorGate.Store(previousGate)
		fail(http.StatusBadRequest, "请先保存所选服务的管理地址和管理密钥", false)
		return
	}
	// Close automatic admission while replacing the single policy file. Checks
	// synchronize through the integration locks and recheck this gate.
	s.monitorMu.Lock()
	s.updateAutoRepairPolicy()
	s.monitorMu.Unlock()
	s.cpaMu.Lock()
	s.updateCPAAutoRepairPolicyLocked()
	s.cpaMu.Unlock()
	err := s.writePoolMonitor(r.Context(), p)
	uncertain := errors.Is(err, privatefile.ErrDurabilityUncertain)
	if err != nil && !uncertain {
		s.poolMonitorGate.Store(previousGate)
		s.monitorMu.Lock()
		s.updateAutoRepairPolicy()
		s.monitorMu.Unlock()
		s.cpaMu.Lock()
		s.updateCPAAutoRepairPolicyLocked()
		s.cpaMu.Unlock()
		fail(http.StatusInternalServerError, "号池监控保存失败，原配置保留", false)
		return
	}
	s.poolMonitor = p
	s.poolMonitorUncertain = uncertain
	s.poolMonitorError = ""
	if uncertain {
		s.poolMonitorError = "号池监控已替换但持久化未确认，自动巡检已暂停；请核对后重新保存"
	}
	if err := s.applyPoolMonitorLocked(); err != nil {
		s.poolMonitorError = "号池监控已保存但运行策略未确认，自动巡检已暂停；请重新读取后保存"
		s.poolMonitorGate.Store(&poolMonitorGate{})
		fail(http.StatusServiceUnavailable, s.poolMonitorError, true)
		return
	}
	if uncertain {
		fail(http.StatusServiceUnavailable, s.poolMonitorError, true)
		return
	}
	writeJSON(w, http.StatusOK, s.publicPoolMonitorStateLocked())
}
func (s *Server) checkPoolMonitor(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Revision *int64 `json:"revision"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	s.poolMonitorMu.Lock()
	p := s.poolMonitor
	if req.Revision == nil || *req.Revision != p.Revision || !p.Enabled || s.poolMonitorError != "" || s.poolMonitorUncertain || s.poolMonitorChecking {
		state := s.publicPoolMonitorStateLocked()
		s.poolMonitorMu.Unlock()
		state["error"] = "请重新读取并保存已启用的巡检配置后再检查"
		state["outcome"] = "failed"
		writeJSON(w, http.StatusConflict, state)
		return
	}
	s.monitorMu.Lock()
	s.cpaMu.Lock()
	running := s.monitorCheckActive || s.monitor.Running() || s.cpaMonitor.running
	configured := s.monitorConfig.BaseURL != "" && s.monitorConfig.APIKey != "" && !s.monitorUncertain
	if p.Target == "cpa" {
		configured = s.cpaConfiguredLocked()
	}
	s.cpaMu.Unlock()
	s.monitorMu.Unlock()
	if running || !configured {
		state := s.publicPoolMonitorStateLocked()
		s.poolMonitorMu.Unlock()
		message := errPoolMonitorInactive.Error()
		if running {
			message = errPoolMonitorBusy.Error()
		}
		state["error"], state["outcome"], state["uncertain"] = message, "failed", false
		writeJSON(w, http.StatusConflict, state)
		return
	}
	s.poolMonitorChecking = true
	s.poolMonitorMu.Unlock()
	var err error
	if p.Target == "cpa" {
		_, err = s.cpaMonitorCheck(r.Context())
	} else {
		_, err = s.monitorCheck(r.Context(), "manual")
	}
	s.poolMonitorMu.Lock()
	s.poolMonitorChecking = false
	state := s.publicPoolMonitorStateLocked()
	s.poolMonitorMu.Unlock()
	if err != nil {
		for _, known := range []error{errPoolMonitorInactive, errPoolMonitorBusy, errCPAMonitorDisabled, errCPAMonitorBusy, errCPAMonitorChanged} {
			if errors.Is(err, known) {
				state["error"], state["outcome"], state["uncertain"] = known.Error(), "failed", false
				writeJSON(w, http.StatusConflict, state)
				return
			}
		}
		state["error"] = "本次巡检未完成，请读取状态核对结果"
		state["outcome"], state["uncertain"] = "unknown", true
		writeJSON(w, http.StatusBadGateway, state)
		return
	}
	writeJSON(w, http.StatusOK, state)
}
