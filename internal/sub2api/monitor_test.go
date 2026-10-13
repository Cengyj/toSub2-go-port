package sub2api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMonitorServiceGroupsDuplicateRemoteIDsAndHidesCredentials(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "secret" {
			t.Errorf("missing x-api-key")
		}
		w.Header().Set("content-type", "application/json")
		if r.URL.Query().Get("page") == "1" {
			_, _ = w.Write([]byte(`{"data":{"items":[{"id":1,"group_ids":[7],"status":"error","credentials":{"email":"A@example.com","access_token":"SECRET"}},{"id":2,"group_ids":[7],"status":"error","extra":{"email":"a@example.com","refresh_token":"SECRET2"}}],"pages":2}}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":{"items":[],"pages":2}}`))
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "sub2api-monitor.json")
	started := 0
	s := NewMonitorService(New(Config{BaseURL: server.URL, APIKey: "secret"}), MonitorConfig{BaseURL: server.URL, GroupIDs: []int{7}, Enabled: true}, MonitorHooks{Evaluate: func(context.Context, string, []RemoteAccount) (RepairDecision, error) {
		return RepairDecision{Action: ActionStart}, nil
	}, StartRepair: func(_ context.Context, email string, accounts []RemoteAccount) error {
		started++
		if email != "a@example.com" || len(accounts) != 2 {
			return context.Canceled
		}
		return nil
	}}, path)
	sum, e := s.Check(context.Background(), "manual")
	if e != nil {
		t.Fatal(e)
	}
	if started != 1 || sum.Started != 2 {
		t.Fatalf("sum=%#v started=%d", sum, started)
	}
	b, _ := os.ReadFile(path)
	if strings.Contains(string(b), "SECRET") || strings.Contains(string(b), "access_token") {
		t.Fatalf("secret persisted: %s", b)
	}
	var state MonitorState
	_ = json.Unmarshal(b, &state)
	if state.LastResult == nil || len(state.LastResult.Error) > 0 {
		t.Fatalf("state=%#v", state)
	}
}

func TestMonitorServiceCooldownAndPermanentSkip(t *testing.T) {
	dir := t.TempDir()
	s := NewMonitorService(nil, MonitorConfig{Enabled: true, Cooldown: time.Hour}, MonitorHooks{}, filepath.Join(dir, "state.json"))
	s.markCooldown("a@example.com", "retry")
	s.markPermanent("b@example.com", "deleted")
	if d := s.decision(context.Background(), "a@example.com", nil); d.Action != ActionCooldown {
		t.Fatal(d)
	}
	if d := s.decision(context.Background(), "b@example.com", nil); d.Action != ActionBlocked {
		t.Fatal(d)
	}
}

func TestMonitorDisabledBusyAndSafeError(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		w.WriteHeader(502)
		_, _ = w.Write([]byte(`access_token=DO_NOT_LEAK`))
	}))
	defer server.Close()
	s := NewMonitorService(New(Config{BaseURL: server.URL, APIKey: "PRIVATE"}), MonitorConfig{BaseURL: server.URL}, MonitorHooks{}, "")
	if _, err := s.Check(context.Background(), "disabled"); err == nil {
		t.Fatal("disabled monitor ran")
	}
	if err := s.Configure(s.Client, MonitorConfig{BaseURL: server.URL, Enabled: true}, MonitorHooks{}); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := s.Check(context.Background(), "manual"); done <- err }()
	<-started
	if _, err := s.Check(context.Background(), "parallel"); err == nil {
		t.Fatal("parallel check ran")
	}
	if err := s.Configure(s.Client, MonitorConfig{}, MonitorHooks{}); err == nil {
		t.Fatal("changed config during check")
	}
	close(release)
	if err := <-done; err == nil {
		t.Fatal("remote failure ignored")
	}
	b, _ := json.Marshal(s.PublicState())
	if strings.Contains(string(b), "DO_NOT_LEAK") || strings.Contains(string(b), "PRIVATE") {
		t.Fatalf("leak: %s", b)
	}
	if s.PublicState().LastError != "Sub2API returned HTTP 502" {
		t.Fatal(s.PublicState().LastError)
	}
}

