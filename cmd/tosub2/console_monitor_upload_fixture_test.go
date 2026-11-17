package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"tosub2-go-port/internal/config"
	"tosub2-go-port/internal/console"
	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/sub2api"
)

// Both authorizations run through the production runner and the local
// HTTP/Sentinel/OAuth fixture. The console must obtain automation evidence from
// the first login and later transfer the second grant to the original account.
func runConsoleMonitorUploadFixture(t *testing.T, cfg config.Config, manager *jobs.Manager, runner jobs.Runner, vault credentials.Store, email, password string) {
	t.Helper()
	t.Setenv("SUB2API_BASE_URL", "")
	t.Setenv("SUB2API_ADMIN_API_KEY", "")
	local := httptest.NewUnstartedServer(nil)
	address := local.Listener.Addr().(*net.TCPAddr)
	cfg.Host, cfg.Port = address.IP.String(), address.Port
	cfg.StartupToken = "synthetic-monitor-console-token"
	app := console.NewServer(cfg, manager, staticFiles, nil, vault)
	defer app.Close()
	manager.SetRunner(runner)
	manager.Start()
	local.Config.Handler = app.Handler()
	local.Start()
	defer local.Close()
	request := func(method, path string, body any, want int) []byte {
		t.Helper()
		var data []byte
		if body != nil {
			var err error
			data, err = json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
		}
		req, err := http.NewRequestWithContext(t.Context(), method, local.URL+path, bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-Console-Token", cfg.StartupToken)
		req.Header.Set("Origin", local.URL)
		req.Header.Set("Content-Type", "application/json")
		resp, err := local.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		data, err = io.ReadAll(resp.Body)
		if err != nil || resp.StatusCode != want {
			t.Fatalf("local %s %s: status=%d err=%v body=%s", method, path, resp.StatusCode, err, data)
		}
		return data
	}
	request("POST", "/api/jobs/batch", map[string]any{"text": email + "----" + password}, http.StatusCreated)
	list := manager.List()
	if len(list) != 1 || list[0].Status != "imported" {
		t.Fatal("HTTP import started authentication")
	}
	id := list[0].ID
	waitSaved := func(attempts int) *jobs.Job {
		t.Helper()
		deadline := time.Now().Add(20 * time.Second)
		for {
			job, _ := manager.Get(id)
			if job.Attempts >= attempts && job.FinishedAt != nil {
				if !job.ResultSaved || job.OperationStatus != "succeeded" || job.Status != jobs.StatusSucceeded {
					t.Fatalf("production OAuth runner failed: attempt=%d status=%s error=%s", job.Attempts, job.Status, job.Error)
				}
				if !job.LastAuthAutomated || !job.LastAuthRequirements["password"] {
					t.Fatal("production login did not record its reusable password automation evidence")
				}
				return job
			}
			if time.Now().After(deadline) {
				t.Fatalf("production OAuth runner did not finish: attempt=%d status=%s", job.Attempts, job.Status)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	request("POST", "/api/jobs/"+id+"/relogin", map[string]any{}, http.StatusOK)
	initial := waitSaved(1)
	if initial.Attempts != 1 || len(initial.AutoRepairPendingAccountIDs) != 0 {
		t.Fatal("initial login unexpectedly admitted an automatic repair")
	}
	readGrant := func(wantAccess, wantRefresh string) {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(cfg.OutputRoot, id, "sub2api-import-oauth.json"))
		var document struct {
			Accounts []struct {
				Credentials struct {
					AccessToken  string `json:"access_token"`
					RefreshToken string `json:"refresh_token"`
				} `json:"credentials"`
			} `json:"accounts"`
		}
		if err != nil || json.Unmarshal(data, &document) != nil || len(document.Accounts) != 1 || document.Accounts[0].Credentials.AccessToken != wantAccess || document.Accounts[0].Credentials.RefreshToken != wantRefresh {
			t.Fatal("the production runner did not commit the expected issued OAuth grant")
		}
	}
	readGrant("fixture-access", "fixture-refresh")
	var mu sync.Mutex
	var calls []string
	account := map[string]any{
		"id": 61, "name": "preserved-remote-name", "platform": "openai", "type": "oauth",
		"status": "error", "schedulable": false, "error_message": "upstream returned 401",
		"group_ids": []int{7}, "proxy_id": 3, "concurrency": 9, "priority": 0,
		"credentials": map[string]any{"email": email, "access_token": "expired-fixture-access", "refresh_token": "expired-fixture-refresh", "model_mapping": "preserved-routing"},
		"extra":       map[string]any{"custom_setting": "preserved"},
	}
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("x-api-key") != "synthetic-monitor-admin" {
			t.Error("monitor lost its selected service credential")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.Method + " " + r.URL.Path {
		case "GET /api/v1/admin/accounts":
			items := []any{}
			if account["status"] == "error" {
				items = append(items, account)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"items": items, "pages": 1, "total": len(items)}})
		case "GET /api/v1/admin/accounts/61":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": account})
		case "PUT /api/v1/admin/accounts/61":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error("monitor sent malformed authorization update")
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			creds, _ := body["credentials"].(map[string]any)
			extra, _ := body["extra"].(map[string]any)
			if len(body) != 2 || creds["email"] != email || creds["access_token"] != "fixture-access-repaired" || creds["refresh_token"] != "fixture-refresh-repaired" || creds["model_mapping"] != "preserved-routing" || extra["custom_setting"] != "preserved" {
				t.Error("monitor changed remote configuration or uploaded an obsolete/foreign grant")
			}
			account["credentials"], account["extra"] = creds, extra
			_ = json.NewEncoder(w).Encode(map[string]any{"data": account})
		case "POST /api/v1/admin/accounts/61/clear-error":
			account["status"], account["error_message"] = "active", ""
			_, _ = io.WriteString(w, `{"ok":true}`)
		case "POST /api/v1/admin/accounts/61/schedulable":
			var body map[string]any
			if json.NewDecoder(r.Body).Decode(&body) != nil || body["schedulable"] != true {
				t.Error("monitor did not restore scheduling after the confirmed credential update")
			}
			account["schedulable"] = true
			_, _ = io.WriteString(w, `{"ok":true}`)
		default:
			t.Errorf("unexpected monitor request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer remote.Close()
	request("POST", "/api/sub2api/monitor", map[string]any{"config": map[string]any{
		"baseUrl": remote.URL, "adminApiKey": "synthetic-monitor-admin", "groupIds": []int{7},
	}}, http.StatusOK)
	var state struct {
		Revision   int64                  `json:"revision"`
		LastResult sub2api.MonitorSummary `json:"lastResult"`
	}
	decodeState := func(data []byte) {
		t.Helper()
		if json.Unmarshal(data, &state) != nil {
			t.Fatal("monitor returned malformed state")
		}
	}
	decodeState(request("GET", "/api/pool-monitor", nil, http.StatusOK))
	decodeState(request("POST", "/api/pool-monitor", map[string]any{
		"revision": state.Revision, "target": "sub2api", "enabled": true,
		"excludedPlanTypes": []string{}, "intervalMinutes": 5, "cooldownMinutes": 5,
	}, http.StatusOK))
	check := func() {
		t.Helper()
		decodeState(request("POST", "/api/pool-monitor/check", map[string]any{"revision": state.Revision}, http.StatusOK))
	}
	check()
	if state.LastResult.Started != 1 || state.LastResult.Updated != 0 {
		t.Fatalf("first monitor pass did not queue the expired account: %+v", state.LastResult)
	}
	repaired := waitSaved(2)
	if repaired.Attempts != 2 || repaired.LastOperationType != "automatic_relogin" || !reflect.DeepEqual(repaired.AutoRepairPendingAccountIDs, []int{61}) || repaired.AutoRepairBackend != sub2api.BackendIdentity(remote.URL) || repaired.AutoRepairSyncPaused {
		t.Fatal("automatic relogin did not retain the original remote transfer intent")
	}
	readGrant("fixture-access-repaired", "fixture-refresh-repaired")
	assertNoTaskCheckpoint(t, filepath.Join(cfg.OutputRoot, id, "login-checkpoint.json"))
	check()
	completed, _ := manager.Get(id)
	if state.LastResult.Updated != 1 || state.LastResult.Started != 0 || len(completed.AutoRepairPendingAccountIDs) != 0 || completed.AutoRepairLastSuccessAt == nil || completed.AutoRepairSyncPaused || !completed.ResultSaved {
		t.Fatalf("next monitor pass did not commit the saved grant transfer: updated=%d started=%d pending=%d paused=%t", state.LastResult.Updated, state.LastResult.Started, len(completed.AutoRepairPendingAccountIDs), completed.AutoRepairSyncPaused)
	}
	check()
	after, _ := manager.Get(id)
	if state.LastResult.Started != 0 || state.LastResult.Updated != 0 || after.Attempts != 2 {
		t.Fatal("completed monitor repair repeated authorization or transfer")
	}
	public := request("GET", "/api/jobs/"+id, nil, http.StatusOK)
	for _, secret := range []string{password, "fixture-access", "fixture-refresh", "fixture-session"} {
		if bytes.Contains(public, []byte(secret)) {
			t.Fatal("monitor completion exposed authentication material in the public account")
		}
	}
	mu.Lock()
	defer mu.Unlock()
	wantCalls := []string{
		"GET /api/v1/admin/accounts", "GET /api/v1/admin/accounts/61", "PUT /api/v1/admin/accounts/61",
		"POST /api/v1/admin/accounts/61/clear-error", "POST /api/v1/admin/accounts/61/schedulable",
		"GET /api/v1/admin/accounts", "GET /api/v1/admin/accounts",
	}
	if !reflect.DeepEqual(calls, wantCalls) || account["name"] != "preserved-remote-name" || account["concurrency"] != 9 || account["priority"] != 0 || account["proxy_id"] != 3 || !reflect.DeepEqual(account["group_ids"], []int{7}) || account["status"] != "active" || account["schedulable"] != true {
		t.Fatalf("monitor repeated remote work or changed the original account configuration: calls=%v", calls)
	}
	t.Log("observed one initial password login, one automatic relogin, and one update/clear-error/schedulable sequence for the original remote account")
}
