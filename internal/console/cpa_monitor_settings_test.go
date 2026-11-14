package console

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

func cpaMonitorPolicyFixture(revision int, enabled bool) map[string]any {
	return map[string]any{"revision": revision, "enabled": enabled, "excludedPlanTypes": []string{"free", "unknown"}, "intervalMinutes": 15, "cooldownMinutes": 30}
}

func TestCPAMonitorPolicySavePreservesConnectionAndUploadParameters(t *testing.T) {
	s, _ := newMonitorTestServer(t)
	var calls atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer remote.Close()
	w := frontendRequest(s, http.MethodPost, "/api/cpa/settings", map[string]any{"baseUrl": remote.URL, "adminApiKey": "private-policy-canary", "revision": 0, "priority": 42, "weight": 0}, "test")
	if w.Code != http.StatusOK {
		t.Fatal("connection fixture failed", w.Code)
	}
	reference := s.cpa.CredentialRef
	w = frontendRequest(s, http.MethodPost, "/api/cpa/monitor", cpaMonitorPolicyFixture(1, true), "test")
	if w.Code != http.StatusOK {
		t.Fatal("policy save failed", w.Code, w.Body.String())
	}
	var state map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	if state["revision"] != float64(2) || state["enabled"] != true || state["configured"] != true || state["intervalMinutes"] != float64(15) || state["cooldownMinutes"] != float64(30) || state["credentialPersistence"] != "secure" || state["configurationError"] != "" {
		t.Fatal("policy response lost monitor fields", state)
	}
	if strings.Contains(w.Body.String(), "private-policy-canary") || strings.Contains(w.Body.String(), "adminApiKey") || strings.Contains(w.Body.String(), remote.URL) || calls.Load() != 0 {
		t.Fatal("saving policy disclosed connection or performed an immediate remote check")
	}
	restored := &Server{cfg: s.cfg, monitorVault: s.monitorVault}
	restored.initializeCPA()
	cfg, err := restored.configuredCPA()
	if err != nil || cfg.BaseURL != remote.URL || cfg.APIKey != "private-policy-canary" || cfg.Priority != 42 || cfg.EffectiveWeight() != 0 || restored.cpa.CredentialRef != reference || !reflect.DeepEqual(restored.cpa.ExcludedPlanTypes, []string{"free", "unknown"}) {
		t.Fatal("policy save changed connection, credential reference or scheduling", err)
	}
	// An independent connection edit must preserve the rules now owned by the
	// monitor page; a monitor editor loaded before it cannot overwrite it.
	w = frontendRequest(s, http.MethodPost, "/api/cpa/settings", map[string]any{"baseUrl": remote.URL, "revision": 2, "priority": 7}, "test")
	if w.Code != http.StatusOK || !s.cpa.MonitorEnabled || s.cpa.CooldownMinutes != 30 {
		t.Fatal("connection edit reset monitor policy")
	}
	w = frontendRequest(s, http.MethodPost, "/api/cpa/monitor", cpaMonitorPolicyFixture(2, false), "test")
	if w.Code != http.StatusConflict || !s.cpa.MonitorEnabled || s.cpa.Priority != 7 {
		t.Fatal("stale monitor editor overwrote a newer connection edit")
	}
}

