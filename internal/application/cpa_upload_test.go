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

	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/protocol"
	"tosub2-go-port/internal/store"
	"tosub2-go-port/internal/sub2api"
)

func TestCPAWorkflowUploadUsesSavedGrantAndPreservesGateAndOutcome(t *testing.T) {
	for _, scenario := range []string{"success", "gate", "identity", "bps", "rejected", "unknown"} {
		t.Run(scenario, func(t *testing.T) {
			var requests, gateCalls atomic.Int32
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.Method != http.MethodPost || r.URL.Path != "/v0/management/auth-files" || r.Header.Get("Authorization") != "Bearer cpa-key-canary" || !strings.HasSuffix(r.URL.Query().Get("name"), ".json") {
					t.Error("CPA upload used the wrong management request")
				}
				var account map[string]any
				if json.NewDecoder(r.Body).Decode(&account) != nil || account["type"] != "codex" || account["email"] != "workflow-cpa@example.test" || account["access_token"] != "access-canary" || account["refresh_token"] != "refresh-canary" || account["account_id"] != "workspace-cpa" {
					t.Error("CPA upload did not use the saved authorization")
				}
				if account["credentials"] != nil || account["extra"] != nil {
					t.Error("CPA upload leaked unrelated vendor configuration")
				}
				w.Header().Set("Content-Type", "application/json")
				switch scenario {
				case "rejected":
					w.WriteHeader(http.StatusUnauthorized)
					_, _ = w.Write([]byte(`{"error":"cpa-key-canary"}`))
				case "unknown":
					_, _ = w.Write([]byte(`{}`))
				default:
					_, _ = w.Write([]byte(`{"status":"ok"}`))
				}
			}))
			defer remote.Close()
			disk, err := store.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			manager := jobs.NewManager(t.Context(), 1, disk, nil)
			manager.SetCredentialStore(credentials.NewMemoryStore())
			t.Cleanup(func() { _ = manager.Shutdown(context.Background()) })
			job, err := manager.Submit(jobs.Spec{ID: "cpa-upload", Email: "workflow-cpa@example.test", ImportOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			job, err = manager.QueueOperation(job.ID, "relogin", map[string]any{
				"uploadAfterRelogin": true, "uploadTarget": "cpa",
				"cpaConfig": map[string]any{"baseUrl": remote.URL, "adminApiKey": "cpa-key-canary"},
			})
			if err != nil {
				t.Fatal(err)
			}
			client, email := protocol.DefaultCodexClientID, job.Email
			if scenario == "bps" {
				client = protocol.DefaultBPSClientID
			}
			if scenario == "identity" {
				email = "other@example.test"
			}
			document, _ := json.Marshal(map[string]any{"type": "sub2api-data", "accounts": []any{map[string]any{
				"platform": "openai", "type": "oauth", "credentials": map[string]any{
					"email": email, "client_id": client, "access_token": "access-canary", "refresh_token": "refresh-canary", "chatgpt_account_id": "workspace-cpa",
				}, "extra": map[string]any{"vendor_private_config": "must-not-copy"},
			}}})
			writePrivateGrantFixture(t, filepath.Join(disk.Root, job.ID, "sub2api-import-oauth.json"), string(document))
			manager.SetAuthorizationSyncCheck(func(_ context.Context, id string, account map[string]any) error {
				gateCalls.Add(1)
				if id != job.ID || account["credentials"].(map[string]any)["access_token"] != "access-canary" {
					t.Error("quality gate received a different authorization")
				}
				if scenario == "gate" {
					return &sub2api.AuthorizationGateError{Reason: "degraded"}
				}
				return nil
			})
			err = UploadSavedAuthorizationAtOrigin(t.Context(), disk.Root, "https://chatgpt.com", manager, job, nil)
			wantRequests, wantGate := int32(1), int32(1)
			if scenario == "gate" || scenario == "identity" || scenario == "bps" {
				wantRequests = 0
			}
			if scenario == "identity" || scenario == "bps" {
				wantGate = 0
			}
			if requests.Load() != wantRequests || gateCalls.Load() != wantGate {
				t.Fatalf("unexpected upload or gate calls: requests=%d gate=%d", requests.Load(), gateCalls.Load())
			}
			if scenario == "success" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			var outcome interface{ UploadOutcomeUncertain() bool }
			if err == nil || !errors.As(err, &outcome) || outcome.UploadOutcomeUncertain() != (scenario == "unknown") || strings.Contains(err.Error(), "canary") {
				t.Fatalf("incorrect or private workflow outcome: %v", err)
			}
		})
	}
}

