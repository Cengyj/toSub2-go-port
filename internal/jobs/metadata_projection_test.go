package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"tosub2-go-port/internal/logging"
	"tosub2-go-port/internal/resetlog"
	"tosub2-go-port/internal/store"
)

func TestMetadataProjectionTracksInPlaceChangesAndSecretContext(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(t.Context(), 1, st, nil)
	defer m.cancel()
	at := time.Now().UTC()
	job := &Job{ID: "cache-fixture", Email: "cache@example.test", Status: StatusImported, CreatedAt: at, StartedAt: &at, Input: map[string]any{"password": "initial-known-value"}, Prompt: "later-known-value", Logs: []string{"later-known-value"}, LogEntries: []LogEntry{{Message: "later-known-value"}}, TOTPIntentReceipts: map[string]TOTPIntentReceipt{"fixture": {ResetIntentID: "fixture-intent", Completed: false}}}
	m.jobs[job.ID] = job
	flush := func() map[string]any {
		t.Helper()
		m.persistLocked()
		if m.lastPersistError != nil {
			t.Fatal(m.lastPersistError)
		}
		var doc map[string]any
		if err := st.ReadJSON(t.Context(), "job-meta.json", &doc); err != nil {
			t.Fatal(err)
		}
		return doc["jobs"].([]any)[0].(map[string]any)
	}
	flush()
	// No timestamp bump is required for a secret, receipt, log, or pointer edit
	// to invalidate the derived record. Cache correctness has no dirty flags.
	job.Input["password"] = "later-known-value"
	job.TOTPIntentReceipts["fixture"] = TOTPIntentReceipt{ResetIntentID: "fixture-intent", Completed: true}
	*job.StartedAt = at.Add(time.Hour)
	row := flush()
	raw, _ := json.Marshal(row)
	if strings.Contains(string(raw), "later-known-value") || strings.Contains(string(raw), "initial-known-value") {
		t.Fatal("secret context change reused unredacted metadata")
	}
	receipt := row["totp_intent_receipts"].(map[string]any)["fixture"].(map[string]any)
	if receipt["completed"] != true || row["started_at"] != job.StartedAt.Format(time.RFC3339Nano) {
		t.Fatal("in-place metadata mutation was omitted")
	}
	job.LogEntries[0].Message = "new public entry"
	job.Logs[0] = "new public line"
	row = flush()
	if row["log_entries"].([]any)[0].(map[string]any)["message"] != "new public entry" || row["logs"].([]any)[0] != "new public line" {
		t.Fatal("in-place log mutation reused old metadata")
	}
	delete(m.jobs, job.ID)
	flushEmpty := func() {
		m.persistLocked()
		if m.lastPersistError != nil {
			t.Fatal(m.lastPersistError)
		}
	}
	flushEmpty()
	if len(m.metadataProjection.current) != 0 || m.metadataProjection.previous != nil {
		t.Fatal("removed account retained cached records or secrets")
	}
}

func TestMetadataLogProjectionBudgetAlwaysFallsBackToFullRedaction(t *testing.T) {
	p := newMetadataLogProjection(metadataProjection{}, false, []string{"private-canary"})
	for i := 0; i < metadataLogProjectionEntries+20; i++ {
		text := fmt.Sprintf("event %04d private-canary", i)
		if got := p.project(text); got != logging.RedactKnown(text, "private-canary") {
			t.Fatal("entry budget bypassed redaction")
		}
	}
	if len(p.values) != metadataLogProjectionEntries || p.bytes > metadataLogProjectionBytes {
		t.Fatal("entry budget was not enforced")
	}
	// An individual text can exceed the byte budget even with room for more
	// entries. It must still be redacted, without retaining the oversized pair.
	p = newMetadataLogProjection(metadataProjection{}, false, []string{"private-canary"})
	oversized := strings.Repeat(" ", metadataLogProjectionBytes/2) + "private-canary"
	if got := p.project(oversized); got != strings.Repeat(" ", metadataLogProjectionBytes/2)+logging.Redacted {
		t.Fatal("byte budget bypassed known-secret redaction")
	}
	if len(p.values) != 0 || p.bytes != 0 {
		t.Fatal("oversized text was retained")
	}
	if got := p.project("password=assignment-canary"); strings.Contains(got, "assignment-canary") {
		t.Fatal("oversized predecessor disabled ordinary redaction")
	}
}

