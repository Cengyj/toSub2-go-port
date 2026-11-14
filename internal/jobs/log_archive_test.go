package jobs

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tosub2-go-port/internal/store"
)

func TestLogArchiveRetainsBeyondSummaryAndPagesAfterTrashRestart(t *testing.T) {
	m, st, vault := trashManager(t)
	j, err := m.Submit(Spec{ID: "log-history", Email: "history@example.test", ImportOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	m.mu.Lock()
	current := m.jobs[j.ID]
	for i := 0; i < 620; i++ {
		m.appendOperationLogLocked(current, JobEvent{JobID: j.ID, At: start.Add(time.Duration(i) * time.Second), Stage: "synthetic", Status: "running", Prompt: fmt.Sprintf("synthetic event %03d", i)})
	}
	if len(current.LogEntries) != 200 {
		t.Fatal("summary unbounded", len(current.LogEntries))
	}
	m.persistLocked()
	m.mu.Unlock()
	page, err := m.ReadLogPage(t.Context(), current, "")
	if err != nil {
		t.Fatal(err)
	}
	if !page.HasMore || len(page.Entries) != 500 || !strings.Contains(page.Logs, "event 619") {
		t.Fatal("latest page incomplete", len(page.Entries))
	}
	older, err := m.ReadLogPage(t.Context(), current, page.NextBefore)
	if err != nil {
		t.Fatal(err)
	}
	// The initial import event is retained too, giving 621 total records.
	if older.HasMore || len(older.Entries) != 121 || !strings.Contains(older.Logs, "event 000") {
		t.Fatal("old history missing", len(older.Entries))
	}
	if _, err = m.TrashAccounts(t.Context(), []string{j.ID}); err != nil {
		t.Fatal(err)
	}
	restarted := NewManager(t.Context(), 1, st, nil)
	restarted.SetCredentialStore(vault)
	if err = restarted.Restore(t.Context()); err != nil {
		t.Fatal(err)
	}
	entry := restarted.ListTrash()[0]
	page, err = restarted.ReadLogPage(t.Context(), entry.Jobs[0], "")
	if err != nil || len(page.Entries) != 500 {
		t.Fatal("archive lost on restart", err)
	}
	if _, err = restarted.RestoreTrash(t.Context(), []string{entry.ID}); err != nil {
		t.Fatal(err)
	}
	active, _ := restarted.Get(j.ID)
	again, err := restarted.ReadLogPage(t.Context(), active, page.NextBefore)
	if err != nil || len(again.Entries) != 121 {
		t.Fatal("restore lost old logs", err)
	}
}
func TestLogArchiveFailureRetainsUnsavedSummaryAndBlocksTrash(t *testing.T) {
	m, st, _ := trashManager(t)
	j, err := m.Submit(Spec{ID: "failed-archive", Email: "failure@example.test", ImportOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(st.Root, j.ID, "operation-logs")
	if err = m.archiveRetainedLogsLocked(j); err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(filepath.Dir(dir), 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(dir, dir+".saved"); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(dir, []byte("synthetic blocker"), 0600); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	current := m.jobs[j.ID]
	current.Logs = nil
	current.LogEntries = nil
	for i := 0; i < 205; i++ {
		m.appendOperationLogLocked(current, JobEvent{JobID: j.ID, Stage: "synthetic", Status: "running", Prompt: fmt.Sprint(i)})
	}
	if len(current.LogEntries) != 205 || current.LastError == "" {
		t.Fatal("failed archive silently truncated")
	}
	m.mu.Unlock()
	if _, err = m.TrashAccounts(t.Context(), []string{j.ID}); err == nil {
		t.Fatal("unarchived history moved")
	}
	if _, ok := m.Get(j.ID); !ok {
		t.Fatal("failure removed active record")
	}
	if err = os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(dir+".saved", dir); err != nil {
		t.Fatal(err)
	}
	if _, err = m.TrashAccounts(t.Context(), []string{j.ID}); err != nil {
		t.Fatal(err)
	}
	page, err := m.ReadLogPage(t.Context(), m.ListTrash()[0].Jobs[0], "")
	if err != nil || len(page.Entries) != 206 {
		t.Fatal("retry lost history", err)
	}
}
func TestLogArchiveRedactsAndRejectsUnsafePathsAndCanceledReads(t *testing.T) {
	st, _ := store.New(t.TempDir())
	m := NewManager(t.Context(), 1, st, nil)
	job := &Job{ID: "safe-archive", Logs: []string{"Authorization: Bearer synthetic-private-token"}, LogEntries: []LogEntry{{At: time.Now().UTC(), Message: "access_token=synthetic-private-token", Level: "info"}}}
	if err := m.archiveRetainedLogsLocked(job); err != nil {
		t.Fatal(err)
	}
	files, err := os.ReadDir(filepath.Join(st.Root, job.ID, "operation-logs"))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		b, err := os.ReadFile(filepath.Join(st.Root, job.ID, "operation-logs", f.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "synthetic-private-token") {
			t.Fatal("secret reached archive")
		}
	}
	if _, err = m.ReadLogPage(t.Context(), job, "../../secret"); err == nil {
		t.Fatal("invalid cursor accepted")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err = m.ReadLogPage(ctx, job, ""); err == nil {
		t.Fatal("canceled read continued")
	}
	bad := &Job{ID: "../escape", Logs: []string{"log"}}
	if err = m.archiveRetainedLogsLocked(bad); err == nil {
		t.Fatal("unsafe identity accepted")
	}
}

func TestLogArchiveRejectsExistingCorruptionAndRetainsDuplicateLegacyLines(t *testing.T) {
	st, _ := store.New(t.TempDir())
	m := NewManager(t.Context(), 1, st, nil)
	j := &Job{ID: "legacy-logs", Logs: []string{"legacy repeated line", "legacy repeated line"}}
	if err := m.archiveRetainedLogsLocked(j); err != nil {
		t.Fatal(err)
	}
	page, err := m.ReadLogPage(t.Context(), j, "")
	if err != nil || strings.Count(page.Logs, "legacy repeated line") != 2 {
		t.Fatal("duplicate legacy records collapsed", page, err)
	}
	files, err := os.ReadDir(filepath.Join(st.Root, j.ID, "operation-logs"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(st.Root, j.ID, "operation-logs", files[0].Name())
	if err = os.WriteFile(path, []byte("{truncated"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = m.archiveRetainedLogsLocked(j); err == nil {
		t.Fatal("existing corruption accepted as durable logs")
	}
}

func TestLogArchiveKeepsMixedLongLegacyAndStructuredRecords(t *testing.T) {
	st, _ := store.New(t.TempDir())
	m := NewManager(t.Context(), 1, st, nil)
	long := strings.Repeat("synthetic legacy detail ", 4000)
	j := &Job{ID: "mixed-history", Logs: []string{long}, LogEntries: []LogEntry{{At: time.Now().UTC(), Level: "info", Message: "new event"}}}
	if err := m.archiveRetainedLogsLocked(j); err != nil {
		t.Fatal(err)
	}
	page, err := m.ReadLogPage(t.Context(), &Job{ID: j.ID}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Entries) != 2 || !strings.Contains(page.Logs, long) || page.Entries[0].Message != long {
		t.Fatal("mixed or long history lost")
	}
}

func TestLogArchiveLegacyDuplicateMultiplicitySurvivesGradualTrimming(t *testing.T) {
	st, _ := store.New(t.TempDir())
	m := NewManager(t.Context(), 1, st, nil)
	j := &Job{ID: "legacy-duplicates"}
	for i := 0; i < 200; i++ {
		j.Logs = append(j.Logs, "synthetic repeated legacy record")
	}
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 201; i++ {
		m.appendOperationLogLocked(j, JobEvent{JobID: j.ID, At: start.Add(time.Duration(i) * time.Second), Stage: "synthetic", Status: "running", Prompt: fmt.Sprintf("new record %d", i)})
	}
	page, err := m.ReadLogPage(t.Context(), j, "")
	if err != nil {
		t.Fatal(err)
	}
	if page.HasMore || len(page.Entries) != 401 || strings.Count(page.Logs, "synthetic repeated legacy record") != 200 {
		t.Fatal("legacy multiplicity lost after successive trims", len(page.Entries))
	}
}
