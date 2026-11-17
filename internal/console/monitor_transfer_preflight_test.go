package console

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"

	"tosub2-go-port/internal/accountlock"
	"tosub2-go-port/internal/sub2api"
)

// A read-only query or another process can own the account when the next
// monitor pass picks up a freshly saved authorization. No remote write has
// started at that point, so contention must not create an uncertainty fence.
func TestMonitorTransferRetriesAfterAccountPreflightContention(t *testing.T) {
	for _, ownership := range []string{"query", "os-lock"} {
		t.Run(ownership, func(t *testing.T) {
			t.Setenv("SUB2API_AUTO_REPAIR_COOLDOWN_MS", "0")
			srv, manager := newMonitorTestServer(t)
			var details, mutations atomic.Int32
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/api/v1/admin/accounts":
					_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"items": []any{}}})
				case r.Method == http.MethodGet && (r.URL.Path == "/api/v1/admin/accounts/61" || r.URL.Path == "/api/v1/admin/accounts/62"):
					details.Add(1)
					id := 61
					if r.URL.Path == "/api/v1/admin/accounts/62" {
						id = 62
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
						"id": id, "credentials": map[string]any{"email": "owner@example.test"},
					}})
				case r.Method == http.MethodPut && (r.URL.Path == "/api/v1/admin/accounts/61" || r.URL.Path == "/api/v1/admin/accounts/62"),
					r.Method == http.MethodPost && (r.URL.Path == "/api/v1/admin/accounts/61/clear-error" || r.URL.Path == "/api/v1/admin/accounts/62/clear-error" || r.URL.Path == "/api/v1/admin/accounts/61/schedulable" || r.URL.Path == "/api/v1/admin/accounts/62/schedulable"):
					mutations.Add(1)
					_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
				default:
					t.Errorf("unexpected remote request: %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusBadRequest)
				}
			}))
			defer remote.Close()
			cfg := sub2api.Config{BaseURL: remote.URL, APIKey: "synthetic-admin"}
			if err := srv.configureMonitor(cfg, true, false); err != nil {
				t.Fatal(err)
			}
			job := monitorPendingFixture(t, srv, manager, sub2api.BackendIdentity(remote.URL))
			var release func()
			if ownership == "query" {
				lease, err := manager.ReserveAccountQuery(t.Context(), job.ID)
				if err != nil {
					t.Fatal(err)
				}
				release = lease.Release
			} else {
				unlock, err := accountlock.Acquire("https://chatgpt.com", job.Email)
				if err != nil {
					t.Fatal(err)
				}
				release = func() {
					if err := unlock(); err != nil {
						t.Error("fixture account lock release failed", err)
					}
				}
			}
			defer release()
			first := runMonitorOutputCheck(srv)
			blocked, _ := manager.Get(job.ID)
			if first.Code != http.StatusBadGateway || details.Load() != 0 || mutations.Load() != 0 {
				t.Fatalf("busy preflight reached remote account: status=%d details=%d mutations=%d", first.Code, details.Load(), mutations.Load())
			}
			if blocked.AutoRepairSyncPaused || !reflect.DeepEqual(blocked.AutoRepairPendingAccountIDs, job.AutoRepairPendingAccountIDs) || blocked.AutoRepairLastSuccessAt != nil || !blocked.ResultSaved {
				t.Error("account contention fenced an unattempted transfer or discarded saved authorization")
			}
			release()
			second := runMonitorOutputCheck(srv)
			completed, _ := manager.Get(job.ID)
			if second.Code != http.StatusOK || completed.AutoRepairSyncPaused || len(completed.AutoRepairPendingAccountIDs) != 0 || completed.AutoRepairLastSuccessAt == nil {
				t.Errorf("next monitor pass did not finish after contention ended: status=%d paused=%t pending=%d", second.Code, completed.AutoRepairSyncPaused, len(completed.AutoRepairPendingAccountIDs))
			}
			if details.Load() != 2 || mutations.Load() != 6 {
				t.Errorf("saved grant did not complete each lifecycle once: details=%d mutations=%d", details.Load(), mutations.Load())
			}
			beforeDetails, beforeMutations := details.Load(), mutations.Load()
			if third := runMonitorOutputCheck(srv); third.Code != http.StatusOK || details.Load() != beforeDetails || mutations.Load() != beforeMutations {
				t.Error("completed transfer was repeated")
			}
		})
	}
}

