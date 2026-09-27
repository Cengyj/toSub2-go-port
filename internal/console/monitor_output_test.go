package console

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/sub2api"
)

func TestMonitorMixedAccountOutputSelectsTaskIdentityAndPreservesRemoteFields(t *testing.T) {
	for _, identity := range []string{"credentials", "extra", "name", "legacy-anonymous"} {
		t.Run(identity, func(t *testing.T) {
			srv, manager := newMonitorTestServer(t)
			const email = "owner@example.test"
			var mu sync.Mutex
			steps := map[int][]string{}
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("x-api-key") != "fixture-admin-key" {
					t.Error("monitor admin authentication was not preserved")
				}
				if r.URL.Path == "/api/v1/admin/accounts" {
					_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"items": []any{}}})
					return
				}
				var id int
				_, _ = fmt.Sscanf(r.URL.Path, "/api/v1/admin/accounts/%d", &id)
				mu.Lock()
				steps[id] = append(steps[id], r.Method+" "+r.URL.Path)
				mu.Unlock()
				switch r.Method {
				case http.MethodGet:
					_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
						"id": id, "name": "existing label", "group_ids": []int{17}, "proxy_id": 23,
						"credentials": map[string]any{"email": email, "access_token": "fixture-old-access", "provider_options": map[string]any{"record": id, "nested": true}},
						"extra":       map[string]any{"retained": map[string]any{"record": id}, "codex_fingerprint_mode": "old"},
					}})
				case http.MethodPut:
					var payload map[string]any
					if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
						t.Error(err)
					}
					credentials, _ := payload["credentials"].(map[string]any)
					extra, _ := payload["extra"].(map[string]any)
					if len(payload) != 2 || credentials["email"] != email || credentials["access_token"] != "fixture-owner-access" || credentials["refresh_token"] != "fixture-owner-refresh" {
						t.Error("wrong account selected or unrelated remote fields overwritten")
					}
					if !reflect.DeepEqual(credentials["provider_options"], map[string]any{"record": float64(id), "nested": true}) || !reflect.DeepEqual(extra["retained"], map[string]any{"record": float64(id)}) || extra["codex_fingerprint_mode"] != "session" {
						t.Error("remote provider-specific credentials or extra fields were lost")
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
				default:
					_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
				}
			}))
			defer remote.Close()
			cfg := sub2api.Config{BaseURL: remote.URL, APIKey: "fixture-admin-key", CodexFingerprintMode: "session"}
			if err := srv.configureMonitor(cfg, true, false); err != nil {
				t.Fatal(err)
			}
			job, err := manager.Submit(jobs.Spec{ID: "mixed-output", Email: email})
			if err != nil {
				t.Fatal(err)
			}
			manager.Cancel(job.ID)
			manager.Emit(jobs.JobEvent{JobID: job.ID, Status: jobs.StatusSucceeded})
			manager.UpdateResult(job.ID, true, "")
			manager.UpdateAutoRepair(job.ID, []int{61, 62}, sub2api.BackendIdentity(remote.URL), "", false)
			owner := map[string]any{"credentials": map[string]any{"access_token": "fixture-owner-access", "refresh_token": "fixture-owner-refresh"}}
			switch identity {
			case "credentials":
				owner["credentials"].(map[string]any)["email"] = " OWNER@EXAMPLE.TEST "
			case "extra":
				owner["extra"] = map[string]any{"email": email}
			case "name":
				owner["name"] = "Existing account " + email
			}
			accounts := []map[string]any{
				{"name": "other@example.test", "credentials": map[string]any{"access_token": "fixture-unrelated-access"}},
				owner,
			}
			if identity == "legacy-anonymous" {
				accounts = []map[string]any{owner}
			}
			writeMonitorOutputFixture(t, srv.cfg.OutputRoot, job.ID, accounts)
			response := runMonitorOutputCheck(srv)
			if response.Code != http.StatusOK {
				t.Fatalf("monitor returned %d: %s", response.Code, response.Body.String())
			}
			var result struct {
				Result sub2api.MonitorSummary
			}
			if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || result.Result.Updated != 2 {
				t.Fatalf("repair summary did not report both remote records: %s", response.Body.String())
			}
			current, _ := manager.Get(job.ID)
			if len(current.AutoRepairPendingAccountIDs) != 0 || current.AutoRepairLastSuccessAt == nil {
				t.Fatal("completed remote updates were left pending")
			}
			mu.Lock()
			defer mu.Unlock()
			for _, id := range []int{61, 62} {
				base := fmt.Sprintf("/api/v1/admin/accounts/%d", id)
				if !reflect.DeepEqual(steps[id], []string{"GET " + base, "PUT " + base, "POST " + base + "/clear-error", "POST " + base + "/schedulable"}) {
					t.Fatal("remote update lifecycle order changed", steps[id])
				}
			}
		})
	}
}

