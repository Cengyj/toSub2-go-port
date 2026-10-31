package accountinsight

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
	"tosub2-go-port/internal/protocol"
)

func TestInsightLeaseCancelledWhileWaitingForCommit(t *testing.T) {
	s, f := newFixture(t)
	entered := make(chan struct{})
	var once sync.Once
	leaseCtx, cancelLease := context.WithCancel(context.Background())
	defer cancelLease()
	s.provider.Acquire = func(context.Context, string) (*Lease, error) {
		return &Lease{Context: leaseCtx, Release: func() {}, Validate: func() error { once.Do(func() { close(entered) }); return leaseCtx.Err() }, Query: func(ctx context.Context, _ protocol.InsightsOptions) protocol.InsightResult { return f.query(ctx) }}, nil
	}
	enable(t, s, false)
	s.writeMu.Lock()
	_, err := s.Refresh([]string{"synthetic"})
	if err != nil {
		s.writeMu.Unlock()
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		s.writeMu.Unlock()
		t.Fatal("query did not reach commit")
	}
	cancelLease()
	s.writeMu.Unlock()
	got := waitResult(t, s, "cancelled")
	if got.Usage != nil {
		t.Fatal("cancelled lease published a new observation")
	}
	if _, err := os.Stat(s.disk.Path(snapshotFile("synthetic"))); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("cancelled lease persisted", err)
	}
}
func TestInsightCredentialChangeDuringPersistenceDoesNotPublish(t *testing.T) {
	s, f := newFixture(t)
	leaseCtx, cancelLease := context.WithCancel(context.Background())
	defer cancelLease()
	s.provider.Acquire = func(context.Context, string) (*Lease, error) {
		return &Lease{Context: leaseCtx, Release: func() {}, Validate: leaseCtx.Err, Query: func(ctx context.Context, _ protocol.InsightsOptions) protocol.InsightResult { return f.query(ctx) }}, nil
	}
	enable(t, s, false)
	base := s.persist
	s.persist = func(ctx context.Context, name string, v any) error {
		err := base(ctx, name, v)
		if name == snapshotFile("synthetic") {
			f.mu.Lock()
			f.account.Revision = "two"
			f.mu.Unlock()
			cancelLease()
		}
		return err
	}
	s.Refresh([]string{"synthetic"})
	waitFor(t, func() bool { return s.Status().Active == 0 && s.Status().Queued == 0 })
	got, _ := s.Snapshot("synthetic")
	if got.Usage != nil || got.LastAttempt.Status != "never" {
		t.Fatal("late result published", got)
	}
	restored, err := New(filepath.Dir(s.disk.Root), f.provider())
	if err != nil {
		t.Fatal(err)
	}
	got, _ = restored.Snapshot("synthetic")
	if got.Usage != nil {
		t.Fatal("cache reused for changed credentials")
	}
}
func TestInsightPrivateCacheRejectsSymlinkAndBroadPermissions(t *testing.T) {
	for _, kind := range []string{"symlink", "permissions", "trailing", "oversize"} {
		t.Run(kind, func(t *testing.T) {
			s, f := newFixture(t)
			path := s.disk.Path("settings.json")
			switch kind {
			case "symlink":
				target := filepath.Join(t.TempDir(), "foreign.json")
				if err := os.WriteFile(target, []byte("{}"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, path); err != nil {
					if runtime.GOOS == "windows" {
						t.Skip("symlink creation needs host support")
					}
					t.Fatal(err)
				}
			case "permissions":
				if runtime.GOOS == "windows" {
					t.Skip("Windows ACL behavior is covered by privatefile platform tests")
				}
				if err := os.WriteFile(path, []byte("{}"), 0644); err != nil {
					t.Fatal(err)
				}
			case "trailing":
				if err := os.WriteFile(path, []byte("{} {}"), 0600); err != nil {
					t.Fatal(err)
				}
			case "oversize":
				file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0600)
				if err != nil {
					t.Fatal(err)
				}
				err = file.Truncate((4 << 20) + 1)
				file.Close()
				if err != nil {
					t.Fatal(err)
				}
			}
			restored, err := New(filepath.Dir(s.disk.Root), f.provider())
			if err != nil {
				t.Fatal(err)
			}
			if restored.Settings().Enabled || restored.Settings().PersistenceWarning == "" {
				t.Fatal("unsafe settings accepted")
			}
		})
	}
}

