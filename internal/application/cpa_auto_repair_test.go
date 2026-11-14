package application

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"tosub2-go-port/internal/cpa"
	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/protocol"
	"tosub2-go-port/internal/sub2api"
)

func TestCPAAutomaticReloginRetainsGrantAndRechecksUploadPolicy(t *testing.T) {
	for _, scenario := range []string{"success", "disabled", "new-plan", "gate-policy-change", "identity", "unknown", "permanent-auth", "remote-disabled", "remote-missing", "remote-recovered", "remote-plan", "remote-identity", "remote-read-failed", "read-policy-change", "read-quality-change"} {
		t.Run(scenario, func(t *testing.T) {
			manager, _, initial, root := workflowPanicFixture(t)
			stableFile, err := cpa.BuildAuthFile(map[string]any{"platform": "openai", "type": "oauth", "credentials": map[string]any{
				"email": initial.Email, "client_id": protocol.DefaultCodexClientID, "access_token": "old-access", "refresh_token": "old-refresh", "chatgpt_account_id": "workspace-cpa",
			}})
			if err != nil {
				t.Fatal(err)
			}
			var writes, reads atomic.Int32
			var qualityRequired atomic.Bool
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet && r.URL.Path == "/v0/management/auth-files" {
					reads.Add(1)
					w.Header().Set("Content-Type", "application/json")
					if scenario == "remote-read-failed" {
						w.WriteHeader(http.StatusUnauthorized)
						_, _ = w.Write([]byte(`{"error":"private-remote-status-canary"}`))
						return
					}
					entry := map[string]any{"name": stableFile.Filename, "type": "codex", "status": "error", "status_message": "invalid_grant", "email": initial.Email, "account_id": "workspace-cpa", "plan_type": "plus"}
					switch scenario {
					case "remote-disabled":
						entry["disabled"] = true
					case "remote-recovered":
						entry["status"] = "active"
					case "remote-plan":
						entry["plan_type"] = "free"
					case "remote-identity":
						entry["account_id"] = "different-account"
					case "read-policy-change":
						manager.SetCPAAutoRepairPolicy(jobs.AutoRepairPolicy{})
					case "read-quality-change":
						qualityRequired.Store(true)
					}
					files := []any{entry}
					if scenario == "remote-missing" {
						files = []any{}
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"files": files})
					return
				}
				writes.Add(1)
				if r.Method != http.MethodPost || r.URL.Path != "/v0/management/auth-files" {
					t.Error("automatic CPA repair used a non-CPA endpoint")
				}
				w.Header().Set("Content-Type", "application/json")
				if scenario == "unknown" {
					_, _ = w.Write([]byte(`{}`))
					return
				}
				_, _ = w.Write([]byte(`{"status":"ok"}`))
			}))
			defer remote.Close()
			policy := jobs.AutoRepairPolicy{Enabled: true, Backend: "cpa-backend", ExcludedPlanTypes: []string{"free"}}
			manager.SetCPAAutoRepairPolicy(policy)
			manager.SetAuthorizationSyncCheck(func(context.Context, string, map[string]any) error {
				if qualityRequired.Load() {
					return &sub2api.AuthorizationGateError{Reason: "settings"}
				}
				if scenario == "gate-policy-change" {
					manager.SetCPAAutoRepairPolicy(jobs.AutoRepairPolicy{})
				}
				return nil
			})
			operation := func(_ context.Context, active *jobs.Job, _ func(jobs.JobEvent)) error {
				if scenario == "permanent-auth" {
					manager.UpdateAutoRepair(active.ID, nil, active.AutoRepairBackend, "account_disabled", true)
					return errors.New("account_disabled")
				}
				accountID, plan := "workspace-cpa", "plus"
				if scenario == "identity" {
					accountID = "different-workspace"
				}
				if scenario == "new-plan" {
					plan = "free"
				}
				doc, _ := json.Marshal(map[string]any{"type": "sub2api-data", "accounts": []any{map[string]any{
					"platform": "openai", "type": "oauth", "credentials": map[string]any{
						"email": active.Email, "client_id": protocol.DefaultCodexClientID,
						"access_token": "new-access", "refresh_token": "new-refresh", "chatgpt_account_id": accountID, "plan_type": plan,
					},
				}}})
				writePrivateGrantFixture(t, filepath.Join(root, active.ID, "sub2api-import-oauth.json"), string(doc))
				manager.UpdateResult(active.ID, true, "new authorization saved")
				if scenario == "disabled" {
					manager.SetCPAAutoRepairPolicy(jobs.AutoRepairPolicy{})
				}
				return nil
			}
			upload := func(ctx context.Context, active *jobs.Job, _ func(jobs.JobEvent)) error {
				return UploadSavedAuthorizationAtOrigin(ctx, root, "https://chatgpt.com", manager, active, nil)
			}
			manager.SetRunner(ReloginUploadRunner(manager, operation, upload))
			_, err = manager.QueueCPAAutoRepair(initial.ID, "cpa-backend", map[string]any{
				"cpaRepairAccountId": "workspace-cpa",
				"cpaConfig":          map[string]any{"baseUrl": remote.URL, "adminApiKey": "synthetic-cpa-key"},
			}, "plus")
			if err != nil {
				t.Fatal(err)
			}
			manager.Start()
			var current *jobs.Job
			deadline := time.Now().Add(3 * time.Second)
			for time.Now().Before(deadline) {
				current, _ = manager.Get(initial.ID)
				if current.OperationStatus == "succeeded" || current.OperationStatus == "failed" {
					break
				}
				time.Sleep(time.Millisecond)
			}
			if scenario == "permanent-auth" {
				if !current.AutoRepairBlocked || writes.Load() != 0 || current.OperationStatus != "failed" {
					t.Fatal("permanent failure was not blocked")
				}
				return
			}
			if !current.ResultSaved || !current.ReloginAuthorizationSaved || current.OperationStatus != "succeeded" {
				t.Fatalf("optional CPA sync lost successful authorization: status=%s upload=%s error=%s", current.OperationStatus, current.ReloginUploadStatus, current.OperationError)
			}
			wantWrites, wantUpload := int32(0), "skipped"
			if scenario == "success" {
				wantWrites, wantUpload = 1, "succeeded"
			}
			if scenario == "unknown" {
				wantWrites, wantUpload = 1, "unknown"
			}
			if scenario == "read-quality-change" {
				wantUpload = "failed"
			}
			if writes.Load() != wantWrites || current.ReloginUploadStatus != wantUpload || current.AutoRepairSyncPaused != (scenario != "success") || (current.AutoRepairLastSuccessAt != nil) != (scenario == "success") {
				t.Fatalf("incorrect CPA repair outcome: writes=%d upload=%s paused=%t", writes.Load(), current.ReloginUploadStatus, current.AutoRepairSyncPaused)
			}
			wantReads := int32(0)
			if scenario == "success" || scenario == "unknown" || strings.HasPrefix(scenario, "remote-") || strings.HasPrefix(scenario, "read-") || scenario == "gate-policy-change" {
				wantReads = 1
			}
			if reads.Load() != wantReads || strings.Contains(current.ReloginUploadError, "private-remote-status-canary") {
				t.Fatal("automatic upload did not recheck remote state or exposed remote diagnostics")
			}
			input, _ := manager.InputCopy(initial.ID)
			if input["cpaConfig"] != nil || input["cpaRepairAccountId"] != nil {
				t.Fatal("completed repair retained private upload inputs")
			}
		})
	}
}

