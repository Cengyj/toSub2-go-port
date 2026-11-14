package console

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"tosub2-go-port/internal/jobs"
)

func savedManualUploadFixture(t *testing.T, s *Server, manager *jobs.Manager, id string) *jobs.Job {
	t.Helper()
	job, err := manager.Submit(jobs.Spec{ID: id, Email: id + "@example.test", ImportOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := writeFlowExport(s.cfg.OutputRoot, id, job.Email); err != nil {
		t.Fatal(err)
	}
	manager.UpdateResult(id, true, "")
	manager.Emit(jobs.JobEvent{JobID: id, Status: jobs.StatusSucceeded})
	return job
}

func TestManualSub2APIUploadRequiresCommittedPrivateGrant(t *testing.T) {
	for _, kind := range []string{"pending-refresh", "unsafe-mode", "symlink", "oversize"} {
		t.Run(kind, func(t *testing.T) {
			s, manager := newMonitorTestServer(t)
			job := savedManualUploadFixture(t, s, manager, "private-upload")
			path := filepath.Join(s.cfg.OutputRoot, job.ID, "sub2api-import-oauth.json")
			var err error
			switch kind {
			case "pending-refresh":
				err = os.WriteFile(filepath.Join(filepath.Dir(path), ".sub2api-import-oauth.json.refresh-grant-recovery.json"), []byte(`{}`), 0600)
			case "unsafe-mode":
				if runtime.GOOS == "windows" {
					t.Skip("POSIX permission fixture; Windows DACL checks are separate")
				}
				err = os.Chmod(path, 0644)
			case "symlink":
				if err = os.Rename(path, path+".original"); err == nil {
					err = os.Symlink(path+".original", path)
				}
			case "oversize":
				err = os.Truncate(path, (16<<20)+1)
			}
			if err != nil {
				if runtime.GOOS == "windows" && kind == "symlink" {
					t.Skip("symlink fixture requires host support")
				}
				t.Fatal(err)
			}
			var requests atomic.Int32
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { requests.Add(1); w.WriteHeader(500) }))
			defer remote.Close()
			w := frontendRequest(s, http.MethodPost, "/api/sub2api/upload", map[string]any{"ids": []string{job.ID}, "config": map[string]any{"baseUrl": remote.URL, "adminApiKey": "fixture-admin"}}, "test")
			if w.Code != http.StatusConflict || requests.Load() != 0 {
				t.Fatalf("unsafe/uncommitted grant reached remote: status=%d requests=%d", w.Code, requests.Load())
			}
		})
	}
}

func TestManualSub2APIUploadRechecksGrantAfterLookup(t *testing.T) {
	for _, change := range []string{"replace", "pending-refresh"} {
		t.Run(change, func(t *testing.T) {
			s, manager := newMonitorTestServer(t)
			job := savedManualUploadFixture(t, s, manager, "changed-upload")
			path := filepath.Join(s.cfg.OutputRoot, job.ID, "sub2api-import-oauth.json")
			var writes atomic.Int32
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					var err error
					if change == "replace" {
						data, readErr := os.ReadFile(path)
						err = readErr
						if err == nil {
							err = os.WriteFile(path, []byte(strings.ReplaceAll(string(data), "synthetic-access", "new-synthetic-access")), 0600)
						}
					} else {
						err = os.WriteFile(filepath.Join(filepath.Dir(path), ".sub2api-import-oauth.json.refresh-grant-recovery.json"), []byte(`{}`), 0600)
					}
					if err != nil {
						t.Error(err)
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"items": []any{}, "total": 0}})
					return
				}
				writes.Add(1)
				_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"created": 1}})
			}))
			defer remote.Close()
			w := frontendRequest(s, http.MethodPost, "/api/sub2api/upload", map[string]any{"ids": []string{job.ID}, "config": map[string]any{"baseUrl": remote.URL, "adminApiKey": "fixture-admin"}}, "test")
			if w.Code != http.StatusConflict || writes.Load() != 0 || !strings.Contains(w.Body.String(), `"uploaded":0`) || !strings.Contains(w.Body.String(), `"uncertain":false`) {
				t.Fatalf("changed grant was uploaded: status=%d writes=%d receipt=%s", w.Code, writes.Load(), w.Body.String())
			}
		})
	}
}

