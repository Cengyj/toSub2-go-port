package jobs

import (
	"fmt"
	"maps"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"tosub2-go-port/internal/credentials"
)

// 1,000 accounts retaining 200 bounded log lines is a supported single-instance
// workload. Its aggregate is larger than the generic settings-file read limit.
// A successful metadata commit must remain readable at the next initialization.
func TestLargeMetadataRestoresAccountsHistoryTrashAndCredentialReferences(t *testing.T) {
	m, disk, vault := trashManager(t)
	defer m.cancel()
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	logs := make([]string, 200)
	for i := range logs {
		// Fixed inert padding keeps this a byte-capacity/restart regression.
		// Arbitrary credential-looking log tokenization has separate tests and
		// need not be repeated 200,000 times under the race instrumenter.
		logs[i] = fmt.Sprintf("event %03d %s", i, strings.Repeat(".", 188))
	}
	for i := 0; i < 1000; i++ {
		id := fmt.Sprintf("capacity-%04d", i)
		m.jobs[id] = &Job{ID: id, Email: id + "@example.test", Status: StatusImported, CreatedAt: at, UpdatedAt: at, LastOperationAt: at,
			Logs: logs, LogEntries: []LogEntry{{At: at, OperationAt: at, Operation: "import", Level: "info", Message: "Synthetic structured event"}},
			ManualExpiresAt: "2027-01-02T03:04:05Z", AuthorizationMethod: AuthorizationMethodChatGPT}
	}
	m.jobs["capacity-0000"].HasPassword = true
	if err := vault.Save("capacity-0000@example.test", credentials.Credentials{Password: "Synthetic-preserved-secret9!"}); err != nil {
		t.Fatal(err)
	}
	history := &Job{ID: "capacity-history", Email: "capacity-0000@example.test", Status: StatusCanceled, CreatedAt: at.Add(-time.Hour), UpdatedAt: at, LastOperationAt: at, OperationStatus: "canceled", Logs: []string{"Retained earlier attempt"}, RecoveryPaused: true}
	m.jobs[history.ID] = history
	trashed := &Job{ID: "capacity-trashed", Email: "trash-capacity@example.test", Status: StatusFailed, CreatedAt: at, UpdatedAt: at, LastOperationAt: at, OperationStatus: "interrupted", Logs: []string{"Retained trash attempt"}, RecoveryPaused: true, AutoRepairSyncPaused: true}
	m.trash["capacity-trash"] = &TrashEntry{ID: "capacity-trash", Email: trashed.Email, State: TrashStateTrashed, DeletedAt: at, Jobs: []*Job{trashed}}
	// Prepare the fixed synthetic v2 records without repeating the production
	// redactor before the restart under test. Restore still uses the real
	// synchronous metadata writer/redactor for every record. Both commits use
	// the production paired size boundary, never a manually oversized byte file.
	template := jobMetadata(m.jobs["capacity-0000"])
	rows := make([]map[string]any, 0, len(m.jobs))
	for id, job := range m.jobs {
		if id == history.ID {
			rows = append(rows, jobMetadata(job))
			continue
		}
		row := maps.Clone(template)
		row["id"], row["email"], row["has_password"] = id, job.Email, job.HasPassword
		rows = append(rows, row)
	}
	document := map[string]any{"version": 2, "jobs": rows, "trash": []any{map[string]any{
		"id": "capacity-trash", "email": trashed.Email, "state": TrashStateTrashed, "deletedAt": at, "jobs": []any{jobMetadata(trashed)},
	}}}
	if err := disk.WriteJSONWithLimit(t.Context(), "job-meta.json", document, MetadataJSONLimit); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(disk.Path("job-meta.json"))
	if err != nil || info.Size() <= 32<<20 {
		t.Fatal("fixture did not cross the historical read boundary", err)
	}
	t.Logf("synthetic metadata: %d bytes, 1000 accounts x 200 log lines, one earlier operation, one trash record", info.Size())
	restored := NewManager(t.Context(), 1, disk, nil)
	defer restored.cancel()
	restored.SetCredentialStore(vault)
	if err := restored.Restore(t.Context()); err != nil {
		t.Fatal("successful aggregate commit could not restart", err)
	}
	var committed struct {
		Jobs  []map[string]any `json:"jobs"`
		Trash []map[string]any `json:"trash"`
	}
	if err := disk.ReadJSONWithLimit(t.Context(), "job-meta.json", &committed, MetadataJSONLimit); err != nil || len(committed.Jobs) != len(m.jobs) || len(committed.Trash) != 1 {
		t.Fatal("real restore commit was not readable at the same byte boundary", err)
	}
	if len(restored.jobs) != len(m.jobs) || len(restored.pending) != 0 || len(restored.active) != 0 || len(restored.trash) != 1 {
		t.Fatal("restart lost records or resumed an old action")
	}
	for id, original := range m.jobs {
		got := restored.jobs[id]
		if got == nil || got.Email != original.Email || !got.CreatedAt.Equal(original.CreatedAt) || got.ManualExpiresAt != original.ManualExpiresAt ||
			!reflect.DeepEqual(got.Logs, original.Logs) || !reflect.DeepEqual(got.LogEntries, original.LogEntries) || got.RecoveryPaused != original.RecoveryPaused {
			t.Fatal("restart changed retained account history or recovery scope", id)
		}
	}
	entry := restored.trash["capacity-trash"]
	if entry == nil || entry.State != TrashStateTrashed || !entry.DeletedAt.Equal(at) || len(entry.Jobs) != 1 || !entry.Jobs[0].RecoveryPaused || !reflect.DeepEqual(entry.Jobs[0].Logs, trashed.Logs) {
		t.Fatal("restart changed retained trash or its pause")
	}
	saved, err := vault.Load("capacity-0000@example.test")
	if err != nil || saved.Password != "Synthetic-preserved-secret9!" || !restored.jobs["capacity-0000"].HasPassword {
		t.Fatal("restart lost protected credential reference", err)
	}
}
