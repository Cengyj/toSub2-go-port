package accountquality

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"tosub2-go-port/internal/modelprobe"
)

func historyFor(t *testing.T, service *Service) ProbeHistory {
	t.Helper()
	rows, _ := service.Snapshots([]string{"a"})
	return rows["a"].ProbeHistory
}

func runHistoryCandy(t *testing.T, service *Service) {
	t.Helper()
	if receipt, err := service.Refresh(t.Context(), []string{"a"}, false); err != nil || len(receipt.Accepted) != 1 {
		t.Fatal("probe not accepted", err, receipt)
	}
	idle(t, service)
}

func TestProbeHistoryKeepsLastFiveCompletedAndRestores(t *testing.T) {
	s, f, root := newFixture(t)
	if len(historyFor(t, s)) != 0 {
		t.Fatal("unknown account acquired invented history")
	}
	var actualIDs []string
	for i := 0; i < 7; i++ {
		f.mu.Lock()
		score := i % 2
		f.probe = func(_ context.Context, options modelprobe.Options) (modelprobe.Result, error) {
			return resultFor(options, score), nil
		}
		if i == 4 {
			f.probe = func(context.Context, modelprobe.Options) (modelprobe.Result, error) {
				return modelprobe.Result{}, &modelprobe.Error{Code: "forbidden", HTTPStatus: 403}
			}
		}
		f.mu.Unlock()
		runHistoryCandy(t, s)
		history := historyFor(t, s)
		actualIDs = append(actualIDs, history[len(history)-1].ID)
	}
	history := historyFor(t, s)
	if len(history) != 5 {
		t.Fatalf("history not bounded: %+v", history)
	}
	for i, entry := range history {
		if entry.ID != actualIDs[i+2] || entry.Kind != "candy" || i > 0 && entry.FinishedAt.Before(history[i-1].FinishedAt) {
			t.Fatal("history order is not actual completion order", history)
		}
		if entry.Status == "failed" && (entry.Score != nil || entry.Total != 0 || entry.Verdict != "" || entry.HTTPStatus != 403) {
			t.Fatal("failed probe acquired a fake score", entry)
		}
	}
	n, err := New(root, fixtureProvider(f))
	if err != nil {
		t.Fatal(err)
	}
	if got := historyFor(t, n); !reflect.DeepEqual(history, got) {
		t.Fatalf("restart changed probe history: %+v", got)
	}
	for _, entry := range history {
		if entry.Score != nil {
			*entry.Score = 99
		}
	}
	for _, entry := range historyFor(t, s) {
		if entry.Score != nil && *entry.Score == 99 {
			t.Fatal("history aliases service memory")
		}
	}
}

func TestProbeHistoryIgnoresQueuedRunningCanceledAndAdmissionFailures(t *testing.T) {
	s, f, _ := newFixture(t)
	s.runGate <- struct{}{}
	if _, err := s.Refresh(t.Context(), []string{"a"}, false); err != nil {
		t.Fatal(err)
	}
	if len(historyFor(t, s)) != 0 {
		t.Fatal("queued probe created history")
	}
	if _, err := s.Cancel(t.Context(), []string{"a"}); err != nil {
		t.Fatal(err)
	}
	<-s.runGate
	idle(t, s)
	if len(historyFor(t, s)) != 0 {
		t.Fatal("queued cancellation created history")
	}
	entered := make(chan struct{})
	f.mu.Lock()
	f.probe = func(ctx context.Context, _ modelprobe.Options) (modelprobe.Result, error) {
		close(entered)
		<-ctx.Done()
		return modelprobe.Result{}, ctx.Err()
	}
	f.mu.Unlock()
	if _, err := s.Refresh(t.Context(), []string{"a"}, false); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("probe not started")
	}
	if len(historyFor(t, s)) != 0 {
		t.Fatal("running probe created history")
	}
	_, _ = s.Cancel(t.Context(), []string{"a"})
	idle(t, s)
	if len(historyFor(t, s)) != 0 {
		t.Fatal("unfinished cancellation created history")
	}
	f.mu.Lock()
	f.probe = func(context.Context, modelprobe.Options) (modelprobe.Result, error) {
		return modelprobe.Result{}, &modelprobe.Error{Code: "invalid_credentials"}
	}
	f.mu.Unlock()
	runHistoryCandy(t, s)
	if len(historyFor(t, s)) != 0 {
		t.Fatal("preflight rejection created history")
	}
}

