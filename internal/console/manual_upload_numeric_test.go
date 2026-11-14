package console

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"

	"tosub2-go-port/internal/jobs"
)

// Exercise the exact string-valued settings emitted by the browser against
// an integer-typed management API, rather than accepting arbitrary JSON maps.
func TestManualUploadBrowserNumericOptions(t *testing.T) {
	for _, populated := range []bool{false, true} {
		name := "blank-browser-defaults"
		if populated {
			name = "populated-browser-settings"
		}
		t.Run(name, func(t *testing.T) {
			srv, manager, base := prepareSMSFlow(t, 2000)
			var accepted atomic.Int32
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet && r.URL.Path == "/api/v1/admin/accounts" {
					_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"items": []any{}, "total": 0}})
					return
				}
				if r.Method != http.MethodPost || r.URL.Path != "/api/v1/admin/accounts/batch" {
					t.Error("unexpected management route")
					w.WriteHeader(http.StatusNotFound)
					return
				}
				var body struct {
					Accounts []struct {
						GroupIDs    []int `json:"group_ids"`
						ProxyID     *int  `json:"proxy_id"`
						Concurrency *int  `json:"concurrency"`
						LoadFactor  *int  `json:"load_factor"`
						Priority    *int  `json:"priority"`
					} `json:"accounts"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				if len(body.Accounts) != 1 {
					t.Error("wrong account count")
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				a := body.Accounts[0]
				if populated && (!reflect.DeepEqual(a.GroupIDs, []int{7}) || a.ProxyID == nil || *a.ProxyID != 4 || a.Concurrency == nil || *a.Concurrency != 2 || a.LoadFactor == nil || *a.LoadFactor != 0 || a.Priority == nil || *a.Priority != 3) {
					t.Error("browser numeric settings were lost")
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				accepted.Add(1)
				_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"created": 1}})
			}))
			defer remote.Close()
			manager.SetRunner(func(_ context.Context, job *jobs.Job, _ func(jobs.JobEvent)) error {
				if err := writeFlowExport(srv.cfg.OutputRoot, job.ID, job.Email); err != nil {
					return err
				}
				manager.UpdateResult(job.ID, true, "")
				return nil
			})
			manager.Start()
			job, err := manager.Submit(jobs.Spec{ID: "first-upload", Email: "owner@example.test"})
			if err != nil {
				t.Fatal(err)
			}
			waitConsoleState(t, manager, job.ID, func(j *jobs.Job) bool { return j.Status == jobs.StatusSucceeded })
			settings := map[string]any{"baseUrl": remote.URL, "adminApiKey": "fixture-admin-key", "groupIds": []string{}, "proxyId": "", "concurrency": "", "loadFactor": "", "priority": "", "modelWhitelist": "", "codexFingerprintMode": "session"}
			if populated {
				settings["groupIds"], settings["proxyId"] = []string{"7"}, "4"
				settings["concurrency"], settings["loadFactor"], settings["priority"] = "2", "0", "3"
			}
			status, response := consoleHTTPPost(t, base, "/api/sub2api/upload", map[string]any{"ids": []string{job.ID}, "config": settings})
			if status != http.StatusOK || accepted.Load() != 1 || response["uploaded"] != float64(1) {
				t.Fatalf("first manual upload rejected: status=%d accepted=%d receipt=%v", status, accepted.Load(), response)
			}
		})
	}
}
