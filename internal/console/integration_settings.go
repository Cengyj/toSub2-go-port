package console

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/settings"
)

func (s *Server) initializeIntegrationSettings() {
	protector, _ := s.monitorVault.(credentials.StateProtector)
	s.integrationSettings = settings.New(s.cfg.OutputRoot, protector)
}

// These are explicit private settings reads/writes. They do not apply monitor
// policy or contact providers. The existing use cases still validate and apply
// the submitted configuration, and report that outcome independently.
func (s *Server) integrationSettingsAPI(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/integration-settings/"), "/")
	if len(parts) > 2 || len(parts) == 2 && parts[1] != "confirm" {
		writeError(w, http.StatusNotFound, "unknown integration settings route")
		return
	}
	scope := settings.Scope(parts[0])
	confirm := len(parts) == 2
	var snapshot settings.Snapshot
	var err error
	verified := false
	switch {
	case !confirm && r.Method == http.MethodGet:
		snapshot, err = s.integrationSettings.Load(r.Context(), scope)
	case !confirm && r.Method == http.MethodPut, confirm && r.Method == http.MethodPost:
		var req struct {
			Revision *int64          `json:"revision"`
			Config   json.RawMessage `json:"config"`
		}
		if !decodeJSON(w, r, &req) {
			return
		}
		if req.Revision == nil {
			writeIntegrationSettingsError(w, settings.ErrInvalid)
			return
		}
		if confirm {
			snapshot, err = s.integrationSettings.Confirm(r.Context(), scope, *req.Revision)
		} else {
			snapshot, err = s.integrationSettings.Save(r.Context(), scope, *req.Revision, req.Config)
		}
		verified = err == nil
	default:
		w.Header().Set("Allow", "GET, PUT")
		if confirm {
			w.Header().Set("Allow", "POST")
		}
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if err != nil {
		writeIntegrationSettingsError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		settings.Snapshot
		Persistence string `json:"credentialPersistence"`
		Verified    bool   `json:"verified"`
		Applied     bool   `json:"applied"`
	}{snapshot, "secure", verified, false})
}

func writeIntegrationSettingsError(w http.ResponseWriter, err error) {
	status, code, message, outcome := http.StatusServiceUnavailable, "SETTINGS_UNAVAILABLE", settings.ErrUnavailable.Error(), "failed"
	switch {
	case errors.Is(err, settings.ErrInvalid):
		status, code, message = http.StatusBadRequest, "SETTINGS_INVALID", settings.ErrInvalid.Error()
	case errors.Is(err, settings.ErrConflict):
		status, code, message = http.StatusConflict, "SETTINGS_CONFLICT", settings.ErrConflict.Error()
	case errors.Is(err, settings.ErrUncertain):
		code, message, outcome = "SETTINGS_UNCERTAIN", settings.ErrUncertain.Error(), "unknown"
	}
	writeJSON(w, status, map[string]any{"error": message, "code": code, "outcome": outcome, "uncertain": outcome == "unknown", "verified": false, "applied": false})
}