func TestManualSub2APIUploadKeepsProgressWhenLaterGrantChanges(t *testing.T) {
	s, manager := newMonitorTestServer(t)
	first := savedManualUploadFixture(t, s, manager, "upload-first")
	second := savedManualUploadFixture(t, s, manager, "upload-second")
	accounts := []any{
		map[string]any{"id": 1, "platform": "openai", "type": "oauth", "status": "active", "credentials": map[string]any{"email": first.Email}},
		map[string]any{"id": 2, "platform": "openai", "type": "oauth", "status": "active", "credentials": map[string]any{"email": second.Email}},
	}
	var writes atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			var data any = map[string]any{"items": accounts, "total": 2}
			if strings.HasSuffix(r.URL.Path, "/1") {
				data = accounts[0]
			} else if strings.HasSuffix(r.URL.Path, "/2") {
				data = accounts[1]
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
			return
		}
		writes.Add(1)
		if err := os.WriteFile(filepath.Join(s.cfg.OutputRoot, second.ID, ".sub2api-import-oauth.json.refresh-grant-recovery.json"), []byte(`{}`), 0600); err != nil {
			t.Error(err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer remote.Close()
	w := frontendRequest(s, http.MethodPost, "/api/sub2api/upload", map[string]any{"ids": []string{first.ID, second.ID}, "config": map[string]any{"baseUrl": remote.URL, "adminApiKey": "fixture-admin"}}, "test")
	var receipt struct {
		Outcome string `json:"outcome"`
		Result  struct {
			Updated      int `json:"updated"`
			Success      int `json:"success"`
			NotAttempted int `json:"not_attempted"`
		} `json:"result"`
	}
	if json.Unmarshal(w.Body.Bytes(), &receipt) != nil || w.Code != http.StatusBadGateway || writes.Load() != 1 || receipt.Outcome != "partial" || receipt.Result.Updated != 1 || receipt.Result.Success != 1 || receipt.Result.NotAttempted != 1 {
		t.Fatalf("later block lost known progress: status=%d writes=%d receipt=%s", w.Code, writes.Load(), w.Body.String())
	}
}

func TestManualSub2APIUploadKeepsLegacyGrantSafety(t *testing.T) {
	for _, scenario := range []string{"legacy-only", "pending-current-refresh", "pending-legacy-refresh", "current-appears"} {
		t.Run(scenario, func(t *testing.T) {
			s, manager := newMonitorTestServer(t)
			job := savedManualUploadFixture(t, s, manager, "legacy-upload")
			directory := filepath.Join(s.cfg.OutputRoot, job.ID)
			current := filepath.Join(directory, "sub2api-import-oauth.json")
			legacy := filepath.Join(directory, "sub2api-data.json")
			if err := os.Rename(current, legacy); err != nil {
				t.Fatal(err)
			}
			journal := ""
			if scenario == "pending-current-refresh" {
				journal = ".sub2api-import-oauth.json.refresh-grant-recovery.json"
			} else if scenario == "pending-legacy-refresh" {
				journal = ".sub2api-data.json.refresh-grant-recovery.json"
			}
			if journal != "" {
				if err := os.WriteFile(filepath.Join(directory, journal), []byte(`{}`), 0600); err != nil {
					t.Fatal(err)
				}
			}
			var reads, writes atomic.Int32
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					reads.Add(1)
					if scenario == "current-appears" {
						if err := writeFlowExport(s.cfg.OutputRoot, job.ID, job.Email); err != nil {
							t.Error(err)
						}
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"items": []any{}, "total": 0}})
					return
				}
				writes.Add(1)
				_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"created": 1}})
			}))
			defer remote.Close()
			w := frontendRequest(s, http.MethodPost, "/api/sub2api/upload", map[string]any{"ids": []string{job.ID}, "config": map[string]any{"baseUrl": remote.URL, "adminApiKey": "fixture-admin"}}, "test")
			if scenario == "legacy-only" {
				if w.Code != http.StatusOK || writes.Load() != 1 {
					t.Fatalf("safe legacy grant lost compatibility: status=%d writes=%d", w.Code, writes.Load())
				}
			} else if w.Code != http.StatusConflict || writes.Load() != 0 || journal != "" && reads.Load() != 0 {
				t.Fatalf("legacy path bypassed committed grant: status=%d reads=%d writes=%d", w.Code, reads.Load(), writes.Load())
			}
		})
	}
}
