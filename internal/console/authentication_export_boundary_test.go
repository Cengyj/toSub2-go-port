package console

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"tosub2-go-port/internal/authmaterial"
	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/sub2api"
)

func TestAuthorizationExportRejectsEmbeddedAuthenticationSessions(t *testing.T) {
	for _, location := range []string{"root", "account", "credentials", "extra", "nested-array"} {
		t.Run(location, func(t *testing.T) {
			s, m := newMonitorTestServer(t)
			job, err := m.Submit(jobs.Spec{ID: "contaminated-export", Email: "owner@example.test", ImportOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			m.UpdateResult(job.ID, true, "")
			m.Emit(jobs.JobEvent{JobID: job.ID, Status: jobs.StatusSucceeded})
			credentials := map[string]any{"email": job.Email, "access_token": "legitimate-token"}
			account := map[string]any{"credentials": credentials, "extra": map[string]any{"business_field": "preserved"}}
			doc := map[string]any{"type": "sub2api-data", "version": 1, "accounts": []any{account}}
			switch location {
			case "root":
				doc["cookies"] = []any{map[string]any{"name": "auth", "value": "synthetic-session-canary"}}
			case "account":
				account["cookie_ref"] = "synthetic-session-canary"
			case "credentials":
				credentials["Cookie"] = "synthetic-session-canary"
			case "extra":
				account["extra"].(map[string]any)["callbackUrl"] = "synthetic-session-canary"
			case "nested-array":
				account["extra"].(map[string]any)["vendor"] = []any{map[string]any{"code_verifier": "synthetic-session-canary"}}
			}
			data, _ := json.Marshal(doc)
			path := filepath.Join(s.cfg.OutputRoot, job.ID, "sub2api-import-oauth.json")
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			var calls atomic.Int32
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(500) }))
			defer remote.Close()
			for _, req := range []struct {
				method, path string
				body         any
				status       int
			}{
				{http.MethodGet, "/api/jobs/" + job.ID + "/download", nil, http.StatusConflict},
				{http.MethodPost, "/api/jobs/download-batch", map[string]any{"ids": []string{job.ID}}, http.StatusConflict},
				{http.MethodPost, "/api/sub2api/upload", map[string]any{"ids": []string{job.ID}, "config": map[string]any{"baseUrl": remote.URL, "adminApiKey": "fixture-admin-key"}}, http.StatusConflict},
				{http.MethodPost, "/api/sub2api/upload", map[string]any{"payload": doc, "baseUrl": remote.URL, "adminApiKey": "fixture-admin-key"}, http.StatusBadRequest},
			} {
				response := frontendRequest(s, req.method, req.path, req.body, "test")
				if response.Code != req.status || strings.Contains(response.Body.String(), "synthetic-session-canary") || strings.Contains(response.Body.String(), "legitimate-token") || response.Header().Get("Content-Disposition") != "" {
					t.Fatalf("session escaped export boundary: route=%s status=%d", req.path, response.Code)
				}
			}
			m.UpdateAutoRepair(job.ID, []int{61}, sub2api.BackendIdentity(remote.URL), "", false)
			current, _ := m.Get(job.ID)
			if _, err := s.syncRepairOutput(t.Context(), sub2api.Config{BaseURL: remote.URL, APIKey: "fixture-admin-key"}, current); err == nil || !strings.Contains(err.Error(), "已移除的登录会话材料") {
				t.Fatalf("automatic upload did not enforce the export boundary: %v", err)
			}
			after, err := os.ReadFile(path)
			if err != nil || string(after) != string(data) || calls.Load() != 0 {
				t.Fatal("export refusal modified credentials or reached remote service")
			}
		})
	}
}

