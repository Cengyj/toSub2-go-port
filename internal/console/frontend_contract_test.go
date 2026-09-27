package console

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/jobs"
)

func frontendRequest(s *Server, method, path string, body any, token string) *httptest.ResponseRecorder {
	var raw []byte
	if body != nil {
		raw, _ = json.Marshal(body)
	}
	r := httptest.NewRequest(method, path, strings.NewReader(string(raw)))
	r.Host = "127.0.0.1"
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Console-Token", token)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}

func TestFrontendBatchDownloadSelectionAndFileIntegrity(t *testing.T) {
	s, m := newMonitorTestServer(t)
	for _, id := range []string{"ready", "waiting", "broken"} {
		if _, err := m.Submit(jobs.Spec{ID: id, Email: id + "@example.com"}); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"ready", "broken"} {
		m.UpdateResult(id, true, "")
		if err := os.MkdirAll(filepath.Join(s.cfg.OutputRoot, id), 0700); err != nil {
			t.Fatal(err)
		}
	}
	output := filepath.Join(s.cfg.OutputRoot, "ready", "sub2api-import-oauth.json")
	data := `{"type":"sub2api-data","version":1,"accounts":[{"name":"ready","credentials":{"access_token":"fixture-token"}}],"proxies":[{"id":1},{"id":1}]}`
	if err := os.WriteFile(output, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	post := func(ids []string) *httptest.ResponseRecorder {
		return frontendRequest(s, "POST", "/api/jobs/download-batch", map[string]any{"ids": ids}, "test")
	}
	w := post([]string{"ready", "waiting", "ready"})
	if w.Code != http.StatusOK {
		t.Fatal(w.Code, w.Body.String())
	}
	var doc struct {
		Accounts []any `json:"accounts"`
		Proxies  []any `json:"proxies"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil || len(doc.Accounts) != 1 || len(doc.Proxies) != 1 {
		t.Fatalf("download duplicates or invalid JSON: %v", err)
	}
	if !strings.Contains(w.Header().Get("Content-Disposition"), "1-accounts-") || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("missing private download headers")
	}
	for _, tc := range []struct {
		ids  []string
		want int
	}{{nil, 400}, {[]string{"ready", "missing"}, 404}, {[]string{"waiting"}, 409}, {[]string{"ready", "broken"}, 409}} {
		w = post(tc.ids)
		if w.Code != tc.want || strings.Contains(w.Body.String(), "fixture-token") || w.Header().Get("Content-Disposition") != "" {
			t.Fatalf("partial or invalid export: status %d wanted %d", w.Code, tc.want)
		}
	}
	if err := os.WriteFile(filepath.Join(s.cfg.OutputRoot, "broken", "sub2api-import-oauth.json"), []byte(`{"accounts":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if w = post([]string{"ready", "broken"}); w.Code != 409 {
		t.Fatal("malformed export silently skipped", w.Code)
	}
	ids := make([]string, 501)
	for i := range ids {
		ids[i] = fmt.Sprint(i)
	}
	if w = post(ids); w.Code != 400 {
		t.Fatal("missing batch limit", w.Code)
	}
}

func TestFrontendSourceExportUsesSecureCredentialsAndRejectsMissingSecrets(t *testing.T) {
	s, m := newMonitorTestServer(t)
	vault := credentials.NewMemoryStore()
	m.SetCredentialStore(vault)
	j, err := m.Submit(jobs.Spec{ID: "source", Email: "source@example.com", Input: map[string]any{
		"password": "Old-password9!", "totpSecret": "JBSWY3DPEHPK3PXP", "mailApiUrl": "https://mail.example/messages", "mailRequestMethod": "POST", "mailRequestBody": "fixture-mail-body",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := vault.Save(j.Email, credentials.Credentials{Password: "New-password9!", TOTPSecret: "GEZDGNBVGY3TQOJQ"}); err != nil {
		t.Fatal(err)
	}
	post := func(ids []string) *httptest.ResponseRecorder {
		return frontendRequest(s, "POST", "/api/jobs/export-source", map[string]any{"ids": ids}, "test")
	}
	w := post([]string{j.ID, j.ID})
	want := "\ufeffsource@example.com----New-password9!----fixture-mail-body----GEZDGNBVGY3TQOJQ\n"
	if w.Code != 200 || w.Body.String() != want || !strings.Contains(w.Header().Get("Content-Disposition"), "1-accounts.txt") {
		t.Fatal("source export failed to preserve the latest private credentials")
	}
	for _, ids := range [][]string{nil, {j.ID, "missing"}} {
		if w := post(ids); w.Code == 200 || strings.Contains(w.Body.String(), "New-password9!") {
			t.Fatal("invalid selection produced partial source output")
		}
	}
	_ = vault.Delete(j.Email)
	m.ConsumeInput(j.ID, "password")
	w = post([]string{j.ID})
	if w.Code != 409 || w.Header().Get("Content-Disposition") != "" {
		t.Fatal("missing stored password silently omitted from export", w.Code)
	}
	m.Input(j.ID, map[string]any{"password": "Known-password9!"})
	_ = vault.Delete(j.Email)
	m.ConsumeInput(j.ID, "totpSecret")
	if w := post([]string{j.ID}); w.Code != 409 {
		t.Fatal("missing stored TOTP silently omitted from export", w.Code)
	}
}

func TestFrontendSourceExportPreservesLegacyMailBodyWithoutPostMethod(t *testing.T) {
	for _, method := range []string{"", "GET"} {
		for _, password := range []string{"", "Synthetic-password9!"} {
			name := method
			if name == "" {
				name = "legacy-no-method"
			}
			if password != "" {
				name += "-password"
			}
			t.Run(name, func(t *testing.T) {
				s, m := newMonitorTestServer(t)
				input := map[string]any{"mailApiUrl": "https://mail.example/messages", "mailRequestBody": "synthetic-original-mail-body", "totpSecret": "JBSWY3DPEHPK3PXP", "password": password}
				if method != "" {
					input["mailRequestMethod"] = method
				}
				j, err := m.Submit(jobs.Spec{ID: "legacy-source", Email: "source@example.com", Input: input})
				if err != nil {
					t.Fatal(err)
				}
				w := frontendRequest(s, "POST", "/api/jobs/export-source", map[string]any{"ids": []string{j.ID}}, "test")
				parts := []string{j.Email}
				if password != "" {
					parts = append(parts, password)
				}
				parts = append(parts, "synthetic-original-mail-body", "JBSWY3DPEHPK3PXP")
				want := "\ufeff" + strings.Join(parts, "----") + "\n"
				if w.Code != http.StatusOK || w.Body.String() != want {
					t.Fatalf("source export discarded the original mail body: status=%d got=%q want=%q", w.Code, w.Body.String(), want)
				}
			})
		}
	}
}

func TestFrontendTOTPSecretIsDedicatedAuthenticatedAndTransient(t *testing.T) {
	s, m := newMonitorTestServer(t)
	j, err := m.Submit(jobs.Spec{ID: "totp", Email: "totp@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	seed, uri := "JBSWY3DPEHPK3PXP", "otpauth://totp/fixture?secret=JBSWY3DPEHPK3PXP"
	m.UpdateTOTPSetup(j.ID, seed, uri, "", false)
	m.Emit(jobs.JobEvent{JobID: j.ID, Stage: "totp_setup_otp", Status: jobs.StatusWaitingInput})
	path := "/api/jobs/" + j.ID + "/totp-setup"
	if w := frontendRequest(s, "GET", path, nil, ""); w.Code != 403 || strings.Contains(w.Body.String(), seed) {
		t.Fatal("unauthenticated TOTP response")
	}
	w := frontendRequest(s, "GET", path, nil, "test")
	var result map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || w.Code != 200 || result["secret"] != seed || result["uri"] != uri || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("React TOTP response contract failed")
	}
	for _, path := range []string{"/api/jobs", "/api/jobs/" + j.ID, "/api/jobs/" + j.ID + "/logs"} {
		if w := frontendRequest(s, "GET", path, nil, "test"); w.Code != 200 || strings.Contains(w.Body.String(), seed) || strings.Contains(w.Body.String(), "otpauth:") {
			t.Fatal("private setup secret leaked to public job response")
		}
	}
	m.Emit(jobs.JobEvent{JobID: j.ID, Status: jobs.StatusSucceeded})
	if w := frontendRequest(s, "GET", path, nil, "test"); w.Code != 404 {
		t.Fatal("setup secret available outside the enrollment prompt")
	}
}

func TestFrontendReauthorizeRejectsMixedBusySelectionBeforeQueueing(t *testing.T) {
	s, m := newMonitorTestServer(t)
	for _, id := range []string{"failed", "busy"} {
		_, err := m.Submit(jobs.Spec{ID: id, Email: id + "@example.com"})
		if err != nil {
			t.Fatal(err)
		}
	}
	m.Cancel("failed")
	m.Emit(jobs.JobEvent{JobID: "failed", Status: jobs.StatusFailed})
	w := frontendRequest(s, "POST", "/api/jobs/reauthorize-batch", map[string]any{"ids": []string{"failed", "busy"}}, "test")
	failed, _ := m.Get("failed")
	if w.Code != 409 || failed.Status != jobs.StatusFailed {
		t.Fatal("mixed reauthorization mutated eligible job before rejecting selection")
	}
	w = frontendRequest(s, "POST", "/api/jobs/relogin-batch", map[string]any{"ids": []string{"failed", "busy"}}, "test")
	var doc map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &doc)
	if w.Code != 200 || doc["started"] != float64(1) || doc["skipped"] != float64(1) {
		t.Fatal("relogin must keep React skip semantics", w.Code)
	}
}

func TestFrontendMonitorConfigurationAndResultFields(t *testing.T) {
	s, _ := newMonitorTestServer(t)
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != "fixture-admin-key" {
			t.Error("monitor did not use configured admin key")
		}
		_, _ = w.Write([]byte(`{"data":{"items":[{"id":1,"email":"absent@example.com","group_ids":[7],"credentials":{"refresh_token":"fixture-remote-secret"}}]}}`))
	}))
	defer remote.Close()
	w := frontendRequest(s, "POST", "/api/sub2api/monitor", map[string]any{"enabled": true, "config": map[string]any{
		"baseUrl": remote.URL, "adminApiKey": "fixture-admin-key", "groupIds": []string{"7"}, "codexFingerprintMode": "full",
	}}, "test")
	var state map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &state); err != nil || w.Code != 200 || state["enabled"] != true || state["configured"] != true || state["intervalMinutes"] != float64(5) {
		t.Fatal("React monitor settings response incompatible", w.Code)
	}
	w = frontendRequest(s, "POST", "/api/sub2api/monitor/check", nil, "test")
	if err := json.Unmarshal(w.Body.Bytes(), &state); err != nil || w.Code != 200 {
		t.Fatal("manual check response incompatible", w.Code)
	}
	for _, key := range []string{"result", "lastResult"} {
		summary, ok := state[key].(map[string]any)
		if !ok || summary["checked"] != float64(1) || summary["missingTask"] != float64(1) {
			t.Fatalf("missing React %s count fields", key)
		}
		for _, count := range []string{"started", "updated", "blocked", "ineligible", "busy", "cooldown"} {
			if _, ok := summary[count].(float64); !ok {
				t.Fatalf("missing monitor summary %s", count)
			}
		}
	}
	if state["lastCheckAt"] == nil || state["running"] != false || strings.Contains(w.Body.String(), "fixture-admin-key") || strings.Contains(w.Body.String(), "fixture-remote-secret") {
		t.Fatal("monitor response leaked credentials or lost status")
	}
	if got := s.monitor.Configuration(); got.CodexFingerprintMode != "full" || len(got.GroupIDs) != 1 || got.GroupIDs[0] != 7 {
		t.Fatal("React monitor configuration ignored")
	}
}
