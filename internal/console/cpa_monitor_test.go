package console

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"tosub2-go-port/internal/cpa"
	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/protocol"
)

func configureCPAMonitorFixture(t *testing.T, s *Server, base string, enabled bool, excluded []string) {
	t.Helper()
	w := frontendRequest(s, http.MethodPost, "/api/cpa/settings", map[string]any{
		"baseUrl": base, "adminApiKey": "cpa-monitor-private-key", "revision": s.publicCPASettings()["revision"],
		"monitorEnabled": enabled, "excludedPlanTypes": excluded, "intervalMinutes": 2, "cooldownMinutes": 3,
	}, "test")
	if w.Code != http.StatusOK {
		t.Fatalf("configure CPA monitor: %d %s", w.Code, w.Body.String())
	}
}

func cpaRepairFixture(t *testing.T, s *Server, m *jobs.Manager, id string) (*jobs.Job, map[string]any) {
	t.Helper()
	j := cpaAccountFixture(t, s, m, id, protocol.DefaultCodexClientID)
	m.UpdateStoredCredentials(j.ID, credentials.Credentials{Password: "synthetic-password"})
	m.RecordAuthorization(j.ID, true, "fixture", map[string]bool{"password": true})
	j, _ = m.Get(j.ID)
	prepared, err := s.prepareCPAAccount(t.Context(), j)
	if err != nil {
		t.Fatal(err)
	}
	return j, map[string]any{"name": prepared.file.Filename, "type": "codex", "email": j.Email,
		"status": "error", "status_message": "invalid_grant", "id_token": map[string]any{"chatgpt_account_id": "account-" + id, "plan_type": "plus"}}
}

