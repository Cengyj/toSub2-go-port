package console

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/store"
	"tosub2-go-port/internal/sub2api"
)

func TestMonitorFiveMinuteDefaultAndConfiguredTimerCancellation(t *testing.T) {
	t.Setenv("SUB2API_MONITOR_INTERVAL_MS", "")
	t.Setenv("SUB2API_AUTO_REPAIR_COOLDOWN_MS", "")
	defaults, _ := newMonitorTestServer(t)
	if cfg := defaults.monitor.Configuration(); cfg.Interval != 5*time.Minute || cfg.Cooldown != 5*time.Minute {
		t.Fatal("legacy timer defaults changed", cfg.Interval, cfg.Cooldown)
	}
	t.Run("first_ui_configuration_honors_environment", func(t *testing.T) {
		t.Setenv("SUB2API_MONITOR_INTERVAL_MS", "1000")
		t.Setenv("SUB2API_AUTO_REPAIR_COOLDOWN_MS", "120")
		srv, _ := newMonitorTestServer(t)
		var calls atomic.Int32
		checked := make(chan time.Time, 16)
		remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			checked <- time.Now()
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"items": []any{}}})
		}))
		defer remote.Close()
		if err := srv.configureMonitor(sub2api.Config{BaseURL: remote.URL, APIKey: "fixture-admin-key"}, true, false); err != nil {
			t.Fatal(err)
		}
		if cfg := srv.monitor.Configuration(); cfg.Interval != time.Second || cfg.Cooldown != 120*time.Millisecond {
			t.Fatal("first configuration lost environment timing", cfg.Interval, cfg.Cooldown)
		}
		// Isolate the actual repeating timer from the configuration wakeup.
		select {
		case <-srv.monitorWake:
		default:
		}
		srv.StartMonitor(context.Background())
		var first, second time.Time
		select {
		case first = <-checked:
		case <-time.After(time.Second):
			t.Fatal("initial check missing")
		}
		select {
		case second = <-checked:
		case <-time.After(3 * time.Second):
			t.Fatal("periodic check missing")
		}
		if second.Sub(first) < time.Second {
			t.Fatal("checks did not use configured timer")
		}
		srv.Close()
		afterClose := calls.Load()
		time.Sleep(1100 * time.Millisecond)
		if calls.Load() != afterClose {
			t.Fatal("monitor timer survived service close")
		}
	})
}

