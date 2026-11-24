package console

import (
	"errors"
	"net/http"
	"strings"

	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/maintenance"
)

func (s *Server) initializeMaintenance() {
	s.maintenance = maintenance.New(s.cfg.OutputRoot)
	s.applyMaintenance()
}
func (s *Server) applyMaintenance() {
	paused := s.maintenance.Status().Enabled
	s.jobs.SetAdmissionPaused(paused)
	if s.insights != nil {
		s.insights.PauseAutomatic(paused)
	}
	if s.recycleBin != nil {
		s.recycleBin.PauseAutomatic(paused)
	}
}

// Requests that can admit work are fenced before their handlers run. Inputs
// and cancellation complete existing operations; they cannot start new ones.
// POST queries/options still take a lease because they may contact providers.
func maintenanceAdmissionRequired(r *http.Request) bool {
	if r.Method == http.MethodOptions {
		return false
	}
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		// The model catalogue is the only GET console endpoint which may
		// contact an upstream. Local status/download reads remain available.
		return strings.Contains(r.URL.Path, "/models")
	}
	if r.Method == http.MethodPost {
		if r.URL.Path == "/api/jobs/cancel-all" {
			return false
		}
		if strings.HasPrefix(r.URL.Path, "/api/jobs/") && (strings.HasSuffix(r.URL.Path, "/input") || strings.HasSuffix(r.URL.Path, "/cancel")) {
			return false
		}
		if strings.HasSuffix(r.URL.Path, "/cancel") && (strings.HasPrefix(r.URL.Path, "/api/account-insights/") || strings.HasPrefix(r.URL.Path, "/api/account-quality/")) {
			return false
		}
	}
	return true
}

type maintenanceStatus struct {
	maintenance.Status
	Jobs           jobs.ActivitySnapshot `json:"jobs"`
	Insights       int                   `json:"insights"`
	Probes         int                   `json:"probes"`
	SMS            int                   `json:"sms"`
	RecycleRunning bool                  `json:"recycleRunning"`
	Drained        bool                  `json:"drained"`
}

func (s *Server) maintenanceStatus() maintenanceStatus {
	state := maintenanceStatus{Status: s.maintenance.Status(), Jobs: s.jobs.ActivitySnapshot()}
	if s.insights != nil {
		q, a := s.insights.Activity()
		state.Insights = q + a
	}
	if s.quality != nil {
		state.Probes = s.quality.Activity()
	}
	if s.recycleBin != nil {
		state.RecycleRunning = s.recycleBin.Status().Running
	}
	s.smsMu.Lock()
	state.SMS = len(s.smsAcquiring) + len(s.smsCompleting)
	for _, lease := range s.smsLeases {
		if lease.cancel != nil {
			state.SMS++
		}
	}
	s.smsMu.Unlock()
	idle := state.Jobs.Drained && state.Insights == 0 && state.Probes == 0 && state.SMS == 0 && !state.RecycleRunning
	state.Drained, _ = s.maintenance.HoldDrained(idle)
	return state
}

func (s *Server) maintenanceAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		s.maintenanceMu.Lock()
		defer s.maintenanceMu.Unlock()
		writeJSON(w, http.StatusOK, s.maintenanceStatus())
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST")
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req struct {
		Enabled  *bool  `json:"enabled"`
		Revision *int64 `json:"revision"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Enabled == nil || req.Revision == nil {
		writeError(w, http.StatusBadRequest, "维护操作需要启用状态和当前版本")
		return
	}
	s.maintenanceMu.Lock()
	err := s.maintenance.Change(r.Context(), *req.Enabled, *req.Revision)
	s.applyMaintenance()
	state := s.maintenanceStatus()
	s.maintenanceMu.Unlock()
	if err != nil {
		status, code, outcome := http.StatusServiceUnavailable, "MAINTENANCE_UNAVAILABLE", "failed"
		if errors.Is(err, maintenance.ErrConflict) {
			status, code = http.StatusConflict, "MAINTENANCE_CONFLICT"
		}
		if errors.Is(err, maintenance.ErrUncertain) {
			code, outcome = "MAINTENANCE_UNCERTAIN", "unknown"
		}
		writeJSON(w, status, map[string]any{"error": err.Error(), "code": code, "outcome": outcome, "maintenance": state})
		return
	}
	writeJSON(w, http.StatusOK, state)
}