func TestMonitorTransferFencePersistenceFailurePreventsRemoteWrite(t *testing.T) {
	t.Setenv("SUB2API_AUTO_REPAIR_COOLDOWN_MS", "0")
	srv, manager := newMonitorTestServer(t)
	var details, mutations atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/admin/accounts" {
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"items": []any{}}})
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/api/v1/admin/accounts/61" {
			details.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": 61, "credentials": map[string]any{"email": "owner@example.test"}}})
			return
		}
		mutations.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer remote.Close()
	cfg := sub2api.Config{BaseURL: remote.URL, APIKey: "synthetic-admin"}
	if err := srv.configureMonitor(cfg, true, false); err != nil {
		t.Fatal(err)
	}
	job := monitorPendingFixture(t, srv, manager, sub2api.BackendIdentity(remote.URL))
	manager.UpdateAutoRepair(job.ID, []int{61}, sub2api.BackendIdentity(remote.URL), "", false)
	// An occupied destination makes atomic replacement fail without touching
	// the grant, settings, or account-lease files used during preparation.
	path := filepath.Join(srv.cfg.OutputRoot, "job-meta.json")
	if err := os.Rename(path, path+".fixture-backup"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	reply := runMonitorOutputCheck(srv)
	after, _ := manager.Get(job.ID)
	if reply.Code != http.StatusBadGateway || details.Load() != 1 || mutations.Load() != 0 || !after.AutoRepairSyncPaused || len(after.AutoRepairPendingAccountIDs) != 1 || after.AutoRepairLastSuccessAt != nil {
		t.Errorf("failed fence save admitted remote work: status=%d details=%d mutations=%d paused=%t pending=%d", reply.Code, details.Load(), mutations.Load(), after.AutoRepairSyncPaused, len(after.AutoRepairPendingAccountIDs))
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path+".fixture-backup", path); err != nil {
		t.Fatal(err)
	}
	if next := runMonitorOutputCheck(srv); next.Code != http.StatusOK || details.Load() != 1 || mutations.Load() != 0 {
		t.Error("failed fence was silently re-admitted after storage recovered")
	}
}

func TestMonitorTransferRetriesAfterReadOnlyLookupFailure(t *testing.T) {
	t.Setenv("SUB2API_AUTO_REPAIR_COOLDOWN_MS", "0")
	srv, manager := newMonitorTestServer(t)
	var reject atomic.Bool
	reject.Store(true)
	var details, mutations atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/admin/accounts" {
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"items": []any{}}})
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/api/v1/admin/accounts/61" {
			details.Add(1)
			if reject.Load() {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": 61, "credentials": map[string]any{"email": "owner@example.test"}}})
			return
		}
		mutations.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer remote.Close()
	cfg := sub2api.Config{BaseURL: remote.URL, APIKey: "synthetic-admin"}
	if err := srv.configureMonitor(cfg, true, false); err != nil {
		t.Fatal(err)
	}
	job := monitorPendingFixture(t, srv, manager, sub2api.BackendIdentity(remote.URL))
	manager.UpdateAutoRepair(job.ID, []int{61}, sub2api.BackendIdentity(remote.URL), "", false)
	first := runMonitorOutputCheck(srv)
	blocked, _ := manager.Get(job.ID)
	if first.Code != http.StatusBadGateway || details.Load() != 1 || mutations.Load() != 0 {
		t.Fatalf("failed detail lookup performed writes: status=%d details=%d mutations=%d", first.Code, details.Load(), mutations.Load())
	}
	if blocked.AutoRepairSyncPaused || blocked.AutoRepairLastError == "" || len(blocked.AutoRepairPendingAccountIDs) != 1 || !blocked.ResultSaved {
		t.Error("read-only failure fenced an unattempted transfer or discarded recovery state")
	}
	reject.Store(false)
	second := runMonitorOutputCheck(srv)
	completed, _ := manager.Get(job.ID)
	if second.Code != http.StatusOK || completed.AutoRepairSyncPaused || len(completed.AutoRepairPendingAccountIDs) != 0 || completed.AutoRepairLastSuccessAt == nil || details.Load() != 2 || mutations.Load() != 3 {
		t.Errorf("lookup recovery failed to transfer pending grant: status=%d paused=%t pending=%d details=%d mutations=%d", second.Code, completed.AutoRepairSyncPaused, len(completed.AutoRepairPendingAccountIDs), details.Load(), mutations.Load())
	}
}

