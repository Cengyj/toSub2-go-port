package console

import (
	"context"
	"embed"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tosub2-go-port/internal/config"
	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/store"
)

func trashConsole(t *testing.T) (*Server, *jobs.Manager, string) {
	t.Helper()
	root := t.TempDir()
	st, err := store.New(root)
	if err != nil {
		t.Fatal(err)
	}
	m := jobs.NewManager(t.Context(), 1, st, nil)
	vault := credentials.NewMemoryStore()
	m.SetCredentialStore(vault)
	s := NewServer(config.Config{StartupToken: "synthetic-console-token", OutputRoot: root}, m, embed.FS{}, nil, vault)
	t.Cleanup(s.Close)
	return s, m, root
}
func trashRequest(s *Server, method, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Host = "127.0.0.1"
	r.Header.Set("X-Console-Token", "synthetic-console-token")
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}
func TestTrashHTTPDefaultsSettingsAndRevisionConflict(t *testing.T) {
	s, _, _ := trashConsole(t)
	w := trashRequest(s, http.MethodGet, "/api/bootstrap", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"accountTrash":true`) {
		t.Fatal(w.Code, w.Body.String())
	}
	w = trashRequest(s, http.MethodGet, "/api/account-trash/settings", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"retentionDays":0`) {
		t.Fatal(w.Code, w.Body.String())
	}
	w = trashRequest(s, http.MethodPut, "/api/account-trash/settings", `{"schemaVersion":1,"revision":1,"retentionDays":30}`)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	w = trashRequest(s, http.MethodPut, "/api/account-trash/settings", `{"schemaVersion":1,"revision":1,"retentionDays":0}`)
	if w.Code != 409 {
		t.Fatal("stale policy accepted", w.Code)
	}
	for _, body := range []string{`{"schemaVersion":1,"revision":2,"retentionDays":-1}`, `{"schemaVersion":1,"revision":2,"retentionDays":1.5}`, `{"schemaVersion":1,"revision":2,"retentionDays":3651}`} {
		if w = trashRequest(s, http.MethodPut, "/api/account-trash/settings", body); w.Code != 400 {
			t.Fatal("invalid policy accepted", w.Code)
		}
	}
}
func TestTrashHTTPLegacyDeleteRetainsHistoryAndRequiresPurgeConfirmation(t *testing.T) {
	s, m, root := trashConsole(t)
	for _, id := range []string{"old-job", "new-job"} {
		if _, err := m.Submit(jobs.Spec{ID: id, Email: "archive@example.test", ImportOnly: true}); err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join(root, id)
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "synthetic-artifact"), []byte("retained synthetic data"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	w := trashRequest(s, http.MethodPost, "/api/jobs/delete-batch", `{"ids":["new-job"]}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"moved":1`) || !strings.Contains(w.Body.String(), "old-job") {
		t.Fatal(w.Code, w.Body.String())
	}
	if len(m.List()) != 0 || len(m.ListTrash()) != 1 {
		t.Fatal("history not grouped")
	}
	entry := m.ListTrash()[0]
	for _, id := range []string{"old-job", "new-job"} {
		if _, err := os.Stat(filepath.Join(root, id, "synthetic-artifact")); err != nil {
			t.Fatal("soft delete removed data", err)
		}
	}
	w = trashRequest(s, http.MethodGet, "/api/account-trash", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"recordCount":2`) {
		t.Fatal(w.Code, w.Body.String())
	}
	w = trashRequest(s, http.MethodGet, "/api/account-trash/"+entry.ID+"/logs?jobId=unrelated", "")
	if w.Code != 404 {
		t.Fatal("cross-account logs exposed")
	}
	body, _ := json.Marshal(map[string]any{"ids": []string{entry.ID}})
	w = trashRequest(s, http.MethodPost, "/api/account-trash/purge", string(body))
	if w.Code != 400 || len(m.ListTrash()) != 1 {
		t.Fatal("unconfirmed purge")
	}
	w = trashRequest(s, http.MethodPost, "/api/account-trash/restore", string(body))
	if w.Code != 200 || len(m.List()) != 2 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w = trashRequest(s, http.MethodPost, "/api/account-trash/move", `{"ids":["old-job"]}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	entry = m.ListTrash()[0]
	body, _ = json.Marshal(map[string]any{"ids": []string{entry.ID}, "confirmPermanent": true})
	w = trashRequest(s, http.MethodPost, "/api/account-trash/purge", string(body))
	if w.Code != 200 || len(m.ListTrash()) != 0 {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, id := range []string{"old-job", "new-job"} {
		if _, err := os.Stat(filepath.Join(root, id)); !os.IsNotExist(err) {
			t.Fatal("purge left private directory", err)
		}
	}
}

func TestTrashHTTPHistoryCarriesLastOperationOutcomeAndTime(t *testing.T) {
	s, m, _ := trashConsole(t)
	job, err := m.Submit(jobs.Spec{ID: "outcome-history", Email: "history@example.test", ImportOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	queued, err := m.QueueOperation(job.ID, "relogin", nil)
	if err != nil || !m.Cancel(job.ID) {
		t.Fatal("could not create a canceled retained operation", err)
	}
	if w := trashRequest(s, http.MethodPost, "/api/account-trash/move", `{"ids":["outcome-history"]}`); w.Code != http.StatusOK {
		t.Fatal(w.Code, w.Body.String())
	}
	w := trashRequest(s, http.MethodGet, "/api/account-trash", "")
	var reply struct {
		Entries []struct {
			Jobs []struct {
				Status, OperationStatus string
				LastOperationAt         time.Time
				HasLogs                 bool
			}
		}
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &reply) != nil || len(reply.Entries) != 1 || len(reply.Entries[0].Jobs) != 1 {
		t.Fatal(w.Code, w.Body.String())
	}
	row := reply.Entries[0].Jobs[0]
	if row.OperationStatus != "canceled" || !row.LastOperationAt.Equal(queued.LastOperationAt) || !row.HasLogs {
		t.Fatalf("operation outcome or log grouping lost: %+v", row)
	}
}
func TestTrashHTTPStorageFailureNeverFallsBackToHardDelete(t *testing.T) {
	s, m, root := trashConsole(t)
	if _, err := m.Submit(jobs.Spec{ID: "retained", Email: "retained@example.test", ImportOnly: true}); err != nil {
		t.Fatal(err)
	}
	s.recycleBin = nil
	w := trashRequest(s, http.MethodPost, "/api/jobs/delete-batch", `{"ids":["retained"]}`)
	if w.Code != 503 {
		t.Fatal(w.Code)
	}
	if _, ok := m.Get("retained"); !ok {
		t.Fatal("failed storage used destructive fallback")
	}
	if _, err := os.Stat(filepath.Join(root, "job-meta.json")); err != nil {
		t.Fatal(err)
	}
}

func TestTrashHTTPReportsCancellationUncertaintyAndPurgePending(t *testing.T) {
	s, _, _ := trashConsole(t)
	w := httptest.NewRecorder()
	s.writeTrashOutcome(w, "moved", 0, []string{}, jobs.ErrCancellationPersistence)
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if w.Code != 503 || got["outcome"] != "unknown" || got["uncertain"] != true || got["error"] != jobs.ErrCancellationPersistence.Error() {
		t.Fatal("unsafe cancellation receipt", w.Code, got)
	}
	w = httptest.NewRecorder()
	s.writeTrashOutcome(w, "purged", 0, []string{}, os.ErrPermission, true)
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["outcome"] != "partial" || got["purgePending"] != true || !strings.Contains(got["error"].(string), "不可恢复") {
		t.Fatal("pending purge called ordinary failure", got)
	}
}

func TestTrashHTTPDuplicateCannotReleaseAnotherSMSFence(t *testing.T) {
	s, m, _ := trashConsole(t)
	j, err := m.Submit(jobs.Spec{ID: "concurrent-trash", Email: "concurrent@example.test", ImportOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	m.SetTrashPauseHook(func(ctx context.Context, selected []*jobs.Job) error {
		s.smsMu.Lock()
		s.smsTrashing[j.ID] = true
		s.smsMu.Unlock()
		close(entered)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	body, _ := json.Marshal(map[string]any{"ids": []string{j.ID}})
	first, second := make(chan *httptest.ResponseRecorder, 1), make(chan *httptest.ResponseRecorder, 1)
	go func() { first <- trashRequest(s, http.MethodPost, "/api/account-trash/move", string(body)) }()
	<-entered
	go func() { second <- trashRequest(s, http.MethodPost, "/api/account-trash/move", string(body)) }()
	select {
	case w := <-second:
		t.Fatalf("duplicate escaped action gate: %d", w.Code)
	case <-time.After(20 * time.Millisecond):
	}
	s.smsMu.Lock()
	held := s.smsTrashing[j.ID]
	s.smsMu.Unlock()
	if !held {
		t.Fatal("duplicate unlocked first transaction SMS fence")
	}
	close(release)
	if w := <-first; w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := <-second; w.Code != 404 {
		t.Fatal(w.Code, w.Body.String())
	}
	s.smsMu.Lock()
	held = s.smsTrashing[j.ID]
	s.smsMu.Unlock()
	if held {
		t.Fatal("completed transaction leaked SMS fence")
	}
}
