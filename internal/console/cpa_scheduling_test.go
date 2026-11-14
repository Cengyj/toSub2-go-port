package console

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"tosub2-go-port/internal/protocol"
)

func TestCPASchedulingSettingsPersistAndPreserveLegacyDefaults(t *testing.T) {
	s, _ := newMonitorTestServer(t)
	if defaults := s.publicCPASettings(); defaults["priority"] != 0 || defaults["weight"] != 1 {
		t.Fatal("default routing options changed", defaults)
	}
	saveCPAFixture(t, s, "https://cpa.example.test")
	w := frontendRequest(s, http.MethodPost, "/api/cpa/settings", map[string]any{"baseUrl": "https://cpa.example.test", "revision": 1, "priority": -8, "weight": 0}, "test")
	if w.Code != http.StatusOK {
		t.Fatal(w.Code, w.Body.String())
	}
	restored := &Server{cfg: s.cfg, monitorVault: s.monitorVault}
	restored.initializeCPA()
	cfg, err := restored.configuredCPA()
	if err != nil || cfg.Priority != -8 || cfg.EffectiveWeight() != 0 {
		t.Fatal("routing options not restored", err)
	}
	// A legacy client that doesn't send these fields must not reset them.
	w = frontendRequest(s, http.MethodPost, "/api/cpa/settings", map[string]any{"baseUrl": "https://cpa.example.test", "revision": 2}, "test")
	if w.Code != http.StatusOK || s.publicCPASettings()["weight"] != 0 || s.publicCPASettings()["priority"] != -8 {
		t.Fatal("legacy save reset scheduling")
	}
	before, _ := os.ReadFile(filepath.Join(s.cfg.OutputRoot, "cpa-settings.json"))
	for _, values := range []map[string]any{{"weight": -1}, {"weight": 1_000_001}, {"weight": 1.5}, {"priority": int64(2147483648)}, {"priority": "1"}} {
		values["baseUrl"], values["revision"] = "https://cpa.example.test", 3
		w = frontendRequest(s, http.MethodPost, "/api/cpa/settings", values, "test")
		after, _ := os.ReadFile(filepath.Join(s.cfg.OutputRoot, "cpa-settings.json"))
		if w.Code != http.StatusBadRequest || string(after) != string(before) {
			t.Fatal("invalid options persisted", w.Code)
		}
	}
	// Metadata written by the initial CPA integration has neither option.
	var legacy map[string]any
	if json.Unmarshal(before, &legacy) != nil {
		t.Fatal("invalid fixture")
	}
	delete(legacy, "priority")
	delete(legacy, "weight")
	data, _ := json.Marshal(legacy)
	if err := os.WriteFile(filepath.Join(s.cfg.OutputRoot, "cpa-settings.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	restored.initializeCPA()
	if got := restored.publicCPASettings(); got["priority"] != 0 || got["weight"] != 1 {
		t.Fatal("legacy disk settings changed defaults")
	}
}

func TestCPAManualUploadFreezesSchedulingForWholeBatch(t *testing.T) {
	s, m := newMonitorTestServer(t)
	var calls atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		var doc map[string]any
		if json.NewDecoder(r.Body).Decode(&doc) != nil || doc["priority"] != float64(9) || doc["weight"] != float64(4) {
			t.Error("batch settings changed or were not uploaded")
		}
		if n == 1 {
			priority, weight := 20, 0
			cfg, err := s.configuredCPA()
			if err != nil {
				t.Error(err)
			}
			if _, err := s.commitCPASettings(r.Context(), cfg.BaseURL, "", 1, cpaSchedulingUpdate{Priority: &priority, Weight: &weight}); err != nil {
				t.Error(err)
			}
		}
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer remote.Close()
	w := frontendRequest(s, http.MethodPost, "/api/cpa/settings", map[string]any{"baseUrl": remote.URL, "adminApiKey": "key", "revision": 0, "priority": 9, "weight": 4}, "test")
	if w.Code != http.StatusOK {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, id := range []string{"scheduled-first", "scheduled-second"} {
		cpaAccountFixture(t, s, m, id, protocol.DefaultCodexClientID)
	}
	w = frontendRequest(s, http.MethodPost, "/api/cpa/upload", map[string]any{"ids": []string{"scheduled-first", "scheduled-second"}}, "test")
	if w.Code != http.StatusOK || calls.Load() != 2 {
		t.Fatal("scheduled batch failed", w.Code, calls.Load(), w.Body.String())
	}
	if s.publicCPASettings()["weight"] != 0 {
		t.Fatal("fixture settings didn't change")
	}
}