func TestProbeHistoryManualAppendIsScoreIndependentAndBounded(t *testing.T) {
	s, f, root := newFixture(t)
	runHistoryCandy(t, s)
	s.mu.Lock()
	before := s.rows["a"]
	before.Snapshot = cloneSnapshot(before.Snapshot)
	s.mu.Unlock()
	for i := 0; i < 6; i++ {
		score := 99
		item := ProbeHistoryEntry{ID: fmt.Sprintf("pelican-test-%d", i), Kind: "pelican", Status: "failed", Score: &score, Total: 100, Verdict: "normal", FinishedAt: time.Now().UTC(), ErrorCode: "forbidden", HTTPStatus: 403, Model: "synthetic-model"}
		if err := s.RecordProbeHistory(t.Context(), "a", "r1", func() error { return nil }, item); err != nil {
			t.Fatal(err)
		}
	}
	got := historyFor(t, s)
	if len(got) != 5 || got[0].ID != "pelican-test-1" || got[4].ID != "pelican-test-5" {
		t.Fatal("manual samples lost newest-five order", got)
	}
	for _, item := range got {
		if item.Score != nil || item.Total != 0 || item.Verdict != "" {
			t.Fatal("manual failure affected score")
		}
	}
	s.mu.Lock()
	after := s.rows["a"]
	after.Snapshot = cloneSnapshot(after.Snapshot)
	s.mu.Unlock()
	before.Snapshot.ProbeHistory, after.Snapshot.ProbeHistory = nil, nil
	if !reflect.DeepEqual(before, after) {
		t.Fatal("manual history changed scoring/cooldown/authorization evidence")
	}
	n, err := New(root, fixtureProvider(f))
	if err != nil || !reflect.DeepEqual(got, historyFor(t, n)) {
		t.Fatal("manual history did not persist", err)
	}
}

func TestProbeHistoryRejectsLateIdentityAndPreservesOnWriteFailure(t *testing.T) {
	for _, scenario := range []string{"revision", "deleted", "grant", "write"} {
		t.Run(scenario, func(t *testing.T) {
			s, f, root := newFixture(t)
			runHistoryCandy(t, s)
			before := historyFor(t, s)
			path := filepath.Join(root, directoryName, snapshotFile("a"))
			beforeFile, _ := os.ReadFile(path)
			validate := func() error { return nil }
			f.mu.Lock()
			switch scenario {
			case "revision":
				f.accounts[0].Revision = "r2"
			case "deleted":
				f.accounts = nil
			case "grant":
				validate = func() error { return ErrSuperseded }
			case "write":
				s.persist = func(context.Context, string, any) error { return errors.New("synthetic write failure") }
			}
			f.mu.Unlock()
			err := s.RecordProbeHistory(t.Context(), "a", "r1", validate, ProbeHistoryEntry{ID: "pelican-late", Kind: "pelican", Status: "success", FinishedAt: time.Now().UTC()})
			if err == nil {
				t.Fatal("invalid history commit succeeded")
			}
			if !reflect.DeepEqual(before, historyFor(t, s)) {
				t.Fatal("late or failed write replaced existing history")
			}
			afterFile, _ := os.ReadFile(path)
			if string(beforeFile) != string(afterFile) {
				t.Fatal("rejected history modified durable evidence")
			}
		})
	}
}

func TestProbeHistoryCandyWriteFailureKeepsPriorHistory(t *testing.T) {
	s, _, _ := newFixture(t)
	runHistoryCandy(t, s)
	before := historyFor(t, s)
	write := s.persist
	s.persist = func(ctx context.Context, name string, value any) error {
		if row, ok := value.(storedRow); ok && row.Snapshot.LastAttempt.Status == "success" {
			return errors.New("synthetic terminal write failure")
		}
		return write(ctx, name, value)
	}
	runHistoryCandy(t, s)
	if !reflect.DeepEqual(before, historyFor(t, s)) {
		t.Fatal("failed terminal save discarded older history")
	}
}

