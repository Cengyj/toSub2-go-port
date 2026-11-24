package console

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
)

func TestPrivateDownloadsRejectUnsafeSavedFiles(t *testing.T) {
	for _, scenario := range []string{"permissions", "symlink", "oversize"} {
		t.Run(scenario, func(t *testing.T) {
			if runtime.GOOS == "windows" && scenario != "oversize" {
				t.Skip("POSIX permission/symlink fixture; Windows private DACL suite is separate")
			}
			s, m := newMonitorTestServer(t)
			job := savedManualUploadFixture(t, s, m, "private-download")
			path := filepath.Join(s.cfg.OutputRoot, job.ID, "sub2api-import-oauth.json")
			var err error
			switch scenario {
			case "permissions":
				err = os.Chmod(path, 0644)
			case "symlink":
				err = os.Rename(path, path+".original")
				if err == nil {
					err = os.Symlink(path+".original", path)
				}
			case "oversize":
				err = os.Truncate(path, (16<<20)+1)
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, req := range []struct {
				method, path string
				body         any
			}{
				{http.MethodGet, "/api/jobs/" + job.ID + "/download", nil},
				{http.MethodPost, "/api/jobs/download-batch", map[string]any{"ids": []string{job.ID}}},
			} {
				w := frontendRequest(s, req.method, req.path, req.body, "test")
				if w.Code != http.StatusConflict || w.Header().Get("Content-Disposition") != "" || strings.Contains(w.Body.String(), "synthetic-access") {
					t.Fatal("private download read an unsafe file", w.Code)
				}
			}
		})
	}
}

func TestUploadIdentityValidationDoesNotDependOnQualityPolicy(t *testing.T) {
	for _, scenario := range []string{"email", "client", "workspace"} {
		t.Run(scenario, func(t *testing.T) {
			s, m := newMonitorTestServer(t)
			job := savedManualUploadFixture(t, s, m, "identity-upload")
			path := filepath.Join(s.cfg.OutputRoot, job.ID, "sub2api-import-oauth.json")
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var document map[string]any
			if err := json.Unmarshal(data, &document); err != nil {
				t.Fatal(err)
			}
			account := document["accounts"].([]any)[0].(map[string]any)
			c := account["credentials"].(map[string]any)
			claims := map[string]any{"email": job.Email}
			switch scenario {
			case "email":
				claims["email"] = "different@example.test"
			case "client":
				c["client_id"] = "one-client"
				claims["client_id"] = "other-client"
			case "workspace":
				c["chatgpt_account_id"] = "current"
				claims["auth"] = map[string]any{"chatgpt_account_id": "other"}
			}
			raw, _ := json.Marshal(claims)
			c["access_token"] = "fixture." + base64.RawURLEncoding.EncodeToString(raw) + ".signature"
			data, _ = json.Marshal(document)
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			var calls atomic.Int32
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(500) }))
			defer remote.Close()
			settings, err := s.quality.Service.Settings()
			if err != nil || settings.Enabled {
				t.Fatal("fixture must use disabled optional scoring")
			}
			w := frontendRequest(s, http.MethodPost, "/api/sub2api/upload", map[string]any{"ids": []string{job.ID}, "config": map[string]any{"baseUrl": remote.URL, "adminApiKey": "synthetic-key"}}, "test")
			if w.Code < 400 || calls.Load() != 0 || strings.Contains(w.Body.String(), c["access_token"].(string)) {
				t.Fatal("contradictory identity reached remote or leaked", w.Code, calls.Load())
			}
			after, err := os.ReadFile(path)
			if err != nil || string(after) != string(data) {
				t.Fatal("identity refusal rewrote saved authorization")
			}
		})
	}
}
