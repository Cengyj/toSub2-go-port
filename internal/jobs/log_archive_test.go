package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tosub2-go-port/internal/resetlog"
	"tosub2-go-port/internal/store"
)

func TestLegacyResetLogoutPageKeepsArchiveButDoesNotInventEvidence(t *testing.T) {
	m, _, _ := trashManager(t)
	oldMsg := "本轮结果：新密钥已核对并保存，退出所有设备未确认完成；未执行退出"
	d := &resetlog.Detail{OpRef: "O0123456789abcdef", Seq: 7, Terminal: true, Stage: "local_commit", Reason: "RESET_COMPLETE_LOGOUT_UNCONFIRMED", Commit: "committed", Logout: "not_performed"}
	e := LogEntry{At: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Level: "warning", Message: oldMsg, Stage: "totp_reset_diagnostic", Diagnostic: d}
	j := &Job{ID: "legacy-reset-logout", LogEntries: []LogEntry{e}, Logs: []string{e.At.Format(time.RFC3339) + " [warning] " + oldMsg}}
	if err := m.archiveRetainedLogsLocked(j); err != nil {
		t.Fatal(err)
	}
	// Use only the stored archive, not the in-memory copy.
	page, err := m.ReadLogPage(t.Context(), &Job{ID: j.ID}, "")
	if err != nil || len(page.Entries) != 1 {
		t.Fatal("legacy entry dropped", err)
	}
	view := page.Entries[0]
	if view.Diagnostic == nil || view.Diagnostic.Match != "" || view.Diagnostic.Logout != "not_performed" || !strings.Contains(view.Message, "未记录退出回执") || strings.Contains(view.Message, "新密钥已核对") || strings.Contains(page.Logs, oldMsg) || !strings.HasSuffix(page.Logs, view.Message) {
		t.Fatal("public structured or plain log claimed more than the historical record proves")
	}
	dir, err := m.logDirectory(j.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	names, err := os.ReadDir(dir)
	if err != nil || len(names) != 1 {
		t.Fatal("unexpected archive contents", err)
	}
	rec, err := readLogRecord(t.Context(), dir, names[0].Name())
	if err != nil || rec.Entry == nil || rec.Entry.Message != oldMsg {
		t.Fatal("reading changed the archive or its identity", err)
	}
}

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
		if i == 1 || i == 2 || i == 608 || i == 609 {
			ref, seq := "O0000000000000001", i
			if i > 500 {
				ref, seq = "O0000000000000002", i-607
			}
			d := &resetlog.Detail{OpRef: ref, Seq: seq, AttemptRef: fmt.Sprintf("A%d", seq), Stage: "verify_factor", Reason: "READ_AGAIN", Source: "state_read"}
			m.applyEventLocked(JobEvent{JobID: j.ID, At: start.Add(time.Duration(i) * time.Second), Data: map[string]any{resetlog.DataKey: d}})
			continue
		}
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
	checkPair := func(page LogPage, ref string) {
		t.Helper()
		var seq []int
		for _, entry := range page.Entries {
			if d := entry.Diagnostic; d != nil {
				if d.OpRef != ref || d.AttemptRef != fmt.Sprintf("A%d", d.Seq) {
					t.Fatal("mixed operation/attempt identity")
				}
				seq = append(seq, d.Seq)
			}
		}
		if len(seq) != 2 || seq[0] != 1 || seq[1] != 2 {
			t.Fatal("collapsed or missing attempts", seq)
		}
	}
	checkPair(page, "O0000000000000002")
	checkPair(older, "O0000000000000001")
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
	checkPair(page, "O0000000000000002")
	checkPair(again, "O0000000000000001")
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
	if !page.Entries[0].LegacyText || page.Entries[1].LegacyText {
		t.Fatal("API did not distinguish original text from structured events")
	}
	encoded, err := json.Marshal(page)
	if err != nil {
		t.Fatal(err)
	}
	var decoded LogPage
	if err := json.Unmarshal(encoded, &decoded); err != nil || len(decoded.Entries) != 2 || !decoded.Entries[0].LegacyText || decoded.Entries[1].LegacyText || decoded.Entries[0].Message != long || decoded.Entries[1].Message != "new event" {
		t.Fatal("API legacy projection did not round-trip its marker and event fields", err)
	}
	files, err := os.ReadDir(filepath.Join(st.Root, j.ID, "operation-logs"))
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		data, err := os.ReadFile(filepath.Join(st.Root, j.ID, "operation-logs", file.Name()))
		if err != nil || strings.Contains(string(data), "legacyText") {
			t.Fatal("API display marker changed persisted archive format", err)
		}
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