func TestCPAMonitorSettingsPersistValidateAndDefaultOff(t *testing.T) {
	s, _ := newMonitorTestServer(t)
	initial := s.publicCPASettings()
	if initial["monitorEnabled"] != false || initial["intervalMinutes"] != 5 || initial["cooldownMinutes"] != 5 {
		t.Fatal("unsafe defaults", initial)
	}
	configureCPAMonitorFixture(t, s, "https://cpa.synthetic.invalid", true, []string{"free", "PLUS", "free"})
	restored := &Server{cfg: s.cfg, monitorVault: s.monitorVault}
	restored.initializeCPA()
	settings := restored.publicCPASettings()
	if settings["monitorEnabled"] != true || settings["intervalMinutes"] != 2 || strings.Join(settings["excludedPlanTypes"].([]string), ",") != "free,plus" {
		t.Fatal("monitor settings not restored", settings)
	}
	before, _ := os.ReadFile(filepath.Join(s.cfg.OutputRoot, "cpa-settings.json"))
	for _, fields := range []map[string]any{{"intervalMinutes": 0}, {"cooldownMinutes": 1441}, {"intervalMinutes": 1.5}, {"monitorEnabled": "yes"}, {"excludedPlanTypes": []string{"mistyped"}}} {
		fields["baseUrl"], fields["revision"] = "https://cpa.synthetic.invalid", 1
		w := frontendRequest(s, http.MethodPost, "/api/cpa/settings", fields, "test")
		after, _ := os.ReadFile(filepath.Join(s.cfg.OutputRoot, "cpa-settings.json"))
		if w.Code != http.StatusBadRequest || string(before) != string(after) {
			t.Fatal("invalid monitor options persisted", w.Code)
		}
	}
	// An old client must preserve the rules it does not know about.
	w := frontendRequest(s, http.MethodPost, "/api/cpa/settings", map[string]any{"baseUrl": "https://cpa.synthetic.invalid", "revision": 1}, "test")
	if w.Code != http.StatusOK || s.publicCPASettings()["monitorEnabled"] != true {
		t.Fatal("old client cleared monitor options")
	}
	var legacy map[string]any
	_ = json.Unmarshal(before, &legacy)
	for _, key := range []string{"monitorEnabled", "excludedPlanTypes", "intervalMinutes", "cooldownMinutes"} {
		delete(legacy, key)
	}
	data, _ := json.Marshal(legacy)
	if err := os.WriteFile(filepath.Join(s.cfg.OutputRoot, "cpa-settings.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	restored.initializeCPA()
	if got := restored.publicCPASettings(); got["monitorEnabled"] != false || got["intervalMinutes"] != 5 {
		t.Fatal("legacy settings enabled automation", got)
	}
}

func TestCPAMonitorQueuesOnlyOwnedEligibleNonexcludedAuthorizations(t *testing.T) {
	s, m := newMonitorTestServer(t)
	m.SetCredentialStore(credentials.NewMemoryStore())
	ready, eligible := cpaRepairFixture(t, s, m, "cpa-monitor-ready")
	_, excluded := cpaRepairFixture(t, s, m, "cpa-monitor-excluded")
	excluded["id_token"].(map[string]any)["plan_type"] = "free"
	manual, ineligible := cpaRepairFixture(t, s, m, "cpa-monitor-manual")
	m.RecordAuthorization(manual.ID, false, "manual", nil)
	_, limited := cpaRepairFixture(t, s, m, "cpa-monitor-limited")
	limited["status_message"] = "429 quota_exceeded"
	// An old workspace with the same email must not suppress the matching file.
	old, err := cpa.BuildAuthFile(map[string]any{"platform": "openai", "type": "oauth", "credentials": map[string]any{
		"email": ready.Email, "client_id": protocol.DefaultCodexClientID, "chatgpt_account_id": "old-workspace", "access_token": "old-access", "refresh_token": "old-refresh"}})
	if err != nil {
		t.Fatal(err)
	}
	wrong := map[string]any{"name": old.Filename, "type": "codex", "email": ready.Email, "status": "error", "status_message": "invalid_grant", "id_token": map[string]any{"chatgpt_account_id": "old-workspace"}}
	var calls atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodGet {
			t.Error("monitor mutated CPA directly")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"files": []any{wrong, eligible, excluded, ineligible, limited}})
	}))
	defer remote.Close()
	configureCPAMonitorFixture(t, s, remote.URL, true, []string{"free"})
	summary, err := s.cpaMonitorCheck(t.Context())
	if err != nil || summary.Checked != 5 || summary.Started != 1 || summary.ExcludedPlan != 1 || summary.Ineligible != 2 || calls.Load() != 1 {
		t.Fatal("wrong candidates", summary, err, calls.Load())
	}
	j, _ := m.Get(ready.ID)
	input, _ := m.InputCopy(ready.ID)
	if j.Status != jobs.StatusQueued || j.UploadTarget != jobs.UploadTargetCPA || !j.UploadAfterRelogin || len(j.AutoRepairPendingAccountIDs) > 0 || input["cpaRepairAccountId"] != "account-"+ready.ID {
		t.Fatal("CPA repair not isolated/frozen", j.Status, j.UploadTarget)
	}
	second, err := s.cpaMonitorCheck(t.Context())
	if err != nil || second.Started != 0 || second.Busy != 2 {
		t.Fatal("queued repair was replayed", second, err)
	}
	w := frontendRequest(s, http.MethodGet, "/api/cpa/monitor", nil, "test")
	if strings.Contains(w.Body.String(), "private-key") || strings.Contains(w.Body.String(), ready.Email) || strings.Contains(w.Body.String(), "invalid_grant") {
		t.Fatal("private metadata exposed", w.Body.String())
	}
}

func TestCPAMonitorMatchesFilenameWhenCPAOmitsEmail(t *testing.T) {
	s, m := newMonitorTestServer(t)
	j, entry := cpaRepairFixture(t, s, m, "cpa-monitor-no-email")
	delete(entry, "email")
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"files": []any{entry}})
	}))
	defer remote.Close()
	configureCPAMonitorFixture(t, s, remote.URL, true, nil)
	summary, err := s.cpaMonitorCheck(t.Context())
	if err != nil || summary.Started != 1 {
		t.Fatal("stable filename was not matched", summary, err, j.ID)
	}
}

func TestCPAMonitorConcurrentCheckAndSettingsChangeDoNotQueueStaleRepair(t *testing.T) {
	s, m := newMonitorTestServer(t)
	j, entry := cpaRepairFixture(t, s, m, "cpa-monitor-stale")
	entered, released := make(chan struct{}), make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(released) }) }
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-released
		_ = json.NewEncoder(w).Encode(map[string]any{"files": []any{entry}})
	}))
	defer remote.Close()
	defer release()
	configureCPAMonitorFixture(t, s, remote.URL, true, nil)
	result := make(chan error, 1)
	go func() { _, err := s.cpaMonitorCheck(context.Background()); result <- err }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("check did not start")
	}
	if _, err := s.cpaMonitorCheck(t.Context()); !errors.Is(err, errCPAMonitorBusy) {
		t.Fatal("parallel check admitted", err)
	}
	configureCPAMonitorFixture(t, s, remote.URL, false, nil)
	release()
	if err := <-result; !errors.Is(err, errCPAMonitorChanged) {
		t.Fatal("stale settings not rejected", err)
	}
	current, _ := m.Get(j.ID)
	if current.Status != jobs.StatusSucceeded || current.AutoRepairLastAttemptAt != nil {
		t.Fatal("disabled monitor queued repair", current.Status)
	}
	if _, err := s.cpaMonitorCheck(t.Context()); !errors.Is(err, errCPAMonitorDisabled) {
		t.Fatal("disabled check allowed", err)
	}
}