func TestCPAMonitorPolicyRejectsUnsafeOrInvalidWrites(t *testing.T) {
	s, _ := newMonitorTestServer(t)
	saveCPAFixture(t, s, "https://cpa.synthetic.invalid")
	before, err := os.ReadFile(filepath.Join(s.cfg.OutputRoot, "cpa-settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, fields := range []map[string]any{
		{"revision": 0}, {"enabled": nil}, {"excludedPlanTypes": nil}, {"excludedPlanTypes": []string{"typo"}},
		{"intervalMinutes": 0}, {"intervalMinutes": 1.5}, {"cooldownMinutes": 1441},
		{"baseUrl": "https://different.synthetic.invalid"}, {"adminApiKey": "misplaced-key"}, {"priority": 7}, {"weight": 0},
	} {
		body := cpaMonitorPolicyFixture(1, true)
		for key, value := range fields {
			body[key] = value
		}
		w := frontendRequest(s, http.MethodPost, "/api/cpa/monitor", body, "test")
		after, err := os.ReadFile(filepath.Join(s.cfg.OutputRoot, "cpa-settings.json"))
		if w.Code < 400 || w.Code >= 500 || err != nil || string(after) != string(before) {
			t.Fatal("invalid policy changed saved configuration", fields, w.Code, err)
		}
	}
	unauthorized := frontendRequest(s, http.MethodPost, "/api/cpa/monitor", cpaMonitorPolicyFixture(1, true), "wrong-token")
	if unauthorized.Code != http.StatusForbidden {
		t.Fatal("policy save did not enforce console authorization", unauthorized.Code)
	}
}

func TestCPAMonitorPolicyCanBePreparedBeforeConnectionAndKeepsFailedSave(t *testing.T) {
	s, _ := newMonitorTestServer(t)
	w := frontendRequest(s, http.MethodPost, "/api/cpa/monitor", cpaMonitorPolicyFixture(0, true), "test")
	if w.Code != http.StatusBadRequest || s.cpa.MonitorEnabled || s.cpa.Revision != 0 {
		t.Fatal("unconfigured CPA monitor enabled")
	}
	w = frontendRequest(s, http.MethodPost, "/api/cpa/monitor", cpaMonitorPolicyFixture(0, false), "test")
	if w.Code != http.StatusOK || s.cpa.Revision != 1 || s.cpa.IntervalMinutes != 15 || s.cpa.MonitorEnabled {
		t.Fatal("disabled policy could not be prepared without a connection", w.Code)
	}
	s.cfg.OutputRoot = t.TempDir()
	if err := os.Mkdir(filepath.Join(s.cfg.OutputRoot, "cpa-settings.json"), 0700); err != nil {
		t.Fatal(err)
	}
	body := cpaMonitorPolicyFixture(1, false)
	body["intervalMinutes"] = 9
	w = frontendRequest(s, http.MethodPost, "/api/cpa/monitor", body, "test")
	if w.Code != http.StatusInternalServerError || s.cpa.Revision != 1 || s.cpa.IntervalMinutes != 15 || !strings.Contains(w.Body.String(), `"outcome":"failed"`) || !strings.Contains(w.Body.String(), `"uncertain":false`) {
		t.Fatal("failed policy save changed live state or reported unknown", w.Code)
	}
}

func TestMonitorServiceConnectionSavePreservesRepairSwitchAndExclusions(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		s, _ := newMonitorTestServer(t)
		w := frontendRequest(s, http.MethodPost, "/api/sub2api/monitor", map[string]any{
			"enabled": enabled, "excludedPlanTypes": []string{"free", "pro"},
			"config": map[string]any{"baseUrl": "https://sub2.synthetic.invalid", "adminApiKey": "synthetic-key", "groupIds": []int{7}},
		}, "test")
		if w.Code != http.StatusOK {
			t.Fatal("Sub2 connection fixture failed", w.Code)
		}
		w = frontendRequest(s, http.MethodPost, "/api/sub2api/monitor", map[string]any{
			"config": map[string]any{"baseUrl": "https://sub2.synthetic.invalid", "groupIds": []int{8}, "openaiRequestTimezone": "UTC"},
		}, "test")
		cfg := s.monitor.Configuration()
		if w.Code != http.StatusOK || cfg.Enabled != enabled || !reflect.DeepEqual(cfg.ExcludedPlanTypes, []string{"free", "pro"}) || !reflect.DeepEqual(cfg.GroupIDs, []int{8}) || cfg.OpenAIRequestTimezone != "UTC" {
			t.Fatal("service-only edit changed monitor rules", enabled, w.Code)
		}
		w = frontendRequest(s, http.MethodPost, "/api/sub2api/monitor", map[string]any{"enabled": !enabled, "excludedPlanTypes": []string{"team"}}, "test")
		cfg = s.monitor.Configuration()
		if w.Code != http.StatusOK || cfg.Enabled != !enabled || !reflect.DeepEqual(cfg.ExcludedPlanTypes, []string{"team"}) || cfg.BaseURL != "https://sub2.synthetic.invalid" || !reflect.DeepEqual(cfg.GroupIDs, []int{8}) {
			t.Fatal("monitor-only edit lost service connection or groups", enabled, w.Code)
		}
	}
}
