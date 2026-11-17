package console

import (
	"embed"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"testing"
	"time"

	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/sub2api"
)

func saveZeroCooldownPolicy(t *testing.T, s *Server, target string, cooldown int) {
	t.Helper()
	body := poolPolicyBody(s, target, true)
	body["excludedPlanTypes"], body["intervalMinutes"], body["cooldownMinutes"] = []string{}, 1, cooldown
	w := frontendRequest(s, http.MethodPost, "/api/pool-monitor", body, "test")
	if w.Code != http.StatusOK {
		t.Fatalf("cooldown %d policy save failed: %d %s", cooldown, w.Code, w.Body.String())
	}
}

func TestPoolMonitorZeroCooldownPersistsAcrossTargetsConnectionsAndRestart(t *testing.T) {
	for _, target := range []string{"sub2api", "cpa"} {
		t.Run(target, func(t *testing.T) {
			s, _ := sharedMonitorFixture(t)
			if s.poolMonitor.CooldownMinutes != 5 {
				t.Fatal("unspecified cooldown lost its five-minute default")
			}
			connectSharedMonitors(t, s, "https://sub2.synthetic.invalid", "https://cpa.synthetic.invalid")
			saveZeroCooldownPolicy(t, s, target, 0)
			before := s.poolMonitor.clone()
			connectSharedMonitors(t, s, "https://sub2.synthetic.invalid", "https://cpa.synthetic.invalid")
			if !reflect.DeepEqual(before, s.poolMonitor) {
				t.Fatal("connection save replaced zero cooldown")
			}
			// The authoritative saved policy must win over legacy environment.
			t.Setenv("SUB2API_AUTO_REPAIR_COOLDOWN_MS", "600000")
			restored := NewServer(s.cfg, s.jobs, embed.FS{}, slog.Default(), s.monitorVault)
			t.Cleanup(restored.Close)
			if !reflect.DeepEqual(before, restored.poolMonitor) || restored.monitor.Configuration().Cooldown != 0 || !restored.monitor.Configuration().CooldownConfigured || restored.cpa.CooldownMinutes != 0 || restored.monitor.Configuration().Interval != time.Minute {
				t.Fatal("restart changed zero cooldown or interval")
			}
			for _, path := range []string{"/api/pool-monitor", "/api/sub2api/monitor", "/api/cpa/monitor", "/api/cpa/settings"} {
				w := frontendRequest(restored, http.MethodGet, path, nil, "test")
				var state map[string]any
				if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &state) != nil || state["cooldownMinutes"] != float64(0) || state["intervalMinutes"] != float64(1) {
					t.Fatalf("zero cooldown not returned at %s: %d %s", path, w.Code, w.Body.String())
				}
			}
			data, err := os.ReadFile(restored.poolMonitorFile())
			var saved map[string]any
			if err != nil || json.Unmarshal(data, &saved) != nil || saved["cooldownMinutes"] != float64(0) {
				t.Fatal("zero cooldown was omitted from durable settings")
			}
		})
	}
}

func TestPoolMonitorZeroCooldownAdmitsRecentAttemptButKeepsBusyGate(t *testing.T) {
	for _, target := range []string{"sub2api", "cpa"} {
		t.Run(target, func(t *testing.T) {
			s, manager := sharedMonitorFixture(t)
			job, entry := cpaRepairFixture(t, s, manager, "zero-cooldown-admission")
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					t.Error("queue admission performed a remote write")
				}
				if target == "cpa" {
					_ = json.NewEncoder(w).Encode(map[string]any{"files": []any{entry}})
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"items": []any{map[string]any{
					"id": 61, "email": job.Email, "status": "error", "group_ids": []int{7},
				}}}})
			}))
			defer remote.Close()
			connectSharedMonitors(t, s, remote.URL, remote.URL)
			saveZeroCooldownPolicy(t, s, target, 5)
			manager.UpdateAutoRepair(job.ID, nil, "", "previous attempt failed", false)
			check := func() map[string]any {
				t.Helper()
				w := frontendRequest(s, http.MethodPost, "/api/pool-monitor/check", map[string]any{"revision": s.poolMonitor.Revision}, "test")
				var state struct{ LastResult map[string]any }
				if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &state) != nil {
					t.Fatalf("monitor check failed: %d %s", w.Code, w.Body.String())
				}
				return state.LastResult
			}
			if result := check(); result["cooldown"] != float64(1) || result["started"] != float64(0) {
				t.Fatal("positive cooldown did not defer recent attempt", result)
			}
			saveZeroCooldownPolicy(t, s, target, 0)
			if result := check(); result["cooldown"] != float64(0) || result["started"] != float64(1) {
				t.Fatal("zero cooldown did not admit recent attempt", result)
			}
			queued, _ := manager.Get(job.ID)
			if queued.Status != jobs.StatusQueued || queued.LastOperationType != "automatic_relogin" {
				t.Fatal("monitor did not queue automatic relogin")
			}
			if result := check(); result["started"] != float64(0) || result["busy"] != float64(1) {
				t.Fatal("zero cooldown bypassed active account ownership", result)
			}
		})
	}
}

