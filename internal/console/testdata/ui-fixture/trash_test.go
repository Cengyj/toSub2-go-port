package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func trashFixtureRequest(t *testing.T, f *trashFixture, active *[]map[string]any, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var data []byte
	if body != nil {
		var err error
		data, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(method, path, bytes.NewReader(data))
	if !f.handle(w, r, active, func(w http.ResponseWriter, code int, payload any) {
		w.WriteHeader(code)
		if err := json.NewEncoder(w).Encode(payload); err != nil {
			t.Fatal(err)
		}
	}) {
		t.Fatal("fixture did not handle its route")
	}
	var payload map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	return w.Code, payload
}

func TestTrashFixtureDefaultRemainsOptInAndResettable(t *testing.T) {
	f := newTrashFixture()
	if len(f.entries) != 1 || f.retentionDays != 0 || f.dataset != "default" || len(f.entries["synthetic-trash-1"].jobs) != 2 {
		t.Fatal("default synthetic scenario changed")
	}
	f.configure(url.Values{"trashReadError": {"true"}})
	if len(f.entries) != 1 {
		t.Fatal("an unrelated fault control seeded extra accounts")
	}
	f.configure(url.Values{"trashDataset": {"large"}})
	if len(f.entries) != 84 || f.retentionDays != 7 || f.readError {
		t.Fatal("large scenario did not reset to predictable data")
	}
	f.configure(url.Values{"trashDataset": {"default"}})
	if len(f.entries) != 1 || f.sequence != 1 || f.retentionDays != 0 || f.dataset != "default" {
		t.Fatal("default reset retained large data or strategy")
	}
}

func TestTrashFixtureLargeDatasetHasValidDiverseDTOs(t *testing.T) {
	f := newTrashFixture()
	f.configure(url.Values{"trashDataset": {"large"}})
	active := []map[string]any{}
	code, response := trashFixtureRequest(t, f, &active, http.MethodGet, "/api/account-trash", nil)
	if code != 200 {
		t.Fatal(code)
	}
	rows := response["entries"].([]any)
	if len(rows) != 84 {
		t.Fatal("list lost generated rows", len(rows))
	}
	var longEmails, purging, withoutLogs, largeHistory, nearExpiry int
	seenJobs := map[string]bool{}
	var previous time.Time
	for _, raw := range rows {
		row := raw.(map[string]any)
		deleted, err := time.Parse(time.RFC3339Nano, row["deletedAt"].(string))
		if err != nil || deleted.IsZero() {
			t.Fatal("fixture emitted an invalid required deletion time")
		}
		if !previous.IsZero() && deleted.After(previous) {
			t.Fatal("fixture ordering differs from real newest-first API")
		}
		previous = deleted
		if len(row["email"].(string)) > 90 {
			longEmails++
		}
		if row["state"] == "purging" {
			purging++
			started, err := time.Parse(time.RFC3339Nano, row["purgeStartedAt"].(string))
			if err != nil || started.Before(deleted) || row["canRestore"] != false || len([]rune(row["lastError"].(string))) < 100 {
				t.Fatal("purging fixture lacks valid time, refusal, or long failure text")
			}
		} else if row["state"] != "trashed" || row["canRestore"] != true {
			t.Fatal("invalid state or restore capability")
		}
		expires, err := time.Parse(time.RFC3339Nano, row["expiresAt"].(string))
		if err != nil || expires.Sub(deleted) != 7*24*time.Hour {
			t.Fatal("expiry did not use UTC elapsed policy")
		}
		if remaining := time.Until(expires); remaining > 0 && remaining < time.Hour {
			nearExpiry++
		}
		history := row["jobs"].([]any)
		if int(row["recordCount"].(float64)) != len(history) {
			t.Fatal("record count disagrees with history")
		}
		if len(history) >= 9 {
			largeHistory++
		}
		anyLogs := false
		for _, value := range history {
			job := value.(map[string]any)
			id := job["id"].(string)
			if seenJobs[id] {
				t.Fatal("history identity reused", id)
			}
			seenJobs[id] = true
			if _, leaked := job["email"]; leaked {
				t.Fatal("fixture history invented fields absent from real DTO")
			}
			if _, ok := job["lastOperationType"].(string); !ok {
				t.Fatal("operation type was not a real string field")
			}
			for _, key := range []string{"createdAt", "updatedAt"} {
				if _, err := time.Parse(time.RFC3339Nano, job[key].(string)); err != nil {
					t.Fatal("invalid history time", key)
				}
			}
			anyLogs = anyLogs || job["hasLogs"] == true
		}
		if !anyLogs {
			withoutLogs++
		}
	}
	if longEmails < 10 || purging < 10 || withoutLogs < 10 || largeHistory < 5 || nearExpiry < 4 {
		t.Fatalf("insufficient varied layout cases: email=%d purging=%d empty=%d history=%d near=%d", longEmails, purging, withoutLogs, largeHistory, nearExpiry)
	}
}

func TestTrashFixtureUnknownOptionalTimeAndSettingsMatchAPI(t *testing.T) {
	f := newTrashFixture()
	f.configure(url.Values{"trashDataset": {"large"}, "trashRetentionDays": {"0"}})
	active := []map[string]any{}
	_, response := trashFixtureRequest(t, f, &active, http.MethodGet, "/api/account-trash", nil)
	for _, value := range response["entries"].([]any) {
		if _, exists := value.(map[string]any)["expiresAt"]; exists {
			t.Fatal("disabled strategy invented an expiry")
		}
	}
	code, settings := trashFixtureRequest(t, f, &active, http.MethodGet, "/api/account-trash/settings", nil)
	if code != 200 || settings["schemaVersion"] != float64(1) || settings["retentionDays"] != float64(0) || settings["settings"] != nil {
		t.Fatal("settings GET does not match real top-level DTO")
	}
	code, saved := trashFixtureRequest(t, f, &active, http.MethodPut, "/api/account-trash/settings", map[string]any{"schemaVersion": 1, "revision": f.revision, "retentionDays": 7})
	if code != 200 || saved["retentionDays"] != float64(7) || saved["settings"] != nil {
		t.Fatal("settings PUT does not match real top-level DTO")
	}
	revision := f.revision
	f.configure(url.Values{"trashRetentionDays": {"3651"}})
	if f.revision != revision || f.retentionDays != 7 {
		t.Fatal("invalid debug retention changed strategy")
	}
}

func TestTrashFixturePurgingRestoreRefusesWholeMixedSelection(t *testing.T) {
	f := newTrashFixture()
	f.configure(url.Values{"trashDataset": {"large"}})
	active := []map[string]any{}
	code, receipt := trashFixtureRequest(t, f, &active, http.MethodPost, "/api/account-trash/restore", map[string]any{"ids": []string{"synthetic-trash-1", "synthetic-trash-5"}})
	if code != 409 || receipt["restored"] != float64(0) || receipt["outcome"] != "failed" || len(active) != 0 || len(f.entries) != 84 {
		t.Fatal("mixed restore partly resurrected a selection with an irreversible account", code, receipt)
	}
}

func TestTrashFixturePartialPurgeCannotRestoreAndRetries(t *testing.T) {
	for _, count := range []int{1, 3} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			f := newTrashFixture()
			f.configure(url.Values{"trashDataset": {"large"}, "trashOutcome": {"partial"}})
			active := []map[string]any{}
			ids := []string{}
			for i := 1; i <= count; i++ {
				ids = append(ids, fmt.Sprintf("synthetic-trash-%d", i))
			}
			code, receipt := trashFixtureRequest(t, f, &active, http.MethodPost, "/api/account-trash/purge", map[string]any{"ids": ids, "confirmPermanent": true})
			wantPurged := 0
			if count > 1 {
				wantPurged = 1
			}
			if code != 409 || receipt["purged"] != float64(wantPurged) || receipt["outcome"] != "partial" || receipt["purgePending"] != true {
				t.Fatal("partial purge receipt lost irreversible state", code, receipt)
			}
			remaining := ids[wantPurged:]
			for _, id := range remaining {
				if entry := f.entries[id]; entry == nil || entry.state != "purging" || entry.purgeStartedAt == nil {
					t.Fatal("partial purge failed to retain every irreversible peer", id)
				}
			}
			code, _ = trashFixtureRequest(t, f, &active, http.MethodPost, "/api/account-trash/restore", map[string]any{"ids": remaining})
			if code != 409 || len(active) != 0 {
				t.Fatal("partial purge could be restored")
			}
			f.configure(url.Values{"trashOutcome": {"success"}})
			code, receipt = trashFixtureRequest(t, f, &active, http.MethodPost, "/api/account-trash/purge", map[string]any{"ids": remaining, "confirmPermanent": true})
			if code != 200 || receipt["purged"] != float64(len(remaining)) || receipt["outcome"] != "completed" || receipt["purgePending"] != nil || receipt["error"] != nil {
				t.Fatal("safe purge retry did not complete", code, receipt)
			}
			for _, id := range ids {
				if f.entries[id] != nil {
					t.Fatal("purged account reappeared", id)
				}
			}
		})
	}
}