func TestMonitorOutputRejectsUnrelatedAndAmbiguousAccountCredentials(t *testing.T) {
	for _, fixture := range []struct {
		name     string
		accounts []map[string]any
	}{
		{"different-account", []map[string]any{{"name": "other@example.test", "credentials": map[string]any{"access_token": "fixture-unrelated-access"}}}},
		{"ambiguous-anonymous", []map[string]any{{"credentials": map[string]any{"access_token": "fixture-first-access"}}, {"credentials": map[string]any{"access_token": "fixture-second-access"}}}},
		{"matching-account-without-credentials", []map[string]any{{"credentials": map[string]any{"email": "other@example.test", "access_token": "fixture-unrelated-access"}}, {"name": "owner@example.test"}}},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			srv, manager := newMonitorTestServer(t)
			var mu sync.Mutex
			accountRequests := 0
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/v1/admin/accounts" {
					mu.Lock()
					accountRequests++
					mu.Unlock()
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"items": []any{}}})
			}))
			defer remote.Close()
			cfg := sub2api.Config{BaseURL: remote.URL, APIKey: "fixture-admin-key"}
			if err := srv.configureMonitor(cfg, true, false); err != nil {
				t.Fatal(err)
			}
			job, err := manager.Submit(jobs.Spec{ID: "unrelated-output", Email: "owner@example.test"})
			if err != nil {
				t.Fatal(err)
			}
			manager.Cancel(job.ID)
			manager.Emit(jobs.JobEvent{JobID: job.ID, Status: jobs.StatusSucceeded})
			manager.UpdateResult(job.ID, true, "")
			manager.UpdateAutoRepair(job.ID, []int{61}, sub2api.BackendIdentity(remote.URL), "", false)
			writeMonitorOutputFixture(t, srv.cfg.OutputRoot, job.ID, fixture.accounts)
			response := runMonitorOutputCheck(srv)
			if response.Code != http.StatusBadGateway || strings.Contains(response.Body.String(), "fixture-") {
				t.Fatal("invalid output did not fail safely")
			}
			current, _ := manager.Get(job.ID)
			if !reflect.DeepEqual(current.AutoRepairPendingAccountIDs, []int{61}) || current.AutoRepairLastSuccessAt != nil {
				t.Fatal("rejected output was marked transferred")
			}
			mu.Lock()
			defer mu.Unlock()
			if accountRequests != 0 {
				t.Fatal("unrelated credentials reached the remote account endpoint")
			}
		})
	}
}

func writeMonitorOutputFixture(t *testing.T, root, id string, accounts []map[string]any) {
	t.Helper()
	dir := filepath.Join(root, id)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(map[string]any{"type": "sub2api-data", "version": 1, "accounts": accounts})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub2api-import-oauth.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
}

func runMonitorOutputCheck(srv *Server) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, "/api/sub2api/monitor/check", nil)
	request.Host = "127.0.0.1"
	request.Header.Set("X-Console-Token", "test")
	response := httptest.NewRecorder()
	srv.Handler().ServeHTTP(response, request)
	return response
}
