package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/store"
)

func TestReloginAutoUploadSeparatesAuthorizationAndSyncOutcomes(t *testing.T) {
	for _, scenario := range []string{"success", "upload-rejected", "upload-unknown", "authorization-failed", "missing-fresh-grant", "canceled-after-authorization", "bad-config"} {
		t.Run(scenario, func(t *testing.T) {
			st, err := store.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			m := jobs.NewManager(t.Context(), 1, st, nil)
			m.SetCredentialStore(credentials.NewMemoryStore())
			t.Cleanup(func() { _ = m.Shutdown(context.Background()) })
			job, err := m.Submit(jobs.Spec{ID: "sync", Email: "sync@example.test", ImportOnly: true, Input: map[string]any{"password": "safe-fixture-password"}})
			if err != nil {
				t.Fatal(err)
			}
			m.UpdateResult(job.ID, true, "old grant")
			cfg := map[string]any{"baseUrl": "https://sub2api.example.test", "adminApiKey": "auto-upload-private-canary"}
			if scenario == "bad-config" {
				cfg["baseUrl"] = "invalid"
			}
			job, err = m.QueueOperation(job.ID, "relogin", map[string]any{"uploadAfterRelogin": true, "sub2apiConfig": cfg})
			if err != nil {
				t.Fatal(err)
			}
			calls := []string{}
			operation := func(ctx context.Context, active *jobs.Job, _ func(jobs.JobEvent)) error {
				calls = append(calls, "authorization")
				if scenario == "authorization-failed" {
					return errors.New("authorization failure auto-upload-private-canary")
				}
				if scenario != "missing-fresh-grant" {
					m.UpdateResult(active.ID, true, "fresh authorization saved")
				}
				if scenario == "canceled-after-authorization" {
					m.Cancel(active.ID)
				}
				return ctx.Err()
			}
			upload := func(_ context.Context, active *jobs.Job, _ func(jobs.JobEvent)) error {
				calls = append(calls, "upload")
				current, _ := m.Get(active.ID)
				if !current.ReloginAuthorizationSaved || !current.ResultSaved || current.ReloginUploadStatus != "running" {
					t.Error("sync started before fresh authorization acknowledgment")
				}
				switch scenario {
				case "upload-rejected":
					return &rejectedUpload{message: "Sub2API rejected auto-upload-private-canary"}
				case "upload-unknown":
					return errors.New("remote response lost auto-upload-private-canary")
				}
				return nil
			}
			m.SetRunner(makeReloginUploadRunner(m, operation, upload))
			m.Start()
			var state *jobs.Job
			deadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) {
				state, _ = m.Get(job.ID)
				if state.OperationStatus == "succeeded" || state.OperationStatus == "failed" || state.OperationStatus == "canceled" {
					break
				}
				time.Sleep(5 * time.Millisecond)
			}
			wantStatus, wantUpload, wantCalls := "succeeded", "succeeded", []string{"authorization", "upload"}
			switch scenario {
			case "upload-rejected":
				wantUpload = "failed"
			case "upload-unknown":
				wantUpload = "unknown"
			case "authorization-failed", "missing-fresh-grant":
				wantStatus, wantUpload, wantCalls = "failed", "skipped", []string{"authorization"}
			case "canceled-after-authorization":
				wantStatus, wantUpload, wantCalls = "canceled", "skipped", []string{"authorization"}
			case "bad-config":
				wantStatus, wantUpload, wantCalls = "failed", "skipped", []string{}
			}
			if state.OperationStatus != wantStatus || state.ReloginUploadStatus != wantUpload || !reflect.DeepEqual(calls, wantCalls) {
				t.Fatalf("outcome operation=%s sync=%s calls=%v, want %s %s %v", state.OperationStatus, state.ReloginUploadStatus, calls, wantStatus, wantUpload, wantCalls)
			}
			if !state.ResultSaved {
				t.Fatal("authorization discarded after independent sync failure")
			}
			if wantStatus == "succeeded" && (state.Status != jobs.StatusSucceeded || state.OperationError != "" || !state.ReloginAuthorizationSaved) {
				t.Fatal("sync outcome hid successful authorization")
			}
			values, _ := m.InputCopy(job.ID)
			meta, err := os.ReadFile(filepath.Join(st.Root, "job-meta.json"))
			if err != nil || values["sub2apiConfig"] != nil || strings.Contains(string(meta), "auto-upload-private-canary") || strings.Contains(state.ReloginUploadError+state.OperationError+state.Error, "auto-upload-private-canary") {
				t.Fatal("private key retained or exposed after operation", err)
			}
			count := len(calls)
			if err := makeReloginUploadRunner(m, operation, upload)(t.Context(), state, func(jobs.JobEvent) {}); err == nil || len(calls) != count {
				t.Fatal("terminal workflow replayed authorization or sync")
			}
		})
	}
}

