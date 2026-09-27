package console

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tosub2-go-port/internal/config"
	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/store"
)

func TestLegacyRestoredJobsHTTPPreservesOldOutputAndBatchIntent(t *testing.T) {
	t.Setenv("SUB2API_BASE_URL", "")
	t.Setenv("SUB2API_ADMIN_API_KEY", "")
	root, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	operationAt := time.Date(2026, 8, 12, 8, 30, 0, 0, time.UTC)
	entries := []struct {
		id, status, operation, result, want string
		output, checkpoint                  bool
	}{
		{"failed-output", "failed", "relogin", "", "full", true, false},
		{"interrupted", "email_otp", "initial_authorization", "", "full", false, false},
		{"password-result", "password_add_starting", "add_password", "password", "resume", false, true},
		{"totp-result", "totp_starting", "setup_2fa", "totp", "resume", false, true},
		{"completed", "completed", "initial_authorization", "", "reauthorize", true, false},
	}
	for i, entry := range entries {
		dir, err := store.New(filepath.Join(root.Root, entry.id))
		if err != nil {
			t.Fatal(err)
		}
		email := entry.id + "@example.test"
		write := func(name string, v any) {
			t.Helper()
			if err := dir.WriteJSON(context.Background(), name, v); err != nil {
				t.Fatal(err)
			}
		}
		write("job-meta.json", map[string]any{"email": email, "status": entry.status, "last_operation_type": entry.operation, "last_operation_at": operationAt, "created_at": operationAt.Add(time.Duration(i) * time.Minute), "updated_at": operationAt.Add(time.Hour), "last_error": "previous safe error"})
		if entry.output {
			write("sub2api-import-oauth.json", map[string]any{"type": "sub2api-data", "version": 1, "accounts": []any{map[string]any{"name": "oauth---" + email, "credentials": map[string]any{"email": email}}}})
		}
		if entry.checkpoint {
			write("login-checkpoint.json", map[string]any{"version": 1, "email": email, "stage": "email_verified"})
		}
		if entry.result == "password" {
			write("password-add-result.json", map[string]any{"version": 1, "email": email, "password": "Synthetic-Recovered9!"})
		}
		if entry.result == "totp" {
			write("totp-setup-result.json", map[string]any{"version": 1, "email": email, "secret": "JBSWY3DPEHPK3PXP", "activation_succeeded": true})
		}
	}
	m := jobs.NewManager(context.Background(), 1, root, nil)
	vault := credentials.NewMemoryStore()
	m.SetCredentialStore(vault)
	if err := m.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	s := NewServer(config.Config{OutputRoot: root.Root, StartupToken: "fixture-token"}, m, embed.FS{}, nil, vault)
	t.Cleanup(s.Close)
	server := httptest.NewServer(s.Handler())
	defer server.Close()
	request := func(method, path string, body any) (int, []byte, http.Header) {
		t.Helper()
		var input []byte
		if body != nil {
			input, _ = json.Marshal(body)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, method, server.URL+path, bytes.NewReader(input))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-Console-Token", "fixture-token")
		req.Header.Set("Content-Type", "application/json")
		resp, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, b, resp.Header
	}
	ids := []string{}
	for page := 1; page <= 3; page++ {
		path := "/api/jobs?page=" + string(rune('0'+page)) + "&pageSize=2"
		code, b, _ := request("GET", path, nil)
		if code != 200 {
			t.Fatal(code, string(b))
		}
		var response struct{ Jobs []map[string]any }
		if err := json.Unmarshal(b, &response); err != nil {
			t.Fatal(err)
		}
		for _, j := range response.Jobs {
			ids = append(ids, j["id"].(string))
			if j["lastOperationAt"] != operationAt.Format(time.RFC3339) {
				t.Fatal("public operation time was replaced by latest update")
			}
		}
		for _, secret := range []string{"Synthetic-Recovered9!", "JBSWY3DPEHPK3PXP"} {
			if strings.Contains(string(b), secret) {
				t.Fatal("private recovery value leaked to paginated API")
			}
		}
	}
	if len(ids) != len(entries) {
		t.Fatal("pagination omitted restored tasks")
	}
	code, b, h := request("GET", "/api/jobs/failed-output/download", nil)
	if code != 200 || !strings.Contains(h.Get("Content-Disposition"), "attachment") || !json.Valid(b) {
		t.Fatal("old completed output no longer downloadable after failed relogin", code)
	}
	bad := append(append([]string{}, ids...), "stale-selection-id")
	code, _, _ = request("POST", "/api/jobs/reauthorize-batch", map[string]any{"ids": bad})
	if code != 404 {
		t.Fatal("stale cross-page selection was not rejected", code)
	}
	for _, j := range m.List() {
		if j.Status == jobs.StatusQueued {
			t.Fatal("stale selection partially queued jobs")
		}
	}
	code, b, _ = request("POST", "/api/jobs/reauthorize-batch", map[string]any{"ids": append(ids, ids[0])})
	var batch struct {
		Started int
		Jobs    []map[string]any
	}
	if code != 200 || json.Unmarshal(b, &batch) != nil || batch.Started != len(entries) {
		t.Fatal("batch failed to deduplicate and queue complete cross-page selection", code, string(b))
	}
	for _, j := range batch.Jobs {
		if j["lastError"] != "" {
			t.Fatal("retry kept previous lastError in public response")
		}
	}
	for _, entry := range entries {
		j, _ := m.Get(entry.id)
		if j.Operation != entry.want {
			t.Fatalf("%s queued %s, want %s", entry.id, j.Operation, entry.want)
		}
	}
	started := make(chan string, len(entries))
	m.SetRunner(func(ctx context.Context, j *jobs.Job, _ func(jobs.JobEvent)) error { started <- j.ID; return nil })
	m.Start()
	defer m.Shutdown(context.Background())
	for range entries {
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatal("recovered batch did not execute")
		}
	}
}