func TestMonitorPartialTransferResumesAfterManagerAndServerRestart(t *testing.T) {
	t.Setenv("SUB2API_AUTO_REPAIR_COOLDOWN_MS", "20")
	srv, jm := newMonitorTestServer(t)
	vault := &lockedSMSVault{values: map[string]credentials.Credentials{}}
	srv.monitorVault = vault
	var fail atomic.Bool
	fail.Store(true)
	var mu sync.Mutex
	gets := map[int]int{}
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/admin/accounts" {
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"items": []any{}}})
			return
		}
		var id int
		_, _ = fmt.Sscanf(r.URL.Path, "/api/v1/admin/accounts/%d", &id)
		if r.Method == http.MethodGet {
			mu.Lock()
			gets[id]++
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": id, "credentials": map[string]any{"email": "owner@example.test"}}})
			return
		}
		if fail.Load() && r.URL.Path == "/api/v1/admin/accounts/2/schedulable" {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer remote.Close()
	cfg := sub2api.Config{BaseURL: remote.URL, APIKey: "fixture-admin-key"}
	if err := srv.configureMonitor(cfg, true, true); err != nil {
		t.Fatal(err)
	}
	j, _ := jm.Submit(jobs.Spec{ID: "restart-transfer", Email: "owner@example.test"})
	jm.Cancel(j.ID)
	jm.Emit(jobs.JobEvent{JobID: j.ID, Status: jobs.StatusSucceeded})
	jm.UpdateResult(j.ID, true, "")
	jm.UpdateAutoRepair(j.ID, []int{1, 2}, sub2api.BackendIdentity(remote.URL), "", false)
	if err := writeFlowExport(srv.cfg.OutputRoot, j.ID, j.Email); err != nil {
		t.Fatal(err)
	}
	current, _ := jm.Get(j.ID)
	if n, err := srv.syncRepairOutput(context.Background(), cfg, current); n != 1 || err == nil {
		t.Fatal("fixture did not persist a partial transfer", n, err)
	}
	srv.Close()
	if err := jm.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(25 * time.Millisecond)
	fail.Store(false)
	st, err := store.New(srv.cfg.OutputRoot)
	if err != nil {
		t.Fatal(err)
	}
	restoredManager := jobs.NewManager(context.Background(), 1, st, slog.Default())
	restoredManager.SetCredentialStore(vault)
	if err := restoredManager.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	var logins atomic.Int32
	restoredManager.SetRunner(func(context.Context, *jobs.Job, func(jobs.JobEvent)) error { logins.Add(1); return nil })
	restored := NewServer(srv.cfg, restoredManager, embed.FS{}, slog.Default(), vault)
	t.Cleanup(func() { restored.Close(); _ = restoredManager.Shutdown(context.Background()) })
	if !restored.monitor.Configuration().Enabled || restored.monitorConfig.APIKey != cfg.APIKey {
		t.Fatal("monitor configuration or secure key not restored")
	}
	restoredManager.Start()
	restored.StartMonitor(context.Background())
	waitConsoleState(t, restoredManager, j.ID, func(j *jobs.Job) bool {
		return len(j.AutoRepairPendingAccountIDs) == 0 && j.AutoRepairLastSuccessAt != nil
	})
	if logins.Load() != 0 {
		t.Fatal("pending transfer repeated authorization")
	}
	mu.Lock()
	if gets[1] != 1 || gets[2] != 2 {
		t.Error("restart repeated successful remote ID or skipped failed ID", gets)
	}
	mu.Unlock()
	for _, name := range []string{"job-meta.json", "sub2api-monitor.json"} {
		data, err := os.ReadFile(filepath.Join(srv.cfg.OutputRoot, name))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "fixture-admin-key") || strings.Contains(string(data), "synthetic-access") {
			t.Fatal("restart persisted credentials in ordinary metadata")
		}
	}
}

func writeFlowExport(root, id, email string) error {
	st, err := store.New(filepath.Join(root, id))
	if err != nil {
		return err
	}
	return st.WriteJSON(context.Background(), "sub2api-import-oauth.json", map[string]any{
		"type": "sub2api-data", "version": 1, "proxies": []any{},
		"accounts": []any{map[string]any{"name": email, "platform": "openai", "type": "oauth", "credentials": map[string]any{"email": email, "access_token": "synthetic-access", "refresh_token": "synthetic-refresh", "id_token": "synthetic-id"}}},
	})
}

