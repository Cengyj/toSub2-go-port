package console

import (
	"encoding/json"
	"testing"
	"time"

	"tosub2-go-port/internal/jobs"
)

// Exercise the real API read projection before the production parser. Older
// text is also included in entries for clients that only consume that array.
// The two representations must not become two events in the combined UI.
func TestJobLogsLegacyPageKeepsOccurrencesWithoutWrapperDuplicates(t *testing.T) {
	manager := jobs.NewManager(t.Context(), 1, nil, nil)
	t.Cleanup(func() { _ = manager.Shutdown(t.Context()) })
	at := time.Date(2026, 11, 24, 1, 2, 3, 0, time.UTC)
	plain := "login running"
	timestamped := "2026-11-24T01:02:02Z [warning] retained warning"
	multiline := "older first line\nolder second line"
	entry := jobs.LogEntry{At: at, OperationAt: at, Operation: "relogin", Stage: "login", Status: "running", OperationStatus: "running", Level: "info", Message: plain}
	page, err := manager.ReadLogPage(t.Context(), &jobs.Job{
		ID:         "legacy-page-fixture",
		Logs:       []string{plain, plain, timestamped, multiline, at.Format(time.RFC3339) + " [info] " + plain},
		LogEntries: []jobs.LogEntry{entry},
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Entries) != 5 {
		t.Fatal("API projection lost an original event", len(page.Entries))
	}
	raw, err := json.Marshal(page.Entries)
	if err != nil {
		t.Fatal(err)
	}
	var records []map[string]any
	if err := json.Unmarshal(raw, &records); err != nil {
		t.Fatal(err)
	}
	views := parseLogsFixture(t, jobLogsVM(t), page.Logs, records)
	if len(views) != 5 {
		t.Fatalf("legacy wrapper duplicated or split original events: got %d, want 5", len(views))
	}
	counts := map[string]int{}
	keys := map[string]bool{}
	structured := 0
	for _, view := range views {
		if keys[view.Key] {
			t.Fatal("repeated legacy events lost their occurrence identity")
		}
		keys[view.Key] = true
		if view.OperationAt != "" {
			structured++
			if view.Operation != "relogin" || view.OperationAt != at.Format(time.RFC3339) {
				t.Fatal("plain text stole a structured event's operation identity")
			}
		} else {
			counts[view.Raw]++
		}
		if view.Raw == timestamped && !view.Abnormal {
			t.Fatal("legacy wrapper hid the original warning level")
		}
	}
	if structured != 1 || counts[plain] != 2 || counts[timestamped] != 1 || counts[multiline] != 1 {
		t.Fatal("legacy multiplicity, multiline content or structured identity changed", counts, structured)
	}
}