func TestMonitorTransferPreflightFailureRespectsCooldown(t *testing.T) {
	t.Setenv("SUB2API_AUTO_REPAIR_COOLDOWN_MS", "60000")
	srv, manager := newMonitorTestServer(t)
	var details, mutations atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/admin/accounts" {
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"items": []any{}}})
			return
		}
		if r.Method == http.MethodGet {
			details.Add(1)
		} else {
			mutations.Add(1)
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer remote.Close()
	if err := srv.configureMonitor(sub2api.Config{BaseURL: remote.URL, APIKey: "synthetic-admin"}, true, false); err != nil {
		t.Fatal(err)
	}
	job := monitorPendingFixture(t, srv, manager, sub2api.BackendIdentity(remote.URL))
	if reply := runMonitorOutputCheck(srv); reply.Code != http.StatusBadGateway {
		t.Fatalf("fresh grant did not attempt its initial transfer: status=%d", reply.Code)
	}
	failed, _ := manager.Get(job.ID)
	if failed.AutoRepairSyncPaused || failed.AutoRepairLastError == "" || failed.AutoRepairLastAttemptAt == nil {
		t.Fatal("unattempted failure lost retry bookkeeping")
	}
	if reply := runMonitorOutputCheck(srv); reply.Code != http.StatusOK || details.Load() != 1 || mutations.Load() != 0 {
		t.Fatalf("cooldown failed to defer read-only retry: status=%d details=%d mutations=%d", reply.Code, details.Load(), mutations.Load())
	}
	after, _ := manager.Get(job.ID)
	if !reflect.DeepEqual(failed, after) {
		t.Fatal("cooldown changed the saved authorization or pending transfer")
	}
}

func TestMonitorTransferFencesLifecycleWhenRemoteGrantAlreadyMatches(t *testing.T) {
	for _, loseReceipt := range []bool{false, true} {
		name := "confirmed"
		if loseReceipt {
			name = "unknown"
		}
		t.Run(name, func(t *testing.T) {
			t.Setenv("SUB2API_AUTO_REPAIR_COOLDOWN_MS", "0")
			srv, manager := newMonitorTestServer(t)
			var updates, lifecycle atomic.Int32
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/api/v1/admin/accounts" {
					_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"items": []any{}}})
					return
				}
				if r.Method == http.MethodGet && r.URL.Path == "/api/v1/admin/accounts/61" {
					_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
						"id": 61, "status": "error",
						"credentials": map[string]any{"email": "owner@example.test", "access_token": "synthetic-access", "refresh_token": "synthetic-refresh", "id_token": "synthetic-id"},
						"extra":       map[string]any{"codex_fingerprint_mode": "session", "openai_request_timezone": sub2api.DefaultOpenAIRequestTimezone},
					}})
					return
				}
				if r.Method == http.MethodPut {
					updates.Add(1)
					t.Error("matching authorization was needlessly written again")
				}
				if r.Method == http.MethodPost && (r.URL.Path == "/api/v1/admin/accounts/61/clear-error" || r.URL.Path == "/api/v1/admin/accounts/61/schedulable") {
					lifecycle.Add(1)
					current, _ := manager.Get("in-flight-repair")
					if !current.AutoRepairSyncPaused {
						t.Error("remote lifecycle began without the live restart fence")
					}
					var saved struct {
						Jobs []struct {
							ID     string `json:"id"`
							Paused bool   `json:"auto_repair_sync_paused"`
						} `json:"jobs"`
					}
					data, err := os.ReadFile(filepath.Join(srv.cfg.OutputRoot, "job-meta.json"))
					if err != nil || json.Unmarshal(data, &saved) != nil || len(saved.Jobs) != 1 || saved.Jobs[0].ID != current.ID || !saved.Jobs[0].Paused {
						t.Error("remote lifecycle began without the persisted restart fence")
					}
					if loseReceipt {
						w.WriteHeader(http.StatusServiceUnavailable)
						return
					}
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
			}))
			defer remote.Close()
			cfg := sub2api.Config{BaseURL: remote.URL, APIKey: "synthetic-admin", CodexFingerprintMode: "session"}
			if err := srv.configureMonitor(cfg, true, false); err != nil {
				t.Fatal(err)
			}
			job := monitorPendingFixture(t, srv, manager, sub2api.BackendIdentity(remote.URL))
			manager.UpdateAutoRepair(job.ID, []int{61}, sub2api.BackendIdentity(remote.URL), "", false)
			reply := runMonitorOutputCheck(srv)
			after, _ := manager.Get(job.ID)
			if loseReceipt {
				if reply.Code != http.StatusBadGateway || !after.AutoRepairSyncPaused || len(after.AutoRepairPendingAccountIDs) != 1 || after.AutoRepairLastSuccessAt != nil || lifecycle.Load() != 1 {
					t.Error("unconfirmed lifecycle mutation lost the retained transfer fence")
				}
			} else if reply.Code != http.StatusOK || after.AutoRepairSyncPaused || len(after.AutoRepairPendingAccountIDs) != 0 || after.AutoRepairLastSuccessAt == nil || lifecycle.Load() != 2 {
				t.Error("confirmed lifecycle completion did not release the fence")
			}
			before := lifecycle.Load()
			if next := runMonitorOutputCheck(srv); next.Code != http.StatusOK || lifecycle.Load() != before || updates.Load() != 0 {
				t.Error("monitor replayed an already attempted lifecycle")
			}
		})
	}
}