func TestCPAMonitorSchedulerAndExplicitCheckStopOnClose(t *testing.T) {
	for _, scheduled := range []bool{false, true} {
		t.Run(map[bool]string{false: "explicit", true: "scheduled"}[scheduled], func(t *testing.T) {
			s, _ := newMonitorTestServer(t)
			entered := make(chan struct{})
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(entered); <-r.Context().Done() }))
			defer remote.Close()
			configureCPAMonitorFixture(t, s, remote.URL, true, nil)
			finished := make(chan struct{})
			if scheduled {
				s.StartMonitor(context.Background())
				close(finished)
			} else {
				go func() { defer close(finished); _, _ = s.cpaMonitorCheck(context.Background()) }()
			}
			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				t.Fatal("monitor did not start")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if err := s.CloseContext(ctx); err != nil {
				t.Fatal(err)
			}
			select {
			case <-finished:
			case <-ctx.Done():
				t.Fatal("explicit request survived close")
			}
			state := s.publicCPAMonitorState()
			if state["running"] != false || state["nextCheckAt"].(*time.Time) != nil {
				t.Fatal("monitor survived close", state)
			}
			s.StartMonitor(context.Background())
			if _, err := s.cpaMonitorCheck(t.Context()); !errors.Is(err, errCPAMonitorDisabled) {
				t.Fatal("closed monitor restarted", err)
			}
		})
	}
}

func TestCPAMonitorScheduleReportsRealDeadlineAndClearsOnDisable(t *testing.T) {
	s, _ := newMonitorTestServer(t)
	var calls atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); _, _ = w.Write([]byte(`{"files":[]}`)) }))
	defer remote.Close()
	configureCPAMonitorFixture(t, s, remote.URL, true, nil)
	if s.publicCPAMonitorState()["nextCheckAt"].(*time.Time) != nil {
		t.Fatal("deadline invented before scheduler")
	}
	s.StartMonitor(t.Context())
	deadline := time.Now().Add(3 * time.Second)
	for s.publicCPAMonitorState()["nextCheckAt"].(*time.Time) == nil && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	next := s.publicCPAMonitorState()["nextCheckAt"].(*time.Time)
	if next == nil || time.Until(*next) > 2*time.Minute || calls.Load() != 1 {
		t.Fatal("scheduler not armed", next, calls.Load())
	}
	configureCPAMonitorFixture(t, s, remote.URL, false, nil)
	if s.publicCPAMonitorState()["nextCheckAt"].(*time.Time) != nil {
		t.Fatal("disabled scheduler retained deadline")
	}
	s.stopMonitor()
}

func TestCPAMonitorPublicJobUsesCPAPolicyAndShowsPausedTransfer(t *testing.T) {
	s, m := newMonitorTestServer(t)
	j, _ := cpaRepairFixture(t, s, m, "cpa-public-policy")
	j.UploadTarget, j.LastOperationType = jobs.UploadTargetCPA, "automatic_relogin"
	j.PlanType = "free"
	s.cpa.ExcludedPlanTypes = []string{"free"}
	if state := s.publicJob(j); state["autoRepairEligible"] != false || !strings.Contains(state["autoRepairEligibilityReason"].(string), "排除") {
		t.Fatal("CPA task eligibility ignored CPA rules", state["autoRepairEligibilityReason"])
	}
	s.cpa.ExcludedPlanTypes = nil
	j.AutoRepairSyncPaused = true
	if state := s.publicJob(j); state["autoRepairEligible"] != false || state["autoRepairSyncPaused"] != true || !strings.Contains(state["autoRepairEligibilityReason"].(string), "核实") {
		t.Fatal("paused transfer not visible", state["autoRepairEligibilityReason"])
	}
}