func TestCPAAutomaticCancellationAfterAuthorizationPausesBeforeUpload(t *testing.T) {
	manager, _, initial, _ := workflowPanicFixture(t)
	manager.SetCPAAutoRepairPolicy(jobs.AutoRepairPolicy{Enabled: true, Backend: "cpa-backend"})
	job, err := manager.QueueCPAAutoRepair(initial.ID, "cpa-backend", map[string]any{
		"cpaRepairAccountId": "workspace-cpa",
		"cpaConfig":          map[string]any{"baseUrl": "https://cpa.example.test", "adminApiKey": "synthetic-cpa-key"},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	operation := func(context.Context, *jobs.Job, func(jobs.JobEvent)) error {
		manager.UpdateResult(job.ID, true, "new grant saved")
		cancel()
		return nil
	}
	uploads := 0
	upload := func(context.Context, *jobs.Job, func(jobs.JobEvent)) error { uploads++; return nil }
	err = ReloginUploadRunner(manager, operation, upload)(ctx, job, func(jobs.JobEvent) {})
	current, _ := manager.Get(job.ID)
	if !errors.Is(err, context.Canceled) || uploads != 0 || !current.ResultSaved || !current.ReloginAuthorizationSaved || !current.AutoRepairSyncPaused || current.ReloginUploadStatus != "skipped" {
		t.Fatal("cancellation between saving authorization and starting upload lost the pause fence")
	}
}