func TestInsightBudgetTimeoutIsNotReportedAsUserCancellation(t *testing.T) {
	s, f := newFixture(t)
	s.budget = 15 * time.Millisecond
	f.query = func(ctx context.Context) protocol.InsightResult { <-ctx.Done(); return protocol.InsightResult{} }
	enable(t, s, false)
	s.Refresh([]string{"synthetic"})
	got := waitResult(t, s, "failed")
	if got.LastAttempt.ErrorCode != "query_timeout" || got.Usage != nil || got.Persisted {
		t.Fatal("timeout was misclassified or persisted an unvalidated result", got)
	}
}

func TestInsightCacheRestoreFailureIsVisibleAndPreservesOriginal(t *testing.T) {
	for _, content := range []string{
		`{"synthetic-private-value":`,
		`{"SchemaVersion":2,"AccountRevision":"one","Snapshot":{"id":"synthetic"}}`,
		`{"SchemaVersion":1,"AccountRevision":"one","Snapshot":{"id":"different-account"}}`,
	} {
		s, f := newFixture(t)
		path := s.disk.Path(snapshotFile("synthetic"))
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		restored, err := New(filepath.Dir(s.disk.Root), f.provider())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = restored.Close(context.Background()) })
		got, ok := restored.Snapshot("synthetic")
		if !ok || got.LastAttempt.Status != "failed" || got.LastAttempt.ErrorCode != "cache_unavailable" || got.LastAttempt.Message == "" || got.Usage != nil || got.LastSuccessAt != nil || got.Persisted || !got.Stale {
			t.Fatalf("cache restore failure was hidden or presented as valid data: %+v", got)
		}
		unchanged, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(unchanged, []byte(content)) {
			t.Fatal("failed cache restore replaced the recovery evidence", err)
		}
		if f.calls.Load() != 0 {
			t.Fatal("cache restore performed an upstream query")
		}
	}
}

func TestInsightMissingCacheRemainsNeverQueried(t *testing.T) {
	s, _ := newFixture(t)
	got, ok := s.Snapshot("synthetic")
	if !ok || got.LastAttempt.Status != "never" || got.LastAttempt.ErrorCode != "" {
		t.Fatal("new account with no cache was reported as a restore failure", got)
	}
}

func TestInsightCacheRestoreFailureRecoversAfterExplicitQuery(t *testing.T) {
	s, f := newFixture(t)
	if err := os.WriteFile(s.disk.Path(snapshotFile("synthetic")), []byte("invalid synthetic cache"), 0600); err != nil {
		t.Fatal(err)
	}
	restored, err := New(filepath.Dir(s.disk.Root), f.provider())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restored.Close(context.Background()) })
	restored.Start(t.Context())
	enable(t, restored, false)
	receipt, err := restored.Refresh([]string{"synthetic"})
	if err != nil || len(receipt.Accepted) != 1 {
		t.Fatal("cache failure blocked a fresh read", receipt, err)
	}
	got := waitResult(t, restored, "success")
	if got.LastAttempt.ErrorCode != "" || got.Usage == nil || got.LastSuccessAt == nil || !got.Persisted || got.Stale || f.calls.Load() != 1 {
		t.Fatal("fresh query did not replace the failed cache state", got)
	}
	var saved storedSnapshot
	if err := restored.read(snapshotFile("synthetic"), &saved); err != nil || saved.Snapshot.Usage == nil || saved.Snapshot.LastAttempt.Status != "success" {
		t.Fatal("fresh result was not safely saved", err)
	}
}
