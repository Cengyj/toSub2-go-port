package console

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"tosub2-go-port/internal/accountquality"
	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/protocol"
)

func saveCPAFixture(t *testing.T, s *Server, base string) {
	t.Helper()
	w := frontendRequest(s, http.MethodPost, "/api/cpa/settings", map[string]any{"baseUrl": base, "adminApiKey": "cpa-private-admin-canary", "revision": 0}, "test")
	if w.Code != http.StatusOK {
		t.Fatalf("configure CPA: %d %s", w.Code, w.Body.String())
	}
}

func cpaAccountFixture(t *testing.T, s *Server, m *jobs.Manager, id, client string) *jobs.Job {
	t.Helper()
	j, err := m.Submit(jobs.Spec{ID: id, Email: id + "@example.test", ImportOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	doc := map[string]any{"type": "sub2api-data", "version": 1, "accounts": []any{map[string]any{
		"platform": "openai", "type": "oauth", "credentials": map[string]any{
			"email": j.Email, "access_token": "cpa-private-access-canary", "refresh_token": "cpa-private-refresh-canary",
			"client_id": client, "chatgpt_account_id": "account-" + id,
		},
	}}}
	data, _ := json.Marshal(doc)
	dir := filepath.Join(s.cfg.OutputRoot, id)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub2api-import-oauth.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	m.UpdateResult(j.ID, true, "")
	m.Emit(jobs.JobEvent{JobID: j.ID, Status: jobs.StatusSucceeded})
	j, _ = m.Get(j.ID)
	return j
}

func TestCPASettingsKeepSecretPrivateAndBindItToDestination(t *testing.T) {
	s, _ := newMonitorTestServer(t)
	saveCPAFixture(t, s, "https://cpa.example.test/v0/management/")
	w := frontendRequest(s, http.MethodGet, "/api/cpa/settings", nil, "test")
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "cpa-private-admin-canary") || !strings.Contains(w.Body.String(), `"configured":true`) {
		t.Fatal("public settings exposed or lost secret", w.Code, w.Body.String())
	}
	data, err := os.ReadFile(filepath.Join(s.cfg.OutputRoot, "cpa-settings.json"))
	if err != nil || strings.Contains(string(data), "cpa-private-admin-canary") || strings.Contains(string(data), "adminApiKey") {
		t.Fatal("plaintext key persisted", err)
	}
	restored := &Server{cfg: s.cfg, monitorVault: s.monitorVault}
	restored.initializeCPA()
	cfg, err := restored.configuredCPA()
	if err != nil || cfg.APIKey != "cpa-private-admin-canary" || cfg.BaseURL != "https://cpa.example.test" {
		t.Fatal("secure settings did not restore", err)
	}
	for _, request := range []map[string]any{
		{"baseUrl": "https://cpa.example.test", "adminApiKey": "replacement", "revision": 0},
		{"baseUrl": "https://different.example.test", "adminApiKey": "", "revision": 1},
	} {
		w = frontendRequest(s, http.MethodPost, "/api/cpa/settings", request, "test")
		if w.Code == http.StatusOK {
			t.Fatal("stale write or key reuse at a new destination was accepted")
		}
	}
	w = frontendRequest(s, http.MethodPost, "/api/cpa/settings", map[string]any{"baseUrl": "https://cpa.example.test", "adminApiKey": "", "revision": 1}, "test")
	if w.Code != http.StatusOK {
		t.Fatal("same destination could not retain saved key", w.Body.String())
	}
	w = frontendRequest(s, http.MethodPost, "/api/cpa/settings", map[string]any{"baseUrl": "", "revision": 2}, "test")
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), `"hasApiKey":true`) {
		t.Fatal("clearing configuration retained its key")
	}
	if len(s.monitorVault.(*monitorMemoryVault).values) != 0 {
		t.Fatal("clearing CPA retained unused credential references")
	}
}

