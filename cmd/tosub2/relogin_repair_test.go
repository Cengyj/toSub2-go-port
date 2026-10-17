package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/store"
)

// Exercise the actual uploader through the manual reauthorization workflow.
// The remote service and grant are synthetic; no authentication is performed.
func TestReloginUploadRepairsRemoteErrorWithoutDiscardingSavedGrant(t *testing.T) {
	for _, scenario := range []string{"success", "clear-rejected", "clear-unknown", "schedule-rejected", "schedule-unknown"} {
		t.Run(scenario, func(t *testing.T) {
			st, err := store.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			m := jobs.NewManager(t.Context(), 1, st, nil)
			m.SetCredentialStore(credentials.NewMemoryStore())
			t.Cleanup(func() { _ = m.Shutdown(context.Background()) })
			job, err := m.Submit(jobs.Spec{ID: "repair", Email: "repair@example.test", ImportOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			calls := []string{}
			remote := map[string]any{"id": 9, "platform": "openai", "type": "oauth", "status": "error", "schedulable": false,
				"error_message": "upstream returned 401",
				"extra":         map[string]any{"custom": "preserved"},
				"credentials":   map[string]any{"email": job.Email, "access_token": "old-fixture-grant"}}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls = append(calls, r.Method+" "+r.URL.Path)
				w.Header().Set("Content-Type", "application/json")
				switch r.Method + " " + r.URL.Path {
				case "GET /api/v1/admin/accounts":
					_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"items": []any{remote}, "pages": 1, "total": 1}})
				case "GET /api/v1/admin/accounts/9":
					_ = json.NewEncoder(w).Encode(map[string]any{"data": remote})
				case "PUT /api/v1/admin/accounts/9":
					var body map[string]any
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
					}
					creds, _ := body["credentials"].(map[string]any)
					if len(body) != 2 || creds["access_token"] != "new-fixture-grant" {
						t.Error("did not update fresh authorization with configuration-preserving payload")
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"data": remote})
				case "POST /api/v1/admin/accounts/9/clear-error", "POST /api/v1/admin/accounts/9/schedulable":
					failing := (strings.HasPrefix(scenario, "clear-") && strings.HasSuffix(r.URL.Path, "clear-error")) ||
						(strings.HasPrefix(scenario, "schedule-") && strings.HasSuffix(r.URL.Path, "schedulable"))
					if failing {
						if strings.HasSuffix(scenario, "unknown") {
							w.WriteHeader(http.StatusBadGateway)
						} else {
							w.WriteHeader(http.StatusForbidden)
						}
						_, _ = w.Write([]byte(`{"message":"remote-private-canary"}`))
						return
					}
					_, _ = w.Write([]byte(`{"success":true}`))
				default:
					t.Error("unexpected mutation or creation", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusBadRequest)
				}
			}))
			defer server.Close()
			job, err = m.QueueOperation(job.ID, "relogin", map[string]any{"uploadAfterRelogin": true, "sub2apiConfig": map[string]any{"baseUrl": server.URL, "adminApiKey": "fixture-key"}})
			if err != nil {
				t.Fatal(err)
			}
			authorizations := 0
			operation := func(ctx context.Context, active *jobs.Job, _ func(jobs.JobEvent)) error {
				authorizations++
				dir, err := store.New(filepath.Join(st.Root, active.ID))
				if err != nil {
					return err
				}
				err = dir.WriteJSON(ctx, "sub2api-import-oauth.json", map[string]any{"type": "sub2api-data", "accounts": []any{map[string]any{"platform": "openai", "type": "oauth", "credentials": map[string]any{"email": active.Email, "access_token": "new-fixture-grant"}}}})
				if err == nil {
					m.UpdateResult(active.ID, true, "fresh authorization saved")
				}
				return err
			}
			upload := func(ctx context.Context, active *jobs.Job, _ func(jobs.JobEvent)) error {
				return uploadTOTPWorkflow(ctx, st.Root, m, active)
			}
			runner := makeReloginUploadRunner(m, operation, upload)
			if err := runner(t.Context(), job, func(jobs.JobEvent) {}); err != nil {
				t.Fatal(err)
			}
			state, _ := m.Get(job.ID)
			wantStatus := "succeeded"
			if strings.HasSuffix(scenario, "rejected") {
				wantStatus = "failed"
			} else if strings.HasSuffix(scenario, "unknown") {
				wantStatus = "unknown"
			}
			wantCalls := []string{"GET /api/v1/admin/accounts", "GET /api/v1/admin/accounts/9", "PUT /api/v1/admin/accounts/9", "POST /api/v1/admin/accounts/9/clear-error"}
			if !strings.HasPrefix(scenario, "clear-") {
				wantCalls = append(wantCalls, "POST /api/v1/admin/accounts/9/schedulable")
			}
			if state.ReloginUploadStatus != wantStatus || !state.ResultSaved || !state.ReloginAuthorizationSaved || !reflect.DeepEqual(calls, wantCalls) {
				t.Fatalf("sync=%s want=%s grantSaved=%t freshGrant=%t calls=%v", state.ReloginUploadStatus, wantStatus, state.ResultSaved, state.ReloginAuthorizationSaved, calls)
			}
			if strings.Contains(state.ReloginUploadError, "private-canary") {
				t.Fatal("remote response exposed")
			}
			grant, err := os.ReadFile(filepath.Join(st.Root, job.ID, "sub2api-import-oauth.json"))
			if err != nil || !strings.Contains(string(grant), "new-fixture-grant") {
				t.Fatal("committed authorization lost", err)
			}
			if err := runner(t.Context(), state, func(jobs.JobEvent) {}); err == nil || authorizations != 1 || !reflect.DeepEqual(calls, wantCalls) {
				t.Fatal("terminal workflow replayed authentication or upload")
			}
		})
	}
}
