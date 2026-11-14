package accountinsight

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"tosub2-go-port/internal/protocol"
)

func TestTrashSnapshotsRetainedAcrossPruneAndStartupButNeverQueryable(t *testing.T) {
	var restored atomic.Bool
	live := Account{ID: "live", AuthorizationMethod: "chatgpt", Revision: "synthetic-v1", CanQuery: true}
	trash := Account{ID: "trash", AuthorizationMethod: "chatgpt", Revision: "synthetic-v1", CanQuery: true}
	var acquisitions atomic.Int32
	p := Provider{Accounts: func() []Account {
		if restored.Load() {
			return []Account{live, trash}
		}
		return []Account{live}
	}, RetainedIDs: func() []string { return []string{"trash"} }, Acquire: func(context.Context, string) (*Lease, error) {
		acquisitions.Add(1)
		return nil, errors.New("synthetic unavailable")
	}}
	s := newCleanupService(t, p)
	for _, id := range []string{"live", "trash", "orphan"} {
		writeCleanupSnapshot(t, s, id)
	}
	before, err := os.ReadFile(s.disk.Path(snapshotFile("trash")))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.PruneDeleted(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.disk.Path(snapshotFile("orphan"))); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("orphan cache retained", err)
	}
	if err := s.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	restarted, err := New(filepath.Dir(s.disk.Root), p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restarted.Close(context.Background()) })
	after, err := os.ReadFile(restarted.disk.Path(snapshotFile("trash")))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("trash history changed during startup cleanup", err)
	}
	if _, ok := restarted.Snapshot("trash"); ok {
		t.Fatal("trash appeared in active snapshot API")
	}
	restarted.Start(t.Context())
	enable(t, restarted, false)
	receipt, err := restarted.Refresh([]string{"trash"})
	if err != nil || len(receipt.Accepted) != 0 || len(receipt.Skipped) != 1 {
		t.Fatal("trash admitted for manual query", receipt, err)
	}
	if got := restarted.Status(); got.Refresh.Total != 1 {
		t.Fatal("trash counted as active query inventory", got.Refresh)
	}
	restored.Store(true)
	if got, ok := restarted.Snapshot("trash"); !ok || got.Usage == nil || got.Usage.Source != "synthetic" || !got.Stale {
		t.Fatal("restored account lost its retained history", got)
	}
	if acquisitions.Load() != 0 {
		t.Fatal("cache retention performed an upstream query")
	}
}

func TestTrashCancellationRetainsHistoryAndPurgeWaitsForLeaseRelease(t *testing.T) {
	var active atomic.Bool
	active.Store(true)
	entered, finish, released := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var finishOnce sync.Once
	p := Provider{Accounts: func() []Account {
		if active.Load() {
			return []Account{{ID: "trash", AuthorizationMethod: "chatgpt", Revision: "synthetic-v1", CanQuery: true}}
		}
		return nil
	}, RetainedIDs: func() []string { return []string{"trash"} }, Acquire: func(ctx context.Context, _ string) (*Lease, error) {
		return &Lease{Context: ctx, Validate: ctx.Err, Release: func() { close(released) }, Query: func(context.Context, protocol.InsightsOptions) protocol.InsightResult {
			close(entered)
			<-finish
			return protocol.InsightResult{Usage: &protocol.InsightUsage{Source: "late-response", ObservedAt: time.Now().UTC()}}
		}}, nil
	}}
	s := newCleanupService(t, p)
	t.Cleanup(func() { finishOnce.Do(func() { close(finish) }) })
	writeCleanupSnapshot(t, s, "trash")
	s.snapshots["trash"] = cleanupSnapshot("trash")
	before, err := os.ReadFile(s.disk.Path(snapshotFile("trash")))
	if err != nil {
		t.Fatal(err)
	}
	s.tick = time.Millisecond
	s.Start(t.Context())
	enable(t, s, false)
	if _, err := s.Refresh([]string{"trash"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("query did not start")
	}
	active.Store(false)
	if err := s.PruneDeleted(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := s.PurgeAccountIDs(t.Context(), []string{"trash"}); !errors.Is(err, ErrCacheBusy) {
		t.Fatal("purge passed a cancelled but unreleased lease", err)
	}
	select {
	case <-released:
		t.Fatal("cleanup released another worker's transport")
	default:
	}
	finishOnce.Do(func() { close(finish) })
	waitFor(t, func() bool { return s.Status().Active == 0 })
	after, err := os.ReadFile(s.disk.Path(snapshotFile("trash")))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("late response replaced retained history", err)
	}
	if _, ok := s.Snapshot("trash"); ok {
		t.Fatal("late response published trash as active")
	}
	s.mu.Lock()
	usage := s.snapshots["trash"].Snapshot.Usage
	flight := s.flights["trash"]
	s.mu.Unlock()
	if usage == nil || usage.Source != "synthetic" || flight != nil {
		t.Fatal("late response replaced memory history or retained ownership")
	}
	if err := s.PurgeAccountIDs(t.Context(), []string{"trash"}); err != nil {
		t.Fatal("purge failed after lease release", err)
	}
	if _, err := os.Stat(s.disk.Path(snapshotFile("trash"))); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("explicit purge retained trash cache", err)
	}
}