func TestCPASettingsMemoryOnlyAndFailedSave(t *testing.T) {
	s, _ := newMonitorTestServer(t)
	s.monitorVault = nil
	saveCPAFixture(t, s, "https://cpa.example.test")
	if got := s.publicCPASettings(); got["credentialPersistence"] != "memory_only" || got["configured"] != true {
		t.Fatal("memory-only configuration not reported", got)
	}
	restored := &Server{cfg: s.cfg}
	restored.initializeCPA()
	if _, err := restored.configuredCPA(); err == nil {
		t.Fatal("secret survived restart without a secure store")
	}
	// Force a real atomic replacement failure without touching the old file.
	s.cfg.OutputRoot = t.TempDir()
	if err := os.Mkdir(filepath.Join(s.cfg.OutputRoot, "cpa-settings.json"), 0700); err != nil {
		t.Fatal(err)
	}
	w := frontendRequest(s, http.MethodPost, "/api/cpa/settings", map[string]any{"baseUrl": "https://new.example.test", "adminApiKey": "new-key", "revision": 1}, "test")
	cfg, err := s.configuredCPA()
	if w.Code != http.StatusInternalServerError || err != nil || cfg.BaseURL != "https://cpa.example.test" || cfg.APIKey != "cpa-private-admin-canary" || !strings.Contains(w.Body.String(), `"uncertain":false`) || !strings.Contains(w.Body.String(), `"outcome":"failed"`) {
		t.Fatal("failed save published new configuration", w.Code, err)
	}
}

func TestCPAUploadRechecksChangedPolicyBeforeNextAccount(t *testing.T) {
	s, m := newMonitorTestServer(t)
	var requests atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		policy, err := s.quality.Service.Settings()
		if err != nil {
			t.Error(err)
		}
		policy.Enabled, policy.BeforeUpload = true, true
		policy.Model = "synthetic-no-inference"
		if _, err := s.quality.Service.Update(r.Context(), policy); err != nil {
			t.Error(err)
		}
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer remote.Close()
	saveCPAFixture(t, s, remote.URL)
	for _, id := range []string{"cpa-policy-first", "cpa-policy-second"} {
		cpaAccountFixture(t, s, m, id, protocol.DefaultCodexClientID)
	}
	// The saved token fixtures intentionally lack the valid JWT required by
	// the enabled probe policy; enabling it must reject before any inference.
	w := frontendRequest(s, http.MethodPost, "/api/cpa/upload", map[string]any{"ids": []string{"cpa-policy-first", "cpa-policy-second"}}, "test")
	if w.Code != http.StatusBadGateway || requests.Load() != 1 || !strings.Contains(w.Body.String(), `"outcome":"partial"`) || !strings.Contains(w.Body.String(), `"uploaded":1`) {
		t.Fatal("batch ignored a policy change between writes", w.Code, w.Body.String(), requests.Load())
	}
	settings, err := accountquality.ReadSettings(s.cfg.OutputRoot)
	if err != nil || !settings.BeforeUpload {
		t.Fatal("fixture failed to publish stricter policy", err)
	}
}

func TestCPAUploadUsesSavedConfigurationAndProtectsWholeBatch(t *testing.T) {
	s, m := newMonitorTestServer(t)
	var requests atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != "/v0/management/auth-files" || r.Header.Get("Authorization") != "Bearer cpa-private-admin-canary" || !strings.HasPrefix(r.URL.Query().Get("name"), "tosub2-codex-") {
			t.Error("wrong CPA management request")
		}
		var doc map[string]any
		if json.NewDecoder(r.Body).Decode(&doc) != nil || doc["type"] != "codex" || doc["access_token"] != "cpa-private-access-canary" || doc["account_id"] != "account-cpa-ready" || doc["credentials"] != nil {
			t.Error("CPA did not receive a converted Codex auth file")
		}
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer remote.Close()
	saveCPAFixture(t, s, remote.URL)
	j := cpaAccountFixture(t, s, m, "cpa-ready", protocol.DefaultCodexClientID)
	bps := cpaAccountFixture(t, s, m, "cpa-bps", protocol.DefaultBPSClientID)
	w := frontendRequest(s, http.MethodPost, "/api/cpa/upload", map[string]any{"ids": []string{j.ID, bps.ID}}, "test")
	if w.Code != http.StatusConflict || requests.Load() != 0 || !strings.Contains(w.Body.String(), "BPS") {
		t.Fatal("incompatible batch reached CPA", w.Code, w.Body.String(), requests.Load())
	}
	w = frontendRequest(s, http.MethodPost, "/api/cpa/upload", map[string]any{"ids": []string{j.ID, j.ID}}, "test")
	if w.Code != http.StatusOK || requests.Load() != 1 || !strings.Contains(w.Body.String(), `"success":1`) || strings.Contains(w.Body.String(), "private-") {
		t.Fatal("deduplicated upload failed or leaked secrets", w.Code, w.Body.String())
	}
	for _, route := range []string{"/api/cpa/settings", "/api/cpa/upload"} {
		w = frontendRequest(s, http.MethodPost, route, map[string]any{"ids": []string{j.ID}}, "wrong-token")
		if w.Code != http.StatusForbidden || requests.Load() != 1 {
			t.Fatal("CPA route missing console authorization", route, w.Code)
		}
	}
}