func TestSub2APIUploadValidatesWholeSelectionAndLegacyCounts(t *testing.T) {
	srv, jm, base := prepareSMSFlow(t, 2000)
	var uploads atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/v1/admin/accounts" {
			if r.Header.Get("x-api-key") != "fixture-admin-key" {
				t.Error("missing admin authentication")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"items": []any{}, "pages": 1}})
			return
		}
		if r.URL.Path != "/api/v1/admin/accounts/batch" || r.Method != http.MethodPost {
			t.Error("wrong batch upload route")
		}
		if r.Header.Get("x-api-key") != "fixture-admin-key" || !strings.HasPrefix(r.Header.Get("Idempotency-Key"), "tosub2-upload-") {
			t.Error("missing admin authentication or idempotency")
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		accounts, _ := body["accounts"].([]any)
		if len(accounts) != 1 {
			t.Error("duplicate selection uploaded duplicate accounts")
		}
		uploads.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"created": 1}})
	}))
	defer remote.Close()
	jm.SetRunner(func(ctx context.Context, j *jobs.Job, _ func(jobs.JobEvent)) error {
		if j.ID == "upload-skipped" {
			<-ctx.Done()
			return ctx.Err()
		}
		if err := writeFlowExport(srv.cfg.OutputRoot, j.ID, j.Email); err != nil {
			return err
		}
		jm.UpdateResult(j.ID, true, "")
		return nil
	})
	jm.Start()
	complete, _ := jm.Submit(jobs.Spec{ID: "upload-complete", Email: "owner@example.test"})
	waitConsoleState(t, jm, complete.ID, func(j *jobs.Job) bool { return j.Status == jobs.StatusSucceeded })
	skipped, _ := jm.Submit(jobs.Spec{ID: "upload-skipped", Email: "other@example.test"})
	jm.Cancel(skipped.ID)
	waitConsoleState(t, jm, skipped.ID, func(j *jobs.Job) bool { return j.Status == jobs.StatusCanceled })
	config := map[string]any{"baseUrl": remote.URL, "adminApiKey": "fixture-admin-key", "groupIds": []int{7}}
	post := func(ids []string) (int, map[string]any) {
		return consoleHTTPPost(t, base, "/api/sub2api/upload", map[string]any{"ids": ids, "config": config})
	}
	status, response := post([]string{complete.ID, complete.ID, skipped.ID})
	if status != http.StatusOK || response["selected"] != float64(2) || response["uploaded"] != float64(1) || response["skipped"] != float64(1) {
		t.Fatal("legacy selection counts differ", status, response)
	}
	status, _ = post([]string{complete.ID, "missing"})
	if status != http.StatusNotFound || uploads.Load() != 1 {
		t.Fatal("missing selection performed partial upload")
	}
	if err := os.WriteFile(filepath.Join(srv.cfg.OutputRoot, complete.ID, "sub2api-import-oauth.json"), []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	status, _ = post([]string{complete.ID})
	if status != http.StatusConflict || uploads.Load() != 1 {
		t.Fatal("invalid output was silently skipped")
	}
}