func TestReloginAutoUploadReadsNewGrantAndUpdatesExistingSub2APIAccount(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m := jobs.NewManager(t.Context(), 1, st, nil)
	m.SetCredentialStore(credentials.NewMemoryStore())
	job, err := m.Submit(jobs.Spec{ID: "upsert", Email: "upsert@example.test", ImportOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	calls := []string{}
	remote := map[string]any{"id": 9, "platform": "openai", "type": "oauth", "name": "custom name", "priority": 17,
		"extra":       map[string]any{"openai_request_timezone": "UTC", "codex_fingerprint_mode": "off", "custom": "keep-extra-config"},
		"credentials": map[string]any{"email": job.Email, "access_token": "old-token", "refresh_token": "old-refresh", "custom": "keep-existing-config"}}
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
			if len(body) != 2 || creds["access_token"] != "new-access" || creds["refresh_token"] != "new-refresh" || creds["custom"] != "keep-existing-config" {
				t.Error("sync submitted stale grant or replaced existing Sub2API configuration")
			}
			if !reflect.DeepEqual(body["extra"], map[string]any{"openai_request_timezone": "Asia/Shanghai", "codex_fingerprint_mode": "off", "custom": "keep-extra-config"}) {
				t.Error("sync must apply the configured timezone and preserve all other extra settings")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": remote})
		default:
			t.Error("existing account was created or changed via unexpected operation")
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer server.Close()
	job, err = m.QueueOperation(job.ID, "relogin", map[string]any{"uploadAfterRelogin": true, "sub2apiConfig": map[string]any{"baseUrl": server.URL, "adminApiKey": "fixture-key", "priority": "2", "openaiRequestTimezone": "Asia/Shanghai"}})
	if err != nil {
		t.Fatal(err)
	}
	operation := func(ctx context.Context, active *jobs.Job, _ func(jobs.JobEvent)) error {
		dir, err := store.New(filepath.Join(st.Root, active.ID))
		if err != nil {
			return err
		}
		err = dir.WriteJSON(ctx, "sub2api-import-oauth.json", map[string]any{"type": "sub2api-data", "accounts": []any{map[string]any{"platform": "openai", "type": "oauth", "credentials": map[string]any{"email": active.Email, "access_token": "new-access", "refresh_token": "new-refresh"}}}})
		if err == nil {
			m.UpdateResult(active.ID, true, "new grant saved")
		}
		return err
	}
	upload := func(ctx context.Context, active *jobs.Job, _ func(jobs.JobEvent)) error {
		return uploadTOTPWorkflow(ctx, st.Root, m, active)
	}
	if err := makeReloginUploadRunner(m, operation, upload)(t.Context(), job, func(jobs.JobEvent) {}); err != nil {
		t.Fatal(err)
	}
	state, _ := m.Get(job.ID)
	if state.ReloginUploadStatus != "succeeded" || !reflect.DeepEqual(calls, []string{"GET /api/v1/admin/accounts", "GET /api/v1/admin/accounts/9", "PUT /api/v1/admin/accounts/9"}) {
		t.Fatal("automatic sync did not finish by updating the existing account", calls)
	}
}

// rejectedUpload is a synthetic confirmed remote refusal, never a live request.
type rejectedUpload struct{ message string }

func (e *rejectedUpload) Error() string                { return e.message }
func (e *rejectedUpload) UploadOutcomeUncertain() bool { return false }
