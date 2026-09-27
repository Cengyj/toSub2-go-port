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

func TestJobsHTTPDiscoversLateCLIOutputAndCheckpoint(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		t.Run(method, func(t *testing.T) {
			t.Setenv("SUB2API_BASE_URL", "")
			t.Setenv("SUB2API_ADMIN_API_KEY", "")
			st, _ := store.New(t.TempDir())
			m := jobs.NewManager(context.Background(), 1, st, nil)
			vault := credentials.NewMemoryStore()
			m.SetCredentialStore(vault)
			if err := m.Restore(context.Background()); err != nil {
				t.Fatal(err)
			}
			s := NewServer(config.Config{OutputRoot: st.Root, StartupToken: "discovery-token"}, m, embed.FS{}, nil, vault)
			defer s.Close()
			server := httptest.NewServer(s.Handler())
			defer server.Close()
			// Files are installed after the running server and empty startup restore.
			for _, id := range []string{"late-output", "late-checkpoint"} {
				dir, _ := store.New(filepath.Join(st.Root, id))
				email := id + "@example.test"
				file := "sub2api-import-oauth.json"
				value := map[string]any{"type": "sub2api-data", "accounts": []any{map[string]any{"credentials": map[string]any{"email": email, "access_token": "Synthetic-private-token"}}}}
				if id == "late-checkpoint" {
					file = "login-checkpoint.json"
					value = map[string]any{"version": 1, "email": email, "stage": "phone_otp", "oauth": map[string]any{"phone": "+12025550120"}}
				}
				if err := dir.WriteJSON(context.Background(), file, value); err != nil {
					t.Fatal(err)
				}
			}
			request := func(method, path string, body []byte) (int, []byte, http.Header) {
				t.Helper()
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				req, _ := http.NewRequestWithContext(ctx, method, server.URL+path, bytes.NewReader(body))
				req.Header.Set("X-Console-Token", "discovery-token")
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
			path := "/api/jobs"
			var body []byte
			if method == http.MethodPost {
				path = "/api/jobs/query"
				body = []byte(`{"emails":["late-output@example.test","late-checkpoint@example.test"]}`)
			}
			status, b, _ := request(method, path, body)
			if status != 200 {
				t.Fatalf("list HTTP %d: %s", status, b)
			}
			var response struct {
				Jobs      []map[string]any
				Selection []map[string]any
			}
			if err := json.Unmarshal(b, &response); err != nil {
				t.Fatal(err)
			}
			if len(response.Jobs) != 2 || len(response.Selection) != 2 {
				t.Fatalf("live listing missed new output/checkpoint: %s", b)
			}
			if strings.Contains(string(b), "Synthetic-private-token") {
				t.Fatal("list leaked CLI token")
			}
			for _, j := range response.Jobs {
				if j["id"] == "late-checkpoint" && (j["status"] != "resume_available" || j["currentPhone"] != "+12025550120") {
					t.Fatal("late checkpoint cannot continue phone flow")
				}
			}
			status, b, headers := request(http.MethodGet, "/api/jobs/late-output/download", nil)
			if status != 200 || headers.Get("Content-Disposition") == "" || !json.Valid(b) || !strings.Contains(string(b), "Synthetic-private-token") {
				t.Fatal("late CLI output is not downloadable")
			}
			status, b, _ = request(http.MethodPost, "/api/jobs/late-checkpoint/retry", []byte("{}"))
			if status != 200 && status != 202 {
				t.Fatalf("resume HTTP %d: %s", status, b)
			}
			if j, _ := m.Get("late-checkpoint"); j.Operation != "resume" || j.Status != jobs.StatusQueued {
				t.Fatal("late checkpoint retry lost resume operation")
			}
			// A subsequent poll must retain the newly queued in-memory operation.
			_, _, _ = request(method, path, body)
			if j, _ := m.Get("late-checkpoint"); j.Status != jobs.StatusQueued {
				t.Fatal("poll reloaded checkpoint over a live queued task")
			}
		})
	}
}