func TestTrashFixturePurgeRequiresConfirmationAndValidWholeSelection(t *testing.T) {
	f := newTrashFixture()
	active := []map[string]any{}
	for _, body := range []any{map[string]any{"ids": []string{"synthetic-trash-1"}}, map[string]any{"ids": []string{""}, "confirmPermanent": true}, map[string]any{"ids": []string{"synthetic-trash-1", "missing"}, "confirmPermanent": true}} {
		code, _ := trashFixtureRequest(t, f, &active, http.MethodPost, "/api/account-trash/purge", body)
		if code < 400 || len(f.entries) != 1 || f.entries["synthetic-trash-1"].state != "trashed" {
			t.Fatal("invalid purge changed retained data")
		}
	}
}

func TestTrashFixtureEmptyLogsAndRetainedPaginationAreConsistent(t *testing.T) {
	f := newTrashFixture()
	f.configure(url.Values{"trashDataset": {"large"}})
	active := []map[string]any{}
	code, page := trashFixtureRequest(t, f, &active, http.MethodGet, "/api/account-trash/synthetic-trash-4/logs?jobId=archived-004-job-01", nil)
	if code != 200 || page["logs"] != "" || page["hasMore"] != false || len(page["entries"].([]any)) != 0 {
		t.Fatal("no-log row returned invented history")
	}
	code, page = trashFixtureRequest(t, f, &active, http.MethodGet, "/api/account-trash/synthetic-trash-1/logs?jobId=archived-job-1", nil)
	if code != 200 || page["hasMore"] != true || len(page["entries"].([]any)) != 500 {
		t.Fatal("default retained history pagination changed")
	}
	code, page = trashFixtureRequest(t, f, &active, http.MethodGet, "/api/account-trash/synthetic-trash-1/logs?jobId=archived-job-1&before="+url.QueryEscape(page["nextBefore"].(string)), nil)
	if code != 200 || page["hasMore"] != false || len(page["entries"].([]any)) != 120 || !strings.Contains(page["logs"].(string), "Synthetic retained log 1 ") {
		t.Fatal("older retained log page lost ordering or completeness")
	}
}

