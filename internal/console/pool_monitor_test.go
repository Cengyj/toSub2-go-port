package console

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/privatefile"
	"tosub2-go-port/internal/sub2api"
)

func sharedMonitorFixture(t *testing.T) (*Server, *jobs.Manager) {
	t.Helper()
	s, m := newMonitorTestServer(t)
	s.poolMonitorGate.Store(&poolMonitorGate{})
	s.initializePoolMonitor()
	return s, m
}
func poolPolicyBody(s *Server, target string, enabled bool) map[string]any {
	p := s.poolMonitor
	return map[string]any{"revision": p.Revision, "target": target, "enabled": enabled, "excludedPlanTypes": []string{"free", "plus"}, "intervalMinutes": 17, "cooldownMinutes": 23}
}
func savePoolPolicy(t *testing.T, s *Server, target string, enabled bool) {
	t.Helper()
	w := frontendRequest(s, http.MethodPost, "/api/pool-monitor", poolPolicyBody(s, target, enabled), "test")
	if w.Code != http.StatusOK {
		t.Fatalf("shared policy save: %d %s", w.Code, w.Body.String())
	}
}
func connectSharedMonitors(t *testing.T, s *Server, sub2URL, cpaURL string) {
	t.Helper()
	for path, body := range map[string]map[string]any{
		"/api/sub2api/monitor": {"config": map[string]any{"baseUrl": sub2URL, "adminApiKey": "shared-sub2-test-key", "groupIds": []int{7}}},
		"/api/cpa/settings":    {"baseUrl": cpaURL, "adminApiKey": "shared-cpa-test-key", "revision": s.publicCPASettings()["revision"], "priority": 11, "weight": 0},
	} {
		w := frontendRequest(s, http.MethodPost, path, body, "test")
		if w.Code != http.StatusOK {
			t.Fatalf("service connection: %s %d %s", path, w.Code, w.Body.String())
		}
	}
}
func TestPoolMonitorOnePolicySelectionRestartAndConnectionSaves(t *testing.T) {
	s, _ := sharedMonitorFixture(t)
	connectSharedMonitors(t, s, "https://sub2.synthetic.invalid", "https://cpa.synthetic.invalid")
	savePoolPolicy(t, s, "sub2api", true)
	if !s.monitor.Configuration().Enabled || s.cpa.MonitorEnabled || !s.poolMonitorAllows("sub2api") || s.poolMonitorAllows("cpa") {
		t.Fatal("Sub2 selection permits multiple engines")
	}
	reference := s.cpa.CredentialRef
	savePoolPolicy(t, s, "cpa", true)
	if s.monitor.Configuration().Enabled || !s.cpa.MonitorEnabled || s.poolMonitorAllows("sub2api") || !s.poolMonitorAllows("cpa") {
		t.Fatal("CPA selection permits multiple engines")
	}
	if s.monitor.Configuration().Interval != 17*time.Minute || s.monitor.Configuration().Cooldown != 23*time.Minute || !reflect.DeepEqual(s.monitor.Configuration().ExcludedPlanTypes, s.cpa.ExcludedPlanTypes) {
		t.Fatal("engines do not share rules")
	}
	before := s.poolMonitor.clone()
	connectSharedMonitors(t, s, "https://sub2.synthetic.invalid", "https://cpa.synthetic.invalid")
	if !reflect.DeepEqual(before, s.poolMonitor) || s.monitor.Configuration().Enabled || !s.cpa.MonitorEnabled || s.cpa.Priority != 11 || s.cpa.EffectiveWeight() != 0 {
		t.Fatal("service save changed shared selection/rules")
	}
	// The connection fixture explicitly replaces its key, which must use a new
	// safe reference without changing the shared policy or upload options.
	if s.cpa.CredentialRef == reference {
		t.Fatal("credential generation was reused")
	}
	restored := NewServer(s.cfg, s.jobs, embed.FS{}, slog.Default(), s.monitorVault)
	t.Cleanup(restored.Close)
	if !reflect.DeepEqual(before, restored.poolMonitor) || restored.monitor.Configuration().Enabled || !restored.cpa.MonitorEnabled || restored.monitor.Configuration().Interval != 17*time.Minute {
		t.Fatal("restart restored independent legacy rules")
	}
	state := restored.publicPoolMonitorState()
	targets := state["targets"].(map[string]any)
	if state["target"] != "cpa" || state["enabled"] != true || targets["sub2api"].(map[string]any)["configured"] != true || targets["cpa"].(map[string]any)["configured"] != true {
		t.Fatal("shared state lacks selected policy or both connections")
	}
	for path, body := range map[string]map[string]any{
		"/api/sub2api/monitor": {"enabled": true},
		"/api/cpa/monitor":     cpaMonitorPolicyFixture(int(s.cpa.Revision), true),
		"/api/cpa/settings":    {"baseUrl": s.cpa.BaseURL, "revision": s.cpa.Revision, "monitorEnabled": true},
	} {
		w := frontendRequest(s, http.MethodPost, path, body, "test")
		if w.Code != http.StatusConflict {
			t.Fatalf("legacy rules bypassed shared authority: %s %d", path, w.Code)
		}
	}
	if _, err := s.monitorCheck(t.Context(), "manual"); err == nil {
		t.Fatal("inactive Sub2 check admitted")
	}
}
func TestPoolMonitorSchedulesOnlySelectedService(t *testing.T) {
	s, _ := sharedMonitorFixture(t)
	var subReads, cpaReads atomic.Int32
	sub2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		subReads.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"items": []any{}}})
	}))
	defer sub2.Close()
	cpa := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cpaReads.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"files": []any{}})
	}))
	defer cpa.Close()
	connectSharedMonitors(t, s, sub2.URL, cpa.URL)
	savePoolPolicy(t, s, "cpa", true)
	s.StartMonitor(t.Context())
	deadline := time.After(time.Second)
	for cpaReads.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("selected CPA scheduler did not run")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	s.stopMonitor()
	if subReads.Load() != 0 || cpaReads.Load() != 1 {
		t.Fatalf("schedulers requested both services: sub2=%d cpa=%d", subReads.Load(), cpaReads.Load())
	}
	savePoolPolicy(t, s, "sub2api", true)
	s.StartMonitor(t.Context())
	deadline = time.After(time.Second)
	for subReads.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("selected Sub2 scheduler did not run")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	s.stopMonitor()
	if subReads.Load() != 1 || cpaReads.Load() != 1 {
		t.Fatalf("target switch ran an inactive service: sub2=%d cpa=%d", subReads.Load(), cpaReads.Load())
	}
}
func TestPoolMonitorMigrationAndMalformedSharedFileFailClosed(t *testing.T) {
	s, _ := newMonitorTestServer(t)
	t.Setenv("SUB2API_MONITOR_INTERVAL_MS", "500")
	if err := s.configureMonitor(sub2api.Config{BaseURL: "https://sub2.synthetic.invalid", APIKey: "migration-key", ExcludedPlanTypes: []string{"team"}}, true, false); err != nil {
		t.Fatal(err)
	}
	configureCPAMonitorFixture(t, s, "https://cpa.synthetic.invalid", true, []string{"free"})
	s.poolMonitorGate.Store(&poolMonitorGate{})
	s.initializePoolMonitor()
	if s.poolMonitor.Target != "sub2api" || !s.poolMonitor.Enabled || s.cpa.MonitorEnabled || !reflect.DeepEqual(s.poolMonitor.ExcludedPlanTypes, []string{"team"}) {
		t.Fatal("both-enabled migration did not pick one deterministic target")
	}
	// An invalid authoritative file must never fall back to an enabled legacy
	// integration, including one still carrying the old saved enabled flag.
	for _, raw := range []string{`{"version":1,"target":"both","enabled":true}`, `null`, `{}`, `{"enabled":true}`, `{"version":1,"target":"cpa","enabled":true}`} {
		if err := os.WriteFile(s.poolMonitorFile(), []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		s.initializePoolMonitor()
		if s.poolMonitor.Enabled || s.monitor.Configuration().Enabled || s.cpa.MonitorEnabled || s.poolMonitorError == "" || s.poolMonitorAllows("sub2api") || s.poolMonitorAllows("cpa") {
			t.Fatal("invalid shared state reopened legacy automation", raw)
		}
	}
	savePoolPolicy(t, s, "cpa", false)
	if s.poolMonitorError != "" {
		t.Fatal("explicit valid save did not repair invalid shared state")
	}
}
func TestPoolMonitorConcurrentRevisionAndUncertainCommitPauseBothEngines(t *testing.T) {
	s, _ := sharedMonitorFixture(t)
	connectSharedMonitors(t, s, "https://sub2.synthetic.invalid", "https://cpa.synthetic.invalid")
	stale := poolPolicyBody(s, "sub2api", true)
	savePoolPolicy(t, s, "cpa", true)
	w := frontendRequest(s, http.MethodPost, "/api/pool-monitor", stale, "test")
	if w.Code != http.StatusConflict || s.poolMonitor.Target != "cpa" {
		t.Fatal("stale editor replaced selected target")
	}
	before, _ := os.ReadFile(s.poolMonitorFile())
	s.poolMonitorWrite = func(context.Context, string, []byte) error { return errors.New("synthetic pre-rename failure") }
	w = frontendRequest(s, http.MethodPost, "/api/pool-monitor", poolPolicyBody(s, "sub2api", true), "test")
	after, _ := os.ReadFile(s.poolMonitorFile())
	if w.Code != http.StatusInternalServerError || string(before) != string(after) || !s.poolMonitorAllows("cpa") {
		t.Fatal("failed save replaced prior policy")
	}
	s.poolMonitorWrite = func(ctx context.Context, path string, data []byte) error {
		if err := privatefile.Write(ctx, path, data); err != nil {
			return err
		}
		return privatefile.ErrDurabilityUncertain
	}
	w = frontendRequest(s, http.MethodPost, "/api/pool-monitor", poolPolicyBody(s, "sub2api", true), "test")
	if w.Code != http.StatusServiceUnavailable || !s.poolMonitorUncertain || s.monitor.Configuration().Enabled || s.cpa.MonitorEnabled || s.poolMonitorAllows("sub2api") || s.poolMonitorAllows("cpa") {
		t.Fatal("uncertain shared commit allowed automatic work")
	}
	_ = frontendRequest(s, http.MethodGet, "/api/pool-monitor", nil, "test")
	if !s.poolMonitorUncertain {
		t.Fatal("passive read silently confirmed uncertain storage")
	}
	s.poolMonitorWrite = nil
	savePoolPolicy(t, s, "sub2api", true)
	if s.poolMonitorUncertain || !s.monitor.Configuration().Enabled {
		t.Fatal("explicit re-save failed to confirm policy")
	}
}
func TestPoolMonitorCheckReservationRejectsConcurrentRuleAndConnectionEdits(t *testing.T) {
	s, _ := sharedMonitorFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		_ = json.NewEncoder(w).Encode(map[string]any{"files": []any{}})
	}))
	defer remote.Close()
	connectSharedMonitors(t, s, "https://sub2.synthetic.invalid", remote.URL)
	savePoolPolicy(t, s, "cpa", true)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- frontendRequest(s, http.MethodPost, "/api/pool-monitor/check", map[string]any{"revision": s.poolMonitor.Revision}, "test")
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		close(release)
		t.Fatal("explicit check did not enter synthetic service")
	}
	w := frontendRequest(s, http.MethodPost, "/api/pool-monitor", poolPolicyBody(s, "sub2api", true), "test")
	connection := frontendRequest(s, http.MethodPost, "/api/cpa/settings", map[string]any{"baseUrl": remote.URL, "revision": s.cpa.Revision}, "test")
	if w.Code != http.StatusConflict || connection.Code != http.StatusConflict || s.poolMonitor.Target != "cpa" {
		close(release)
		t.Fatal("in-flight check did not reserve its selected config")
	}
	close(release)
	if w := <-done; w.Code != http.StatusOK {
		t.Fatalf("synthetic check failed: %d %s", w.Code, w.Body.String())
	}
	w = frontendRequest(s, http.MethodPost, "/api/pool-monitor/check", map[string]any{"revision": s.poolMonitor.Revision - 1}, "test")
	if w.Code != http.StatusConflict {
		t.Fatal("stale check revision admitted")
	}
}
func TestPoolMonitorSwitchInvalidatesQueuedOldTarget(t *testing.T) {
	s, m := sharedMonitorFixture(t)
	connectSharedMonitors(t, s, "https://sub2.synthetic.invalid", "https://cpa.synthetic.invalid")
	savePoolPolicy(t, s, "sub2api", true)
	j, err := m.Submit(jobs.Spec{ID: "shared-old-target", Email: "old-target@synthetic.invalid"})
	if err != nil {
		t.Fatal(err)
	}
	m.Cancel(j.ID)
	if _, err := m.QueueAutoRepair(j.ID, []int{1}, sub2api.BackendIdentity(s.monitorConfig.BaseURL), "team"); err != nil {
		t.Fatal(err)
	}
	savePoolPolicy(t, s, "cpa", true)
	// The old target must stay invalidated even if selection changes back
	// before the manager's scheduler has had a chance to run.
	savePoolPolicy(t, s, "sub2api", true)
	var calls atomic.Int32
	m.SetRunner(func(context.Context, *jobs.Job, func(jobs.JobEvent)) error { calls.Add(1); return nil })
	m.Start()
	defer m.Shutdown(context.Background())
	deadline := time.After(time.Second)
	for {
		current, _ := m.Get(j.ID)
		if current.Status == jobs.StatusCanceled {
			break
		}
		select {
		case <-deadline:
			t.Fatal("old target queued operation was not skipped")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("switch executed queued old-target login")
	}
	data, err := os.ReadFile(filepath.Join(s.cfg.OutputRoot, "pool-monitor.json"))
	if err != nil {
		t.Fatal(err)
	}
	if bytesContainSecret(data) {
		t.Fatal("shared policy contains credentials")
	}
}
func bytesContainSecret(data []byte) bool {
	return strings.Contains(string(data), "shared-sub2-test-key") || strings.Contains(string(data), "shared-cpa-test-key")
}

func TestPoolMonitorMigratesSoleCPAPolicyAndRejectsInvalidWrites(t *testing.T) {
	s, _ := newMonitorTestServer(t)
	configureCPAMonitorFixture(t, s, "https://cpa.synthetic.invalid", true, []string{"team"})
	s.poolMonitorGate.Store(&poolMonitorGate{})
	s.initializePoolMonitor()
	if s.poolMonitor.Target != "cpa" || !s.poolMonitor.Enabled || s.poolMonitor.IntervalMinutes != 2 || s.poolMonitor.CooldownMinutes != 3 || !reflect.DeepEqual(s.poolMonitor.ExcludedPlanTypes, []string{"team"}) || s.monitor.Configuration().Enabled {
		t.Fatal("migration lost the sole active CPA policy")
	}
	before, _ := os.ReadFile(s.poolMonitorFile())
	for _, field := range []map[string]any{
		{"target": "both"}, {"intervalMinutes": 0}, {"cooldownMinutes": 1441}, {"intervalMinutes": 1.5}, {"excludedPlanTypes": []string{"misspelled"}}, {"adminApiKey": "must-not-enter-policy"},
	} {
		body := poolPolicyBody(s, "cpa", true)
		for key, value := range field {
			body[key] = value
		}
		w := frontendRequest(s, http.MethodPost, "/api/pool-monitor", body, "test")
		after, _ := os.ReadFile(s.poolMonitorFile())
		if w.Code != http.StatusBadRequest || string(before) != string(after) || !s.poolMonitorAllows("cpa") {
			t.Fatalf("invalid shared write changed active policy: %d %v", w.Code, field)
		}
	}
}

func TestPoolMonitorSub2UncertainConnectionPausesUntilConnectionResaved(t *testing.T) {
	s, _ := sharedMonitorFixture(t)
	connectSharedMonitors(t, s, "https://sub2.synthetic.invalid", "https://cpa.synthetic.invalid")
	savePoolPolicy(t, s, "sub2api", true)
	s.monitorConfigure = func(client *sub2api.Client, config sub2api.MonitorConfig, hooks sub2api.MonitorHooks) error {
		if err := s.monitor.Configure(client, config, hooks); err != nil {
			return err
		}
		return privatefile.ErrDurabilityUncertain
	}
	body := map[string]any{"config": map[string]any{"baseUrl": s.monitorConfig.BaseURL, "codexFingerprintMode": "session"}}
	w := frontendRequest(s, http.MethodPost, "/api/sub2api/monitor", body, "test")
	state := s.publicPoolMonitorState()
	if w.Code != http.StatusServiceUnavailable || !s.monitorUncertain || s.poolMonitorAllows("sub2api") || state["configured"] != false || state["uncertain"] != true || state["configurationError"] == "" {
		t.Fatalf("unconfirmed Sub2 connection kept automatic monitoring: %d %#v", w.Code, state)
	}
	if _, err := s.monitorCheck(t.Context(), "manual"); err == nil {
		t.Fatal("unconfirmed connection admitted a check")
	}
	w = frontendRequest(s, http.MethodPost, "/api/pool-monitor", poolPolicyBody(s, "sub2api", true), "test")
	if w.Code != http.StatusBadRequest {
		t.Fatal("rule save bypassed unconfirmed connection")
	}
	s.monitorConfigure = nil
	w = frontendRequest(s, http.MethodPost, "/api/sub2api/monitor", body, "test")
	state = s.publicPoolMonitorState()
	if w.Code != http.StatusOK || s.monitorUncertain || !s.poolMonitorAllows("sub2api") || !s.monitor.Configuration().Enabled || state["uncertain"] != false || state["configurationError"] != "" {
		t.Fatal("confirmed connection save did not recover shared policy", w.Code, state)
	}
}