func TestPoolMonitorMigratesExplicitZeroCooldown(t *testing.T) {
	for _, source := range []string{"sub2api-environment", "cpa-settings"} {
		t.Run(source, func(t *testing.T) {
			t.Setenv("SUB2API_AUTO_REPAIR_COOLDOWN_MS", "0")
			s, _ := newMonitorTestServer(t)
			if source == "cpa-settings" {
				configureCPAMonitorFixture(t, s, "https://cpa.synthetic.invalid", true, nil)
				body := cpaMonitorPolicyFixture(int(s.cpa.Revision), true)
				body["cooldownMinutes"] = 0
				w := frontendRequest(s, http.MethodPost, "/api/cpa/monitor", body, "test")
				if w.Code != http.StatusOK {
					t.Fatalf("legacy zero policy rejected: %d %s", w.Code, w.Body.String())
				}
				s.initializeCPA()
			} else if err := s.configureMonitor(sub2api.Config{BaseURL: "https://sub2.synthetic.invalid", APIKey: "synthetic-key"}, true, false); err != nil {
				t.Fatal(err)
			}
			s.poolMonitorGate.Store(&poolMonitorGate{})
			s.initializePoolMonitor()
			if s.poolMonitorError != "" || s.poolMonitor.CooldownMinutes != 0 || s.monitor.Configuration().Cooldown != 0 || s.cpa.CooldownMinutes != 0 {
				t.Fatal("migration re-enabled a disabled cooldown")
			}
		})
	}
}

func TestPoolMonitorMigrationKeepsPositiveSecondCooldown(t *testing.T) {
	t.Setenv("SUB2API_AUTO_REPAIR_COOLDOWN_MS", "1000")
	s, _ := newMonitorTestServer(t)
	if s.monitor.Configuration().Cooldown != time.Second || s.publicMonitorState()["cooldownMinutes"] != float64(1) {
		t.Fatal("positive legacy cooldown was treated as disabled")
	}
	s.poolMonitorGate.Store(&poolMonitorGate{})
	s.initializePoolMonitor()
	if s.poolMonitorError != "" || s.poolMonitor.CooldownMinutes != 1 || s.monitor.Configuration().Cooldown != time.Minute {
		t.Fatal("positive second cooldown lost historical minute rounding")
	}
}

func TestPoolMonitorZeroCooldownStillRejectsInvalidOrMissingValues(t *testing.T) {
	s, _ := sharedMonitorFixture(t)
	before, err := os.ReadFile(s.poolMonitorFile())
	if err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []any{-1, 1.5, 1441, "0", nil} {
		body := poolPolicyBody(s, "sub2api", false)
		body["cooldownMinutes"] = invalid
		w := frontendRequest(s, http.MethodPost, "/api/pool-monitor", body, "test")
		if w.Code != http.StatusBadRequest {
			t.Fatalf("invalid cooldown %v accepted: %d", invalid, w.Code)
		}
	}
	for _, field := range []string{"cooldownMinutes", "intervalMinutes"} {
		body := poolPolicyBody(s, "sub2api", false)
		body["cooldownMinutes"] = 0
		if field == "cooldownMinutes" {
			delete(body, field)
		} else {
			body[field] = 0
		}
		if w := frontendRequest(s, http.MethodPost, "/api/pool-monitor", body, "test"); w.Code != http.StatusBadRequest {
			t.Fatal("missing cooldown or zero interval accepted", field, w.Code)
		}
	}
	after, err := os.ReadFile(s.poolMonitorFile())
	if err != nil || string(before) != string(after) {
		t.Fatal("invalid input changed persisted policy")
	}
}
