package console

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/jobs"
)

func TestAccountExpiryHTTPFailureAndCancellationLeavePreviousMetadata(t *testing.T) {
	s, m := newMonitorTestServer(t)
	j, err := m.Submit(jobs.Spec{ID: "expiry-failure", Email: "expiry-failure@example.test", ImportOnly: true, Input: map[string]any{"manualExpiresAt": "2030-01-01T00:00:00Z"}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.cfg.OutputRoot, "job-meta.json")
	backup := path + ".fixture-backup"
	if err := os.Rename(path, backup); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	body := map[string]any{"expiresAt": "2031-01-01T00:00:00Z"}
	w := frontendRequest(s, http.MethodPost, "/api/jobs/"+j.ID+"/expiration", body, "test")
	after, _ := m.Get(j.ID)
	if w.Code != http.StatusConflict || after.ManualExpiresAt != j.ManualExpiresAt || strings.Contains(w.Body.String(), s.cfg.OutputRoot) {
		t.Fatal("failed write reported success or exposed storage")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(backup, path); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	r := httptest.NewRequest(http.MethodPost, "/api/jobs/"+j.ID+"/expiration", strings.NewReader(`{"expiresAt":"2031-01-01T00:00:00Z"}`)).WithContext(ctx)
	w = httptest.NewRecorder()
	s.setAccountExpiry(w, r, j.ID)
	after, _ = m.Get(j.ID)
	if w.Code != http.StatusConflict || after.ManualExpiresAt != j.ManualExpiresAt {
		t.Fatal("canceled request changed metadata")
	}
	// Bare metadata updates stay available even without any credential store.
	w = frontendRequest(s, http.MethodPost, "/api/jobs/"+j.ID+"/expiration", body, "test")
	if w.Code != http.StatusOK {
		t.Fatal("metadata update required credential store", w.Code)
	}
}

func TestAccountExpiryHTTPPersistsEditsAndExplicitClearWithoutAuthorization(t *testing.T) {
	s, m := newMonitorTestServer(t)
	m.SetCredentialStore(credentials.NewMemoryStore())
	response := frontendRequest(s, http.MethodPost, "/api/jobs/batch", map[string]any{"text": "expiry@example.test----synthetic-password----JBSWY3DPEHPK3PXP----2030-10-20 16:30", "timeZone": "Asia/Shanghai"}, "test")
	if response.Code != http.StatusCreated || len(m.List()) != 1 {
		t.Fatal("expiry import failed", response.Code)
	}
	job := m.List()[0]
	path := "/api/jobs/" + job.ID + "/expiration"
	if job.ManualExpiresAt != "2030-10-20T08:30:00Z" || job.Status != jobs.StatusImported || job.Attempts != 0 {
		t.Fatal("import changed authorization or lost expiry")
	}
	for _, token := range []string{"", "invalid"} {
		if w := frontendRequest(s, http.MethodPost, path, map[string]any{"expiresAt": ""}, token); w.Code != http.StatusForbidden {
			t.Fatal("expiry edit is not protected")
		}
	}
	if w := frontendRequest(s, http.MethodGet, path, nil, "test"); w.Code != http.StatusMethodNotAllowed {
		t.Fatal("expiry accepted GET")
	}
	for _, body := range []any{map[string]any{}, map[string]any{"expiresAt": nil}, map[string]any{"expiresAt": 16.3}, map[string]any{"expiresAt": "synthetic-private-invalid"}, map[string]any{"expiresAt": "2030-02-30 16:30"}, map[string]any{"expiresAt": "16:30", "timeZone": "invalid/private-zone"}} {
		w := frontendRequest(s, http.MethodPost, path, body, "test")
		current, _ := m.Get(job.ID)
		if w.Code != http.StatusBadRequest || current.ManualExpiresAt != job.ManualExpiresAt || strings.Contains(w.Body.String(), "private") {
			t.Fatal("invalid edit changed expiry or echoed input", w.Code)
		}
	}
	for _, value := range []string{"2031-01-02T03:04:00+08:00", ""} {
		w := frontendRequest(s, http.MethodPost, path, map[string]any{"expiresAt": value, "timeZone": "Asia/Shanghai"}, "test")
		var result struct {
			Job map[string]any `json:"job"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || w.Code != http.StatusOK || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("edit failed", w.Code)
		}
		want, source := "2031-01-01T19:04:00Z", "manual"
		if value == "" {
			want, source = "", "unknown"
		}
		if result.Job["manualExpiresAt"] != want || result.Job["expiresAt"] != want || result.Job["expiresAtSource"] != source || result.Job["subscriptionExpiresAt"] != "" {
			t.Fatal("edit response omitted or misreported expiry")
		}
		current, _ := m.Get(job.ID)
		if current.Status != jobs.StatusImported || current.Attempts != 0 || current.Operation != job.Operation {
			t.Fatal("metadata edit started authorization")
		}
		info := frontendRequest(s, http.MethodGet, "/api/jobs/"+job.ID+"/account-info", nil, "test")
		var account map[string]any
		if json.Unmarshal(info.Body.Bytes(), &account) != nil || info.Code != http.StatusOK || account["expiresAt"] != want || account["expiresAtSource"] != source || account["manualExpiresAt"] != want || account["subscriptionExpiresAt"] != "" {
			t.Fatal("account info disagrees with saved expiry")
		}
	}
	if w := frontendRequest(s, http.MethodPost, "/api/jobs/missing/expiration", map[string]any{"expiresAt": ""}, "test"); w.Code != http.StatusNotFound {
		t.Fatal("missing account did not return 404")
	}
}

func TestAccountExpiryPublicPriorityAndStructuredBatchValidation(t *testing.T) {
	s, m := newMonitorTestServer(t)
	for _, tc := range []struct{ manual, subscription, want, source string }{
		{"2030-01-01T00:00:00Z", "2031-01-01T00:00:00Z", "2030-01-01T00:00:00Z", "manual"},
		{"", "2031-01-01T00:00:00Z", "2031-01-01T00:00:00Z", "subscription"},
		{"", "", "", "unknown"},
	} {
		j := &jobs.Job{ID: "expiry-public", ManualExpiresAt: tc.manual, SubscriptionExpiresAt: tc.subscription}
		for _, result := range []map[string]any{s.publicJob(j), s.publicSelectionJob(j)} {
			if result["expiresAt"] != tc.want || result["expiresAtSource"] != tc.source {
				t.Fatal("public expiry precedence mismatch")
			}
		}
	}
	for _, invalid := range []any{nil, 42, "synthetic-invalid-private-date"} {
		w := frontendRequest(s, http.MethodPost, "/api/jobs/batch", map[string]any{"jobs": []any{map[string]any{"email": "valid@example.test"}, map[string]any{"email": "invalid@example.test", "input": map[string]any{"manualExpiresAt": invalid}}}}, "test")
		if w.Code != http.StatusBadRequest || len(m.List()) != 0 || strings.Contains(w.Body.String(), "private-date") {
			t.Fatal("invalid structured batch partially imported or exposed input", w.Code)
		}
	}
}