func TestMetadataLogProjectionReusesOnlyCurrentIdentityAndSecretContext(t *testing.T) {
	at := time.Now().UTC()
	job := &Job{ID: "log-projection", Email: "first@example.test", CreatedAt: at, UpdatedAt: at, Status: StatusImported,
		Input: map[string]any{"password": "first-canary"}}
	for i := 0; i < 200; i++ {
		text := fmt.Sprintf("event %03d first-canary later-canary next@example.test", i)
		job.Logs = append(job.Logs, text)
		job.LogEntries = append(job.LogEntries, LogEntry{At: at, Message: text})
	}
	duration := int64(10)
	job.LogEntries[0].Diagnostic = &resetlog.Detail{OpRef: resetlog.OperationRef("synthetic-log-operation"), Seq: 1, Stage: "identity", Reason: "IDENTITY_VERIFIED", DurationMS: &duration}
	var cache metadataProjectionCache
	check := func() {
		t.Helper()
		cache = cache.begin(1)
		got := cache.job(job)
		cache.previous = nil
		if !reflect.DeepEqual(got, jobMetadata(job)) {
			t.Fatal("metadata log reuse differed from the complete public projection")
		}
	}
	check()
	duration++
	if cached := cache.current[job.ID].source.LogEntries[0].Diagnostic; cached == nil || *cached.DurationMS != 10 {
		t.Fatal("cached diagnostic shared its live mutable pointer")
	}
	check()
	// A new event invalidates the whole job snapshot while retaining history.
	job.UpdatedAt = at.Add(time.Second)
	check()
	// A newly learned secret must rescan all old raw text, including both log
	// representations. A changed email is also part of the redaction context.
	job.Input["password"] = "later-canary"
	check()
	job.Email = "next@example.test"
	check()
	job.Logs[100], job.LogEntries[100].Message = "password=changed-canary", "Bearer changed-canary"
	check()
	// Moving the retained window must not keep dropped raw history alive via
	// an extra memo. The temporary text map is never part of the stored cache.
	job.Logs = append([]string(nil), job.Logs[100:]...)
	job.LogEntries = append([]LogEntry(nil), job.LogEntries[100:]...)
	check()
	if !reflect.DeepEqual(cache.current[job.ID].source.Logs, job.Logs) || len(cache.current[job.ID].source.LogEntries) != 100 {
		t.Fatal("discarded history retained by the source snapshot")
	}
	cache = cache.begin(0)
	cache.previous = nil
	if len(cache.current) != 0 {
		t.Fatal("removed account retained its text projection")
	}
}

func TestMetadataLogProjectionOverlappingSecretsMatchPublicAndEvents(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		job := &Job{ID: "overlapping", Email: "account@example.test", Input: map[string]any{}, Logs: []string{"short-value-long-tail short-value"}, LogEntries: []LogEntry{{Message: "short-value-long-tail short-value"}}}
		if reverse {
			job.Input["password"], job.Input["totpSecret"] = "short-value", "short-value-long-tail"
		} else {
			job.Input["totpSecret"], job.Input["password"] = "short-value-long-tail", "short-value"
		}
		want := logging.Redacted + " " + logging.Redacted
		var cache metadataProjectionCache
		for i := 0; i < 4; i++ {
			// Reusing the exact same secret set must have the same result as a
			// fresh public/event projection, independent of input-map iteration.
			job.UpdatedAt = time.Now().UTC()
			cache = cache.begin(1)
			row := cache.job(job)
			cache.previous = nil
			public := SanitizeJobForPublic(job)
			event := sanitizeEventForJob(JobEvent{Prompt: job.Logs[0]}, job)
			if row["logs"].([]string)[0] != want || row["log_entries"].([]LogEntry)[0].Message != want || public.Logs[0] != want || event.Prompt != want {
				t.Fatal("overlapping secret left a suffix or differed across projections")
			}
		}
	}
}

func TestMetadataProjectionNeverReplacesCommitOrRollback(t *testing.T) {
	for _, failure := range []error{os.ErrPermission, store.ErrDurabilityUncertain} {
		t.Run(failure.Error(), func(t *testing.T) {
			st, err := store.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			m := NewManager(t.Context(), 1, st, nil)
			defer m.cancel()
			job := &Job{ID: "commit-fixture", Email: "commit@example.test", Status: StatusImported, CreatedAt: time.Now().UTC(), Prompt: "before"}
			m.jobs[job.ID] = job
			m.persistLocked()
			if m.lastPersistError != nil {
				t.Fatal(m.lastPersistError)
			}
			before := cloneJob(job)
			job.Prompt = "after"
			writes := 0
			m.metadataWriter = func(ctx context.Context, name string, value any) error {
				writes++
				if errors.Is(failure, store.ErrDurabilityUncertain) {
					if err := st.WriteJSON(ctx, name, value); err != nil {
						return err
					}
				}
				return failure
			}
			m.persistLocked()
			if !errors.Is(m.lastPersistError, failure) || writes != 1 {
				t.Fatal("cache suppressed or changed failed commit")
			}
			if !errors.Is(failure, store.ErrDurabilityUncertain) {
				*job = *before
			}
			m.metadataWriter = func(ctx context.Context, name string, value any) error {
				writes++
				return st.WriteJSON(ctx, name, value)
			}
			m.persistLocked()
			m.persistLocked()
			if m.lastPersistError != nil || writes != 3 {
				t.Fatal("unchanged projection skipped durable confirmation")
			}
			var document struct {
				Jobs []json.RawMessage `json:"jobs"`
			}
			if err := st.ReadJSON(t.Context(), "job-meta.json", &document); err != nil {
				t.Fatal(err)
			}
			restored, ok := decodeLegacyJob(document.Jobs[0], job.ID)
			if !ok || restored.Prompt != job.Prompt || !reflect.DeepEqual(restored.TOTPIntentReceipts, job.TOTPIntentReceipts) {
				t.Fatal("cached candidate overwrote rollback/uncertain retained state")
			}
		})
	}
}