func TestCPAUploadPreservesPartialUnknownAndStopsRemaining(t *testing.T) {
	s, m := newMonitorTestServer(t)
	var requests atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			_, _ = w.Write([]byte(`{"status":"ok"}`))
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("remote-private-response-canary"))
	}))
	defer remote.Close()
	saveCPAFixture(t, s, remote.URL)
	ids := []string{"cpa-first", "cpa-second", "cpa-third"}
	for _, id := range ids {
		cpaAccountFixture(t, s, m, id, protocol.DefaultCodexClientID)
	}
	w := frontendRequest(s, http.MethodPost, "/api/cpa/upload", map[string]any{"ids": ids}, "test")
	var reply struct {
		Outcome   string                         `json:"outcome"`
		Uncertain bool                           `json:"uncertain"`
		Uploaded  int                            `json:"uploaded"`
		Skipped   int                            `json:"skipped"`
		Results   []struct{ ID, Outcome string } `json:"results"`
	}
	if json.Unmarshal(w.Body.Bytes(), &reply) != nil || w.Code != http.StatusBadGateway || requests.Load() != 2 || reply.Outcome != "partial" || !reply.Uncertain || reply.Uploaded != 1 || reply.Skipped != 1 {
		t.Fatal("partial/unknown receipt was lost or retried", w.Code, w.Body.String(), requests.Load())
	}
	if len(reply.Results) != 3 || reply.Results[0].Outcome != "succeeded" || reply.Results[1].Outcome != "unknown" || reply.Results[2].Outcome != "skipped" || strings.Contains(w.Body.String(), "canary") {
		t.Fatal("per-account results invalid or secret response exposed", w.Body.String())
	}
}

func TestCPAAutomaticUploadSnapshotsSavedTargetAndKeepsSecretsPrivate(t *testing.T) {
	for _, batch := range []bool{false, true} {
		for _, operation := range []string{"relogin", "setup-2fa"} {
			t.Run(operation+map[bool]string{true: "-batch", false: "-single"}[batch], func(t *testing.T) {
				s, m := newMonitorTestServer(t)
				m.SetCredentialStore(credentials.NewMemoryStore())
				saveCPAFixture(t, s, "https://cpa.example.test")
				j := importTOTPActionFixture(t, m, "cpa-auto", true)
				body := map[string]any{"uploadTarget": "cpa", "authorizationMethod": "chatgpt", "uploadAfterRelogin": true}
				if operation == "setup-2fa" {
					delete(body, "uploadAfterRelogin")
					body["resetTotp"], body["reloginAfterTotp"], body["uploadAfterTotp"] = true, true, true
				}
				path := "/api/jobs/" + j.ID + "/" + operation
				if batch {
					path, body["ids"] = "/api/jobs/"+operation+"-batch", []string{j.ID}
				}
				w := frontendRequest(s, http.MethodPost, path, body, "test")
				if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"uploadTarget":"cpa"`) {
					t.Fatal("CPA workflow not admitted/projected", w.Code, w.Body.String())
				}
				values, _ := m.InputCopy(j.ID)
				cfg, ok := values["cpaConfig"].(map[string]any)
				if !ok || cfg["adminApiKey"] != "cpa-private-admin-canary" || cfg["baseUrl"] != "https://cpa.example.test" || cfg["priority"] != 0 || cfg["weight"] != 1 || values["sub2apiConfig"] != nil {
					t.Fatal("queued operation lost or mixed private destination")
				}
				priority, weight := 10, 0
				if _, err := s.commitCPASettings(context.Background(), "https://changed.example.test", "replacement-key", 1, cpaSchedulingUpdate{Priority: &priority, Weight: &weight}); err != nil {
					t.Fatal(err)
				}
				values, _ = m.InputCopy(j.ID)
				frozen := values["cpaConfig"].(map[string]any)
				if frozen["baseUrl"] != "https://cpa.example.test" || frozen["priority"] != 0 || frozen["weight"] != 1 {
					t.Fatal("settings edit redirected an already admitted operation")
				}
				meta, err := os.ReadFile(filepath.Join(s.cfg.OutputRoot, "job-meta.json"))
				if err != nil || strings.Contains(string(meta), "cpa-private-admin-canary") || strings.Contains(w.Body.String(), "cpa-private-admin-canary") {
					t.Fatal("CPA secret leaked through metadata/public response", err)
				}
			})
		}
	}
}