func TestAuthorizationExportKeepsTokensFactorEvidenceAndVendorFields(t *testing.T) {
	data := []byte(`{"accounts":[{"credentials":{"access_token":"access","refresh_token":"refresh","id_token":"identity"},"extra":{"codex_fingerprint_mode":"session","factor":{"session_id":"factor-binding"},"business_state":"active","provider_options":{"opaque":9007199254740993}}}]}`)
	if authmaterial.ContainsLoginSessionJSON(data) {
		t.Fatal("normal token authorization or business metadata was classified as a login session")
	}
}

func TestAuthorizationDownloadRejectsDuplicateFieldSessionBeforeOverwrite(t *testing.T) {
	s, m := newMonitorTestServer(t)
	job, err := m.Submit(jobs.Spec{ID: "duplicate-export", Email: "duplicate@example.test", ImportOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	m.UpdateResult(job.ID, true, "")
	m.Emit(jobs.JobEvent{JobID: job.ID, Status: jobs.StatusSucceeded})
	// A map decoder retains only the second extra object, while single-file
	// downloads preserve the original bytes. Inspect the whole source first.
	data := []byte(`{"type":"sub2api-data","version":1,"accounts":[{"credentials":{"email":"duplicate@example.test","access_token":"legitimate-token"},"extra":{"cookies":[{"value":"synthetic-duplicate-session-canary"}]},"extra":{}}]}`)
	path := filepath.Join(s.cfg.OutputRoot, job.ID, "sub2api-import-oauth.json")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	for _, request := range []struct {
		method, path string
		body         any
	}{
		{http.MethodGet, "/api/jobs/" + job.ID + "/download", nil},
		{http.MethodPost, "/api/jobs/download-batch", map[string]any{"ids": []string{job.ID}}},
	} {
		response := frontendRequest(s, request.method, request.path, request.body, "test")
		if response.Code != http.StatusConflict || strings.Contains(response.Body.String(), "synthetic-duplicate-session-canary") || strings.Contains(response.Body.String(), "legitimate-token") || response.Header().Get("Content-Disposition") != "" {
			t.Fatalf("duplicate field escaped download guard: route=%s status=%d", request.path, response.Code)
		}
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(data) {
		t.Fatal("refusing an ambiguous export rewrote its original credential file")
	}
}

func TestUnknownInputAndProviderEventCannotPersistAuthenticationMaterial(t *testing.T) {
	s, m := newMonitorTestServer(t)
	canary := "synthetic-unknown-cookie-material"
	response := frontendRequest(s, http.MethodPost, "/api/jobs", map[string]any{"id": "unknown-input", "email": "unknown@example.test", "input": map[string]any{"vendor_extension": map[string]any{"cookies": []any{map[string]any{"value": canary}}, "callback": "synthetic-unfinished-code"}}}, "test")
	if response.Code != http.StatusOK && response.Code != http.StatusCreated && response.Code != http.StatusAccepted {
		t.Fatalf("unknown business input unexpectedly rejected: %d", response.Code)
	}
	m.Emit(jobs.JobEvent{JobID: "unknown-input", Stage: "network_retry", Status: jobs.StatusRunning, Prompt: `response={"cookies":[{"value":"` + canary + `"}]}`, Data: map[string]any{"headers": map[string]any{"Set-Cookie": canary}, "callback": "synthetic-unfinished-code"}})
	for _, route := range []string{"/api/jobs/unknown-input", "/api/jobs"} {
		response = frontendRequest(s, http.MethodGet, route, nil, "test")
		if response.Code != http.StatusOK || strings.Contains(response.Body.String(), canary) || strings.Contains(response.Body.String(), "synthetic-unfinished-code") {
			t.Fatal("unknown input or provider event escaped public projection")
		}
	}
	data, err := os.ReadFile(filepath.Join(s.cfg.OutputRoot, "job-meta.json"))
	if err != nil || strings.Contains(string(data), canary) || strings.Contains(string(data), "synthetic-unfinished-code") {
		t.Fatal("unknown input or raw authentication response entered metadata/log persistence")
	}
}
