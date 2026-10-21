package console

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/protocol"
	"tosub2-go-port/internal/store"
	"tosub2-go-port/internal/sub2api"
)

type transferPlanCalls struct {
	lists, details, mutations atomic.Int32
}

func (c *transferPlanCalls) total() int32 {
	return c.lists.Load() + c.details.Load() + c.mutations.Load()
}

func transferPlanAccount(method, plan string) map[string]any {
	clientID := protocol.DefaultCodexClientID
	if method == "bps" {
		clientID = protocol.DefaultBPSClientID
	}
	return map[string]any{
		"id": 61, "name": "oauth---transfer@example.test", "platform": "openai", "type": "oauth", "status": "error",
		"credentials": map[string]any{"email": "transfer@example.test", "access_token": "synthetic-transfer-access", "refresh_token": "synthetic-transfer-refresh", "client_id": clientID, "chatgpt_account_id": "synthetic-workspace", "plan_type": plan},
		"extra":       map[string]any{"plan_type": plan},
	}
}

func transferPlanRemote(t *testing.T, method string, mutationHook ...func(http.ResponseWriter, *http.Request) bool) (*httptest.Server, *transferPlanCalls) {
	t.Helper()
	calls := &transferPlanCalls{}
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		account := transferPlanAccount(method, "free")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/admin/accounts":
			calls.lists.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"items": []any{account}, "pages": 1, "total": 1}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/admin/accounts/61":
			calls.details.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"data": account})
		case r.Method == http.MethodPut && r.URL.Path == "/api/v1/admin/accounts/61",
			r.Method == http.MethodPost && (r.URL.Path == "/api/v1/admin/accounts/61/clear-error" || r.URL.Path == "/api/v1/admin/accounts/61/schedulable"):
			calls.mutations.Add(1)
			if len(mutationHook) > 0 && mutationHook[0](w, r) {
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		default:
			t.Errorf("unexpected remote request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	t.Cleanup(remote.Close)
	return remote, calls
}

func transferPlanJob(t *testing.T, srv *Server, manager *jobs.Manager, backend, method, cachedPlan, exportPlan string) *jobs.Job {
	t.Helper()
	job, err := manager.Submit(jobs.Spec{ID: "plan-transfer", Email: "transfer@example.test", Input: map[string]any{"authorizationMethod": method}})
	if err != nil {
		t.Fatal(err)
	}
	manager.Cancel(job.ID)
	writeMonitorOutputFixture(t, srv.cfg.OutputRoot, job.ID, []map[string]any{transferPlanAccount(method, cachedPlan)})
	manager.UpdateResult(job.ID, true, "new grant saved")
	manager.Emit(jobs.JobEvent{JobID: job.ID, Status: jobs.StatusSucceeded})
	manager.UpdateAutoRepair(job.ID, []int{61}, backend, "", false)
	writeMonitorOutputFixture(t, srv.cfg.OutputRoot, job.ID, []map[string]any{transferPlanAccount(method, exportPlan)})
	current, _ := manager.Get(job.ID)
	if current.PlanType != cachedPlan || !current.ResultSaved || current.Status != jobs.StatusSucceeded {
		t.Fatal("fixture did not retain the cached plan before fresh export inspection")
	}
	return current
}

func assertTransferPaused(t *testing.T, manager *jobs.Manager, job *jobs.Job, backend string) *jobs.Job {
	t.Helper()
	current, ok := manager.Get(job.ID)
	if !ok || current.Status != jobs.StatusSucceeded || !current.ResultSaved || current.AutoRepairSkippedReason == "" || current.AutoRepairLastSuccessAt != nil || current.AutoRepairBackend != backend || !reflect.DeepEqual(current.AutoRepairPendingAccountIDs, []int{61}) {
		t.Fatalf("policy pause discarded authorization/targets or reported completion: %+v", jobs.SanitizeJobForPublic(current))
	}
	return current
}

func TestMonitorTransferPlanExclusionPreventsAllRemoteRequests(t *testing.T) {
	for _, method := range []string{"chatgpt", "bps"} {
		for _, source := range []struct{ name, cached, exported string }{
			{"cached-free", "free", "plus"},
			{"fresh-export-free", "plus", "free"},
		} {
			t.Run(method+"/"+source.name, func(t *testing.T) {
				srv, manager := newMonitorTestServer(t)
				remote, calls := transferPlanRemote(t, method)
				backend := sub2api.BackendIdentity(remote.URL)
				job := transferPlanJob(t, srv, manager, backend, method, source.cached, source.exported)
				cfg := sub2api.Config{BaseURL: remote.URL, APIKey: "synthetic-admin", ExcludedPlanTypes: []string{"free"}}
				updated, err := srv.syncRepairOutput(t.Context(), cfg, job)
				if err != nil || updated != 0 || calls.total() != 0 {
					t.Fatalf("excluded transfer accessed the backend: updated=%d requests=%d err=%v", updated, calls.total(), err)
				}
				paused := assertTransferPaused(t, manager, job, backend)
				if n, err := srv.monitorHooks(cfg).RetryPending(t.Context()); err != nil || n != 0 || calls.total() != 0 {
					t.Fatalf("pending retry bypassed pause: updated=%d requests=%d err=%v", n, calls.total(), err)
				}
				if again := assertTransferPaused(t, manager, paused, backend); again.AutoRepairSkippedReason != paused.AutoRepairSkippedReason {
					t.Fatal("repeated pause lost its recorded reason")
				}
			})
		}
	}
}

func TestMonitorTransferPlanPauseSurvivesRestartAndResumesWithoutLogin(t *testing.T) {
	for _, method := range []string{"chatgpt", "bps"} {
		t.Run(method, func(t *testing.T) {
			srv, manager := newMonitorTestServer(t)
			remote, calls := transferPlanRemote(t, method)
			backend := sub2api.BackendIdentity(remote.URL)
			job := transferPlanJob(t, srv, manager, backend, method, "free", "free")
			cfg := sub2api.Config{BaseURL: remote.URL, APIKey: "synthetic-admin", ExcludedPlanTypes: []string{"free"}}
			if n, err := srv.monitorHooks(cfg).RetryPending(t.Context()); err != nil || n != 0 || calls.total() != 0 {
				t.Fatalf("initial pause failed: updated=%d requests=%d err=%v", n, calls.total(), err)
			}
			paused := assertTransferPaused(t, manager, job, backend)
			path := filepath.Join(srv.cfg.OutputRoot, job.ID, "sub2api-import-oauth.json")
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			srv.Close()
			if err := manager.Shutdown(t.Context()); err != nil {
				t.Fatal(err)
			}
			st, err := store.New(srv.cfg.OutputRoot)
			if err != nil {
				t.Fatal(err)
			}
			vault := &monitorMemoryVault{values: map[string]credentials.Credentials{}}
			restoredManager := jobs.NewManager(t.Context(), 1, st, nil)
			restoredManager.SetCredentialStore(vault)
			if err := restoredManager.Restore(t.Context()); err != nil {
				t.Fatal(err)
			}
			restored := NewServer(srv.cfg, restoredManager, embed.FS{}, nil, vault)
			t.Cleanup(func() { restored.Close(); _ = restoredManager.Shutdown(context.Background()) })
			var logins atomic.Int32
			restoredManager.SetRunner(func(context.Context, *jobs.Job, func(jobs.JobEvent)) error { logins.Add(1); return nil })
			restoredManager.Start()
			recovered := assertTransferPaused(t, restoredManager, job, backend)
			if recovered.AutoRepairSkippedReason != paused.AutoRepairSkippedReason || recovered.AuthorizationMethod != method {
				t.Fatal("restart lost pause reason or authorization identity")
			}
			if n, err := restored.monitorHooks(cfg).RetryPending(t.Context()); err != nil || n != 0 || calls.total() != 0 {
				t.Fatalf("restart bypassed pause: updated=%d requests=%d err=%v", n, calls.total(), err)
			}
			cfg.ExcludedPlanTypes = nil
			if n, err := restored.monitorHooks(cfg).RetryPending(t.Context()); err != nil || n != 1 {
				t.Fatalf("removing the exclusion did not finish the saved transfer: updated=%d err=%v", n, err)
			}
			completed, _ := restoredManager.Get(job.ID)
			if !completed.ResultSaved || completed.Status != jobs.StatusSucceeded || completed.AutoRepairSkippedReason != "" || len(completed.AutoRepairPendingAccountIDs) != 0 || completed.AutoRepairBackend != "" || completed.AutoRepairLastSuccessAt == nil || logins.Load() != 0 || calls.lists.Load() != 0 || calls.details.Load() != 1 || calls.mutations.Load() != 3 {
				t.Fatalf("resume lost state or repeated authorization: logins=%d lists=%d details=%d mutations=%d", logins.Load(), calls.lists.Load(), calls.details.Load(), calls.mutations.Load())
			}
			if after, err := os.ReadFile(path); err != nil || !bytes.Equal(before, after) {
				t.Fatal("pause/resume changed the durable authorization export")
			}
		})
	}
}

func TestMonitorSummarySeparatesPausedTransferFromCompletedUpload(t *testing.T) {
	srv, manager := newMonitorTestServer(t)
	remote, calls := transferPlanRemote(t, "bps")
	backend := sub2api.BackendIdentity(remote.URL)
	job := transferPlanJob(t, srv, manager, backend, "bps", "free", "free")
	cfg := sub2api.Config{BaseURL: remote.URL, APIKey: "synthetic-admin", ExcludedPlanTypes: []string{"free"}}
	if err := srv.configureMonitor(cfg, true, false); err != nil {
		t.Fatal(err)
	}
	summary, err := srv.monitorCheck(t.Context(), "manual")
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(summary)
	if err != nil {
		t.Fatal(err)
	}
	var public map[string]any
	if err := json.Unmarshal(encoded, &public); err != nil {
		t.Fatal(err)
	}
	if public["autoUploadPaused"] != float64(1) || summary.Updated != 0 || summary.Started != 0 || calls.lists.Load() != 1 || calls.details.Load() != 0 || calls.mutations.Load() != 0 {
		t.Fatalf("paused transfer was hidden or reported as success: summary=%s lists=%d details=%d mutations=%d", encoded, calls.lists.Load(), calls.details.Load(), calls.mutations.Load())
	}
	assertTransferPaused(t, manager, job, backend)
}

func TestManualUploadFreePlanRemainsAvailableWhileAutomaticTransferExcluded(t *testing.T) {
	for _, method := range []string{"chatgpt", "bps"} {
		t.Run(method, func(t *testing.T) {
			srv, _ := newMonitorTestServer(t)
			remote, calls := transferPlanRemote(t, method)
			if err := srv.configureMonitor(sub2api.Config{BaseURL: remote.URL, APIKey: "synthetic-admin", ExcludedPlanTypes: []string{"free"}}, true, false); err != nil {
				t.Fatal(err)
			}
			response := frontendRequest(srv, http.MethodPost, "/api/sub2api/upload", map[string]any{
				"config":  map[string]any{"baseUrl": remote.URL, "adminApiKey": "synthetic-admin", "excludedPlanTypes": []string{"free"}},
				"payload": map[string]any{"accounts": []any{transferPlanAccount(method, "free")}},
			}, "test")
			if response.Code != http.StatusOK || calls.lists.Load() != 1 || calls.details.Load() != 1 || calls.mutations.Load() != 3 {
				t.Fatalf("automatic exclusion blocked an explicit upload: HTTP=%d lists=%d details=%d mutations=%d", response.Code, calls.lists.Load(), calls.details.Load(), calls.mutations.Load())
			}
			if strings.Contains(response.Body.String(), "synthetic-transfer") || strings.Contains(response.Body.String(), "synthetic-admin") {
				t.Fatal("manual upload response leaked fixture credentials")
			}
		})
	}
}

func TestPausedManualUploadAcknowledgesConfirmedTargetsAndPreservesManualActions(t *testing.T) {
	for _, method := range []string{"chatgpt", "bps"} {
		t.Run(method, func(t *testing.T) {
			srv, manager := newMonitorTestServer(t)
			remote, calls := transferPlanRemote(t, method)
			backend := sub2api.BackendIdentity(remote.URL)
			job := transferPlanJob(t, srv, manager, backend, method, "free", "free")
			cfg := sub2api.Config{BaseURL: remote.URL, APIKey: "synthetic-admin", ExcludedPlanTypes: []string{"free"}}
			if err := srv.configureMonitor(cfg, true, false); err != nil {
				t.Fatal(err)
			}
			if n, err := srv.monitorHooks(cfg).RetryPending(t.Context()); err != nil || n != 0 || calls.total() != 0 {
				t.Fatalf("fixture did not pause automatic transfer: updated=%d requests=%d err=%v", n, calls.total(), err)
			}
			paused := assertTransferPaused(t, manager, job, backend)
			public := srv.publicJob(paused)
			reason, _ := public["autoRepairEligibilityReason"].(string)
			if public["autoRepairEligible"] != false || !strings.Contains(reason, "套餐") || public["canForceRelogin"] != true || public["canDownload"] != true {
				t.Fatal("plan exclusion hid manual actions or advertised automatic eligibility")
			}
			response := frontendRequest(srv, http.MethodPost, "/api/sub2api/upload", map[string]any{
				"ids": []string{job.ID}, "config": map[string]any{"baseUrl": remote.URL, "adminApiKey": "synthetic-admin"},
			}, "test")
			if response.Code != http.StatusOK || calls.lists.Load() != 1 || calls.details.Load() != 1 || calls.mutations.Load() != 3 {
				t.Fatalf("manual sync did not finish the account lifecycle: HTTP=%d lists=%d details=%d mutations=%d", response.Code, calls.lists.Load(), calls.details.Load(), calls.mutations.Load())
			}
			completed, _ := manager.Get(job.ID)
			if !completed.ResultSaved || completed.Status != jobs.StatusSucceeded || completed.AuthorizationMethod != method || completed.AutoRepairSkippedReason != "" || len(completed.AutoRepairPendingAccountIDs) != 0 || completed.AutoRepairBackend != "" || completed.AutoRepairLastSuccessAt == nil {
				t.Fatal("confirmed manual sync did not acknowledge the paused transfer")
			}
			if n, err := srv.monitorHooks(cfg).RetryPending(t.Context()); err != nil || n != 0 || calls.total() != 5 {
				t.Fatalf("acknowledged transfer was retried: updated=%d requests=%d err=%v", n, calls.total(), err)
			}
		})
	}
}

func TestManualUploadDoesNotClearUnconfirmedOrUnrelatedPausedTransfers(t *testing.T) {
	for _, scenario := range []string{"other-backend", "payload-only", "incomplete-ids", "no-confirmed-ids", "stale-snapshot", "new-operation", "rejected", "unknown"} {
		t.Run(scenario, func(t *testing.T) {
			srv, manager := newMonitorTestServer(t)
			var job *jobs.Job
			var backend string
			remote, calls := transferPlanRemote(t, "bps", func(w http.ResponseWriter, r *http.Request) bool {
				if !strings.HasSuffix(r.URL.Path, "/schedulable") {
					return false
				}
				switch scenario {
				case "rejected":
					w.WriteHeader(http.StatusBadRequest)
					return true
				case "unknown":
					w.WriteHeader(http.StatusServiceUnavailable)
					return true
				case "stale-snapshot":
					manager.UpdateAutoRepair(job.ID, []int{62}, backend, "", false)
					newer, _ := manager.Get(job.ID)
					if _, err := manager.PauseAutoRepairTransfer(newer, "newer plan pause"); err != nil {
						t.Error(err)
					}
				case "new-operation":
					// The scheduler is not started: queue only a synthetic newer
					// user intent while the older upload is still returning.
					if _, err := manager.QueueOperation(job.ID, "relogin", nil); err != nil {
						t.Error(err)
					}
				}
				return false
			})
			backend = sub2api.BackendIdentity(remote.URL)
			if scenario == "other-backend" {
				backend = sub2api.BackendIdentity("https://other-backend.example.test")
			}
			job = transferPlanJob(t, srv, manager, backend, "bps", "free", "free")
			pending := []int{61}
			if scenario == "incomplete-ids" {
				pending = []int{61, 62}
				manager.UpdateAutoRepair(job.ID, pending, backend, "", false)
				job, _ = manager.Get(job.ID)
			}
			paused, err := manager.PauseAutoRepairTransfer(job, "free plan pause")
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "no-confirmed-ids" {
				if err := srv.finishManualRepairTransfers([]*jobs.Job{paused}, map[string]any{"ok": true, "success": float64(1), "updated": float64(1)}); err != nil {
					t.Fatal(err)
				}
				current := assertTransferPaused(t, manager, job, backend)
				if current.AutoRepairSkippedReason != paused.AutoRepairSkippedReason || calls.total() != 0 {
					t.Fatal("aggregate success without confirmed IDs acknowledged the pause")
				}
				return
			}
			body := map[string]any{"ids": []string{job.ID}, "config": map[string]any{"baseUrl": remote.URL, "adminApiKey": "synthetic-admin"}}
			if scenario == "payload-only" {
				body["payload"] = map[string]any{"accounts": []any{transferPlanAccount("bps", "free")}}
			}
			response := frontendRequest(srv, http.MethodPost, "/api/sub2api/upload", body, "test")
			wantStatus := http.StatusOK
			if scenario == "rejected" || scenario == "unknown" {
				wantStatus = http.StatusBadGateway
			}
			if response.Code != wantStatus || calls.mutations.Load() != 3 {
				t.Fatalf("fixture did not reach expected manual result: HTTP=%d mutations=%d", response.Code, calls.mutations.Load())
			}
			wantReason := paused.AutoRepairSkippedReason
			if scenario == "stale-snapshot" {
				pending, wantReason = []int{62}, "newer plan pause"
			}
			current, _ := manager.Get(job.ID)
			if scenario == "new-operation" {
				if current.Status != jobs.StatusQueued || current.Operation != "relogin" || current.AutoRepairLastSuccessAt != nil || !current.ResultSaved || manager.QueuePosition(job.ID) <= 0 {
					t.Fatal("old upload acknowledgement overwrote the queued newer operation")
				}
				return
			}
			if !current.ResultSaved || current.AutoRepairBackend != backend || !reflect.DeepEqual(current.AutoRepairPendingAccountIDs, pending) || current.AutoRepairSkippedReason != wantReason || current.AutoRepairLastSuccessAt != nil {
				t.Fatal("manual result cleared an unrelated, unconfirmed, or superseded repair intent")
			}
		})
	}
}

func TestManualUploadAcknowledgementDiskFailureRetainsPauseAndReportsPartial(t *testing.T) {
	srv, manager := newMonitorTestServer(t)
	metadata := filepath.Join(srv.cfg.OutputRoot, "job-meta.json")
	backup := filepath.Join(srv.cfg.OutputRoot, "job-meta-before-upload.json")
	var injected atomic.Bool
	remote, calls := transferPlanRemote(t, "chatgpt", func(_ http.ResponseWriter, r *http.Request) bool {
		if strings.HasSuffix(r.URL.Path, "/schedulable") && injected.CompareAndSwap(false, true) {
			if err := os.Rename(metadata, backup); err != nil {
				t.Error(err)
				return false
			}
			if err := os.Mkdir(metadata, 0700); err != nil {
				t.Error(err)
			}
		}
		return false
	})
	t.Cleanup(func() {
		if injected.Load() {
			if info, err := os.Lstat(metadata); err == nil && info.IsDir() {
				_ = os.Remove(metadata)
			}
			if _, err := os.Stat(backup); err == nil {
				_ = os.Rename(backup, metadata)
			}
		}
	})
	backend := sub2api.BackendIdentity(remote.URL)
	job := transferPlanJob(t, srv, manager, backend, "chatgpt", "free", "free")
	paused, err := manager.PauseAutoRepairTransfer(job, "free plan pause")
	if err != nil {
		t.Fatal(err)
	}
	response := frontendRequest(srv, http.MethodPost, "/api/sub2api/upload", map[string]any{
		"ids": []string{job.ID}, "config": map[string]any{"baseUrl": remote.URL, "adminApiKey": "synthetic-admin"},
	}, "test")
	var result map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !injected.Load() || response.Code != http.StatusBadGateway || result["outcome"] != "partial" || !strings.Contains(response.Body.String(), "无需重新授权") || calls.mutations.Load() != 3 {
		t.Fatalf("failed local acknowledgement misreported remote completion: HTTP=%d mutations=%d", response.Code, calls.mutations.Load())
	}
	current := assertTransferPaused(t, manager, job, backend)
	if current.AutoRepairSkippedReason != paused.AutoRepairSkippedReason {
		t.Fatal("failed metadata write erased the retained pause reason")
	}
}