func TestSub2APIMonitorConsoleManagerRepairAndUploadFlow(t *testing.T) {
	srv, jm, base := prepareSMSFlow(t, 2000)
	var runs atomic.Int32
	var mu sync.Mutex
	updated := map[int]int{}
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "fixture-admin-key" {
			t.Error("monitor admin authentication missing")
		}
		if r.URL.Path == "/api/v1/admin/accounts" {
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"items": []any{
				map[string]any{"id": 1, "email": "OWNER@example.test", "group_ids": []int{7}},
				map[string]any{"id": 2, "email": "owner@example.test", "group_ids": []int{7}},
			}}})
			return
		}
		var id int
		_, _ = fmt.Sscanf(r.URL.Path, "/api/v1/admin/accounts/%d", &id)
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": id, "credentials": map[string]any{"email": "owner@example.test", "existing": "keep"}, "extra": map[string]any{"retain": true}}})
			return
		}
		if r.Method == http.MethodPut && r.URL.Path == fmt.Sprintf("/api/v1/admin/accounts/%d", id) {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			creds, _ := body["credentials"].(map[string]any)
			if creds["access_token"] != "synthetic-access" || creds["existing"] != "keep" {
				t.Error("monitor did not merge refreshed credentials")
			}
			mu.Lock()
			updated[id]++
			mu.Unlock()
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer remote.Close()
	j, _ := jm.Submit(jobs.Spec{ID: "monitor-chain", Email: "owner@example.test", Input: map[string]any{"password": "synthetic-password"}})
	jm.Cancel(j.ID)
	jm.RecordAuthorization(j.ID, true, "", map[string]bool{"password": true})
	jm.SetRunner(func(ctx context.Context, job *jobs.Job, _ func(jobs.JobEvent)) error {
		runs.Add(1)
		if job.Operation != "relogin" {
			return fmt.Errorf("monitor queued operation %s", job.Operation)
		}
		if err := writeFlowExport(srv.cfg.OutputRoot, job.ID, job.Email); err != nil {
			return err
		}
		jm.RecordAuthorization(job.ID, true, "", map[string]bool{"password": true})
		jm.UpdateResult(job.ID, true, "")
		return nil
	})
	status, response := consoleHTTPPost(t, base, "/api/sub2api/monitor", map[string]any{"enabled": true, "config": map[string]any{"baseUrl": remote.URL, "adminApiKey": "fixture-admin-key", "groupIds": []int{7}}})
	if status != http.StatusOK {
		t.Fatal(status, response)
	}
	status, response = consoleHTTPPost(t, base, "/api/sub2api/monitor/check", map[string]any{})
	if status != http.StatusOK || response["result"].(map[string]any)["started"] != float64(2) {
		t.Fatal("monitor did not queue both remote IDs", status, response)
	}
	queued, _ := jm.Get(j.ID)
	if queued.Status != jobs.StatusQueued || len(queued.AutoRepairPendingAccountIDs) != 2 {
		t.Fatal("repair target binding was lost")
	}
	decision, err := srv.monitorHooks(sub2api.Config{BaseURL: remote.URL}).Evaluate(context.Background(), j.Email, nil)
	if err != nil || decision.Action != sub2api.ActionBusy {
		t.Fatal("queued repair not classified busy", decision, err)
	}
	jm.Start()
	waitConsoleState(t, jm, j.ID, func(j *jobs.Job) bool { return j.Status == jobs.StatusSucceeded && j.ResultSaved })
	status, response = consoleHTTPPost(t, base, "/api/sub2api/monitor/check", map[string]any{})
	if status != http.StatusOK || response["result"].(map[string]any)["updated"] != float64(2) {
		t.Fatal("completed authorization not transferred", status, response)
	}
	current, _ := jm.Get(j.ID)
	if runs.Load() != 1 || len(current.AutoRepairPendingAccountIDs) != 0 || current.AutoRepairLastSuccessAt == nil {
		t.Fatal("repair did not finalize or repeated login")
	}
	if srv.publicJob(current)["lastOperationType"] != "automatic_relogin" {
		t.Fatal("completed automatic repair lost its visible operation label")
	}
	mu.Lock()
	defer mu.Unlock()
	if updated[1] != 1 || updated[2] != 1 {
		t.Fatal("remote IDs not updated exactly once", updated)
	}
}

func TestMonitorPendingUploadUsesOnlyLatestAccountTask(t *testing.T) {
	srv, jm := newMonitorTestServer(t)
	var requests atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer remote.Close()
	old, _ := jm.Submit(jobs.Spec{ID: "old", Email: "owner@example.test"})
	jm.Cancel(old.ID)
	jm.Emit(jobs.JobEvent{JobID: old.ID, Status: jobs.StatusSucceeded})
	jm.UpdateResult(old.ID, true, "")
	jm.UpdateAutoRepair(old.ID, []int{3}, sub2api.BackendIdentity(remote.URL), "", false)
	if err := writeFlowExport(srv.cfg.OutputRoot, old.ID, old.Email); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Millisecond)
	latest, _ := jm.Submit(jobs.Spec{ID: "latest", Email: "OWNER@example.test"})
	jm.Cancel(latest.ID)
	n, err := srv.monitorHooks(sub2api.Config{BaseURL: remote.URL, APIKey: "fixture"}).RetryPending(context.Background())
	if err != nil || n != 0 || requests.Load() != 0 {
		t.Fatal("historical authorization was retransferred", n, err)
	}
	jm.UpdateAutoRepair(latest.ID, []int{3}, sub2api.BackendIdentity(remote.URL), "account_disabled", true)
	blocked, _ := jm.Get(latest.ID)
	if !blocked.AutoRepairBlocked || blocked.AutoRepairBlockedReason != "account_disabled" || len(blocked.AutoRepairPendingAccountIDs) != 0 || blocked.AutoRepairBackend != "" {
		t.Fatal("permanent failure did not clear pending repair targets")
	}
}