func TestTrashFixtureUnknownReceiptRequiresReadAndMoveKeepsLegacyCount(t *testing.T) {
	f := newTrashFixture()
	active := []map[string]any{{"id": "move-job", "email": "move@example.invalid", "status": "completed", "createdAt": time.Now().UTC().Format(time.RFC3339)}}
	code, receipt := trashFixtureRequest(t, f, &active, http.MethodPost, "/api/account-trash/move", map[string]any{"ids": []string{"move-job"}})
	if code != 200 || receipt["moved"] != float64(1) || receipt["deleted"] != float64(1) || receipt["error"] != nil {
		t.Fatal("move receipt differs from production compatibility contract")
	}
	f.configure(url.Values{"trashOutcome": {"unknown"}})
	code, receipt = trashFixtureRequest(t, f, &active, http.MethodPost, "/api/account-trash/purge", map[string]any{"ids": []string{"synthetic-trash-1"}, "confirmPermanent": true})
	if code != 503 || receipt["outcome"] != "unknown" || receipt["uncertain"] != true || receipt["purged"] != float64(1) {
		t.Fatal("unknown receipt claimed a confirmed outcome")
	}
	if f.entries["synthetic-trash-1"] != nil {
		t.Fatal("authoritative read cannot resolve a lost successful receipt")
	}
}