func TestPurgeAccountIDsProtectsActiveSelectionAndOnlyRemovesSelectedCache(t *testing.T) {
	s := newCleanupService(t, Provider{Accounts: func() []Account { return []Account{{ID: "live"}} }, RetainedIDs: func() []string { return []string{"trash", "unselected"} }})
	for _, id := range []string{"live", "trash", "unselected"} {
		writeCleanupSnapshot(t, s, id)
		s.snapshots[id] = cleanupSnapshot(id)
	}
	if err := s.PurgeAccountIDs(t.Context(), []string{"trash", "live"}); !errors.Is(err, ErrCacheBusy) {
		t.Fatal("active mixed selection accepted", err)
	}
	if _, err := os.Stat(s.disk.Path(snapshotFile("trash"))); err != nil {
		t.Fatal("mixed selection partially deleted trash", err)
	}
	if err := s.disk.WriteBytes(t.Context(), snapshotFile("trash"), []byte("malformed synthetic cache")); err != nil {
		t.Fatal(err)
	}
	if err := s.disk.WriteJSON(t.Context(), "settings.json", DefaultSettings()); err != nil {
		t.Fatal(err)
	}
	if err := s.PurgeAccountIDs(t.Context(), []string{"trash", "trash"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.disk.Path(snapshotFile("trash"))); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("target cache remains", err)
	}
	if _, exists := s.snapshots["trash"]; exists {
		t.Fatal("target memory cache remains")
	}
	for _, name := range []string{snapshotFile("live"), snapshotFile("unselected"), "settings.json"} {
		if _, err := os.Stat(s.disk.Path(name)); err != nil {
			t.Fatal("unrelated cache or settings changed", name, err)
		}
	}
	if err := s.PurgeAccountIDs(t.Context(), []string{"trash"}); err != nil {
		t.Fatal("idempotent purge failed", err)
	}
}

func TestPurgeAccountIDsFailureAndCancellationPreserveRetryState(t *testing.T) {
	s := newCleanupService(t, Provider{Accounts: func() []Account { return nil }, RetainedIDs: func() []string { return []string{"trash"} }})
	writeCleanupSnapshot(t, s, "trash")
	s.snapshots["trash"] = cleanupSnapshot("trash")
	err := s.purgeAccountIDs(t.Context(), []string{"trash"}, func(string) error { return errors.New("synthetic-private-error") })
	if !errors.Is(err, ErrCacheCleanup) || strings.Contains(err.Error(), "synthetic-private") {
		t.Fatal("removal failure hidden or leaked", err)
	}
	if _, err := os.Stat(s.disk.Path(snapshotFile("trash"))); err != nil || s.snapshots["trash"].Snapshot.ID != "trash" {
		t.Fatal("failed removal lost retry state", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := s.PurgeAccountIDs(ctx, []string{"trash"}); !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation ignored", err)
	}
	if _, err := os.Stat(s.disk.Path(snapshotFile("trash"))); err != nil {
		t.Fatal("cancelled purge removed cache", err)
	}
	if err := s.PurgeAccountIDs(t.Context(), []string{"trash"}); err != nil {
		t.Fatal("purge retry failed", err)
	}
}

func TestPurgeAccountIDsRejectsReplacedRootAndNonregularCache(t *testing.T) {
	for _, kind := range []string{"root", "directory", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			s := newCleanupService(t, Provider{Accounts: func() []Account { return nil }})
			path := s.disk.Path(snapshotFile("trash"))
			protected := path
			switch kind {
			case "root":
				writeCleanupSnapshot(t, s, "trash")
				moved := s.disk.Root + "-retained"
				if err := os.Rename(s.disk.Root, moved); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(moved, s.disk.Root); err != nil {
					t.Fatal(err)
				}
				protected = filepath.Join(moved, snapshotFile("trash"))
			case "directory":
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				protected = filepath.Join(t.TempDir(), "synthetic-private-target.json")
				if err := protocol.WritePrivateJSON(protected, cleanupSnapshot("trash")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(protected, path); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.PurgeAccountIDs(t.Context(), []string{"trash"}); !errors.Is(err, ErrCacheCleanup) {
				t.Fatal("unsafe target accepted", err)
			}
			if _, err := os.Stat(protected); err != nil {
				t.Fatal("unsafe target was removed", err)
			}
		})
	}
}