func TestProbeHistoryPostWriteSupersessionRollsBackBeforeRestart(t *testing.T) {
	for _, scenario := range []string{"revision", "deleted", "no-prior-row", "rollback-fails"} {
		t.Run(scenario, func(t *testing.T) {
			s, f, root := newFixture(t)
			if scenario != "no-prior-row" {
				runHistoryCandy(t, s)
			}
			before := historyFor(t, s)
			write := s.persist
			writes := 0
			s.persist = func(ctx context.Context, name string, value any) error {
				writes++
				if writes == 2 && scenario == "rollback-fails" {
					return errors.New("synthetic rollback failure")
				}
				if err := write(ctx, name, value); err != nil {
					return err
				}
				if writes == 1 {
					f.mu.Lock()
					if scenario == "deleted" {
						f.accounts = nil
					} else {
						f.accounts[0].Revision = "r2"
					}
					f.mu.Unlock()
				}
				return nil
			}
			err := s.RecordProbeHistory(t.Context(), "a", "r1", func() error { return nil }, ProbeHistoryEntry{ID: "pelican-raced", Kind: "pelican", Status: "success", FinishedAt: time.Now().UTC()})
			if scenario == "rollback-fails" {
				if !errors.Is(err, ErrCachePersistence) || !reflect.DeepEqual(before, historyFor(t, s)) {
					t.Fatal("failed rollback claimed success or replaced prior in-memory history", err)
				}
				return // Durability cannot be claimed when the compensating write fails.
			}
			if !errors.Is(err, ErrSuperseded) {
				t.Fatal("raced authority change accepted", err)
			}
			n, loadErr := New(root, fixtureProvider(f))
			if loadErr != nil || !reflect.DeepEqual(before, historyFor(t, n)) {
				t.Fatal("superseded candidate survived restart", loadErr, historyFor(t, n))
			}
			if scenario == "no-prior-row" {
				if _, statErr := os.Stat(filepath.Join(root, directoryName, snapshotFile("a"))); !errors.Is(statErr, os.ErrNotExist) {
					t.Fatal("rollback invented an original row", statErr)
				}
			}
		})
	}
}

func TestProbeHistoryDeduplicatesNewestByKindAndID(t *testing.T) {
	now := time.Now().UTC()
	newer := ProbeHistoryEntry{ID: "same-id", Kind: "pelican", Status: "success", FinishedAt: now, Model: "newer-model"}
	older := newer
	older.FinishedAt, older.Model = now.Add(-time.Minute), "older-model"
	score := 1
	candy := ProbeHistoryEntry{ID: "same-id", Kind: "candy", Status: "success", FinishedAt: now, Score: &score, Total: 1, Verdict: "normal"}
	got := normalizeProbeHistory(ProbeHistory{newer, older, candy})
	if len(got) != 2 || got[0].Model != "newer-model" || got[1].Kind != "candy" {
		t.Fatal("older duplicate or another kind replaced latest sample", got)
	}
	bad := newer
	bad.HTTPStatus = 503
	if len(normalizeProbeHistory(ProbeHistory{bad})) != 0 {
		t.Fatal("success accepted an HTTP failure")
	}
}

func TestProbeHistoryToleratesMalformedOptionalDataAndBackfillsOnlyEvidence(t *testing.T) {
	for _, raw := range []string{`"bad"`, `[{"finishedAt":"bad"}]`, `[{"kind":"PRIVATE-CANARY"}]`} {
		var snapshot Snapshot
		if err := json.Unmarshal([]byte(`{"id":"a","lastAttempt":{"status":"never"},"probeHistory":`+raw+`}`), &snapshot); err != nil || len(snapshot.ProbeHistory) != 0 || snapshot.ID != "a" {
			t.Fatal("optional history invalidated parent snapshot", err)
		}
	}
	s, f, root := newFixture(t)
	runHistoryCandy(t, s)
	s.mu.Lock()
	row := s.rows["a"]
	row.Snapshot.ProbeHistory = nil
	s.mu.Unlock()
	if err := s.disk.WriteJSON(t.Context(), snapshotFile("a"), row); err != nil {
		t.Fatal(err)
	}
	n, err := New(root, fixtureProvider(f))
	if err != nil || len(historyFor(t, n)) != 1 {
		t.Fatal("confirmed legacy completion was not backfilled exactly once", err)
	}
	row.Snapshot.LastAttempt.Status = "running"
	if err := s.disk.WriteJSON(t.Context(), snapshotFile("a"), row); err != nil {
		t.Fatal(err)
	}
	n, err = New(root, fixtureProvider(f))
	if err != nil || len(historyFor(t, n)) != 0 {
		t.Fatal("legacy unfinished probe became history", err)
	}
	data, _ := json.Marshal(historyFor(t, n))
	if strings.Contains(string(data), "PRIVATE-CANARY") {
		t.Fatal("history exposed untrusted text")
	}
}