func TestMonitorConfigRestoreAndPartialPendingCount(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"data":{"items":[]}}`)) }))
	defer server.Close()
	statePath := filepath.Join(t.TempDir(), "sub2api-monitor.json")
	s := NewMonitorService(nil, MonitorConfig{}, MonitorHooks{}, statePath)
	cfg := MonitorConfig{BaseURL: server.URL, Enabled: true, GroupIDs: []int{8}, Cooldown: time.Hour}
	if err := s.Configure(New(Config{BaseURL: server.URL, APIKey: "SECRET"}), cfg, MonitorHooks{RetryPending: func(context.Context) (int, error) { return 2, context.DeadlineExceeded }}); err != nil {
		t.Fatal(err)
	}
	sum, err := s.Check(context.Background(), "manual")
	if err == nil || sum.Updated != 2 {
		t.Fatalf("partial result: %+v %v", sum, err)
	}
	restored := NewMonitorService(nil, MonitorConfig{}, MonitorHooks{}, statePath)
	if got := restored.Configuration(); !got.Enabled || got.BaseURL != server.URL || len(got.GroupIDs) != 1 || got.GroupIDs[0] != 8 {
		t.Fatal(got)
	}
	if err := s.Configure(New(Config{BaseURL: server.URL, APIKey: "SECRET"}), MonitorConfig{BaseURL: server.URL, Enabled: false}, MonitorHooks{}); err != nil {
		t.Fatal(err)
	}
	restored = NewMonitorService(nil, MonitorConfig{}, MonitorHooks{}, statePath)
	if restored.Configuration().Enabled {
		t.Fatal("disabled setting lost")
	}
}

func TestSyncAccountCredentialsMergesAndRequiresAllSteps(t *testing.T) {
	steps := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		steps = append(steps, r.Method+" "+r.URL.Path)
		switch {
		case r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"data":{"id":12,"credentials":{"refresh_token":"OLD","upstream_specific":true},"extra":{"existing":"preserved"}}}`))
		case r.Method == http.MethodPut:
			var payload map[string]any
			_ = json.NewDecoder(r.Body).Decode(&payload)
			c := payload["credentials"].(map[string]any)
			if c["refresh_token"] != "NEW" || c["upstream_specific"] != true {
				t.Errorf("credentials=%v", c)
			}
			e := payload["extra"].(map[string]any)
			if e["existing"] != "preserved" || e["codex_fingerprint_mode"] != "default" || e["openai_request_timezone"] != "Asia/Shanghai" {
				t.Errorf("extra=%v", e)
			}
			_, _ = w.Write([]byte(`{}`))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	defer server.Close()
	if err := New(Config{BaseURL: server.URL, APIKey: "test", OpenAIRequestTimezone: "Asia/Shanghai"}).SyncAccountCredentials(context.Background(), 12, map[string]any{"refresh_token": "NEW"}, "default"); err != nil {
		t.Fatal(err)
	}
	if len(steps) != 4 || !strings.HasSuffix(steps[2], "/clear-error") || !strings.HasSuffix(steps[3], "/schedulable") {
		t.Fatal(steps)
	}
}

func TestLegacyMonitorGoldenLoadsAndDropsPlaintextKeyOnSave(t *testing.T) {
	fixture, err := os.ReadFile("../../docs/fixtures/go-port-golden/sub2api-monitor.json")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "sub2api-monitor.json")
	if err := os.WriteFile(path, fixture, 0600); err != nil {
		t.Fatal(err)
	}
	monitor := NewMonitorService(nil, MonitorConfig{}, MonitorHooks{}, path)
	cfg := monitor.Configuration()
	if !cfg.Enabled || cfg.BaseURL != "https://sub2api.example" || len(cfg.GroupIDs) != 1 || cfg.GroupIDs[0] != 7 {
		t.Fatal(cfg)
	}
	if monitor.PublicState().LastResult == nil || monitor.PublicState().LastResult.Updated != 1 {
		t.Fatal(monitor.PublicState())
	}
	key := monitor.TakeLegacyAPIKey()
	if key != "fixture-admin-key" {
		t.Fatal("legacy key unavailable for secure migration")
	}
	if monitor.TakeLegacyAPIKey() != "" {
		t.Fatal("legacy key readable twice")
	}
	if err := monitor.Save(); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	if strings.Contains(string(b), key) || strings.Contains(string(b), "adminApiKey") {
		t.Fatal("key remained in normal monitor JSON")
	}
}