func TestCPAWorkflowRejectsInvalidConfigurationBeforeAuthentication(t *testing.T) {
	for _, kind := range []string{"relogin", "totp"} {
		t.Run(kind, func(t *testing.T) {
			disk, err := store.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			manager := jobs.NewManager(t.Context(), 1, disk, nil)
			vault := credentials.NewMemoryStore()
			manager.SetCredentialStore(vault)
			t.Cleanup(func() { _ = manager.Shutdown(context.Background()) })
			job, err := manager.Submit(jobs.Spec{ID: "invalid-cpa", Email: "invalid-cpa@example.test", ImportOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			op := "relogin"
			if kind == "totp" {
				op = "setup-2fa"
			}
			job, err = manager.QueueOperation(job.ID, op, map[string]any{
				"uploadTarget": "cpa", "uploadAfterRelogin": true, "uploadAfterTotp": true, "reloginAfterTotp": true,
				"cpaConfig": map[string]any{"baseUrl": "https://cpa.example.test", "adminApiKey": ""},
			})
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			operation := func(context.Context, *jobs.Job, func(jobs.JobEvent)) error { calls++; return nil }
			runner := ReloginUploadRunner(manager, operation, operation)
			if kind == "totp" {
				runner = TOTPWorkflowRunner(manager, vault, operation, operation)
			}
			if err := runner(t.Context(), job, func(jobs.JobEvent) {}); err == nil || calls != 0 {
				t.Fatal("invalid CPA configuration started an account operation", err)
			}
			input, _ := manager.InputCopy(job.ID)
			if input["cpaConfig"] != nil {
				t.Fatal("rejected workflow retained CPA configuration")
			}
		})
	}
}

func TestCPAOptionalUploadCompletesReloginAndTOTPWorkflows(t *testing.T) {
	for _, operationName := range []string{"relogin", "setup-2fa"} {
		t.Run(operationName, func(t *testing.T) {
			var writes atomic.Int32
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				writes.Add(1)
				if r.Method != http.MethodPost || r.URL.Path != "/v0/management/auth-files" {
					t.Error("workflow used a non-CPA upload endpoint")
				}
				var doc map[string]any
				if json.NewDecoder(r.Body).Decode(&doc) != nil || doc["priority"] != float64(7) || doc["weight"] != float64(0) {
					t.Error("workflow lost the configured CPA scheduling options")
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"status":"ok"}`))
			}))
			defer remote.Close()
			manager, vault, initial, root := workflowPanicFixture(t)
			job, err := manager.QueueOperation(initial.ID, operationName, map[string]any{
				"uploadTarget": "cpa", "uploadAfterRelogin": true, "uploadAfterTotp": true, "reloginAfterTotp": true,
				"cpaConfig": map[string]any{"baseUrl": remote.URL, "adminApiKey": "workflow-cpa-private-canary", "priority": 7, "weight": 0},
			})
			if err != nil {
				t.Fatal(err)
			}
			authorizations := 0
			operation := func(_ context.Context, active *jobs.Job, _ func(jobs.JobEvent)) error {
				if active.Operation == "setup-2fa" {
					saved := credentials.Credentials{TOTPSecret: "JBSWY3DPEHPK3PXP"}
					if err := vault.Save(active.Email, saved); err != nil {
						return err
					}
					manager.UpdateStoredCredentials(active.ID, saved)
					return nil
				}
				authorizations++
				document, _ := json.Marshal(map[string]any{"type": "sub2api-data", "accounts": []any{map[string]any{
					"platform": "openai", "type": "oauth", "credentials": map[string]any{
						"email": active.Email, "client_id": protocol.DefaultCodexClientID, "access_token": "new-access", "refresh_token": "new-refresh", "chatgpt_account_id": "workspace-cpa",
					},
				}}})
				writePrivateGrantFixture(t, filepath.Join(root, active.ID, "sub2api-import-oauth.json"), string(document))
				manager.UpdateResult(active.ID, true, "new authorization saved")
				return nil
			}
			upload := func(ctx context.Context, active *jobs.Job, _ func(jobs.JobEvent)) error {
				return UploadSavedAuthorizationAtOrigin(ctx, root, "https://chatgpt.com", manager, active, nil)
			}
			runner := ReloginUploadRunner(manager, operation, upload)
			if operationName == "setup-2fa" {
				runner = TOTPWorkflowRunner(manager, vault, operation, upload)
			}
			if err := runner(t.Context(), job, func(jobs.JobEvent) {}); err != nil {
				t.Fatal(err)
			}
			current, _ := manager.Get(job.ID)
			if writes.Load() != 1 || authorizations != 1 || !current.ResultSaved || current.UploadTarget != jobs.UploadTargetCPA {
				t.Fatal("workflow lost the new grant or repeated its upload")
			}
			if operationName == "relogin" && current.ReloginUploadStatus != "succeeded" || operationName == "setup-2fa" && (current.TOTPUploadStatus != "succeeded" || current.TOTPWorkflowStatus != "succeeded") {
				t.Fatal("CPA completion was not recorded")
			}
			input, _ := manager.InputCopy(job.ID)
			if input["cpaConfig"] != nil {
				t.Fatal("completed CPA workflow retained its management key")
			}
		})
	}
}
