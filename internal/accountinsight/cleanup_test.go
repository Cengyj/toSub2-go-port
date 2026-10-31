package accountinsight

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"tosub2-go-port/internal/protocol"
)

func cleanupSnapshot(id string) storedSnapshot {
	return storedSnapshot{SchemaVersion: 1, AccountRevision: "synthetic-v1", Snapshot: Snapshot{ID: id, AuthorizationMethod: "chatgpt", LastAttempt: Attempt{Status: "success"}, InsightResult: protocol.InsightResult{Usage: &protocol.InsightUsage{Source: "synthetic", ObservedAt: time.Now().UTC()}}}}
}
func writeCleanupSnapshot(t *testing.T, s *Service, id string) {
	t.Helper()
	if err := s.disk.WriteJSON(t.Context(), snapshotFile(id), cleanupSnapshot(id)); err != nil {
		t.Fatal(err)
	}
}
func newCleanupService(t *testing.T, p Provider) *Service {
	t.Helper()
	if p.Acquire == nil {
		p.Acquire = func(context.Context, string) (*Lease, error) { return nil, errors.New("synthetic unavailable") }
	}
	s, err := New(t.TempDir(), p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := s.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	return s
}

func TestPruneDeletedPreservesCurrentAccountsSettingsAndUnknownFiles(t *testing.T) {
	live := Account{ID: "live", AuthorizationMethod: "chatgpt", Revision: "synthetic-v1", CanQuery: true}
	s := newCleanupService(t, Provider{Accounts: func() []Account { return []Account{live} }})
	writeCleanupSnapshot(t, s, "live")
	writeCleanupSnapshot(t, s, "deleted")
	if err := s.disk.WriteJSON(t.Context(), "settings.json", DefaultSettings()); err != nil {
		t.Fatal(err)
	}
	if err := s.disk.WriteJSON(t.Context(), "notes.json", map[string]any{"keep": "synthetic private note"}); err != nil {
		t.Fatal(err)
	}
	future := cleanupSnapshot("future")
	future.SchemaVersion = 2
	if err := s.disk.WriteJSON(t.Context(), snapshotFile("future"), future); err != nil {
		t.Fatal(err)
	}
	if err := s.disk.WriteJSON(t.Context(), snapshotFile("wrong-name"), cleanupSnapshot("other-id")); err != nil {
		t.Fatal(err)
	}
	if err := s.disk.WriteBytes(t.Context(), snapshotFile("malformed"), []byte("{invalid")); err != nil {
		t.Fatal(err)
	}
	dirName := snapshotFile("directory")
	if err := os.Mkdir(s.disk.Path(dirName), 0700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "private-target.json")
	if err := protocol.WritePrivateJSON(outside, cleanupSnapshot("linked")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, s.disk.Path(snapshotFile("linked"))); err != nil {
		t.Fatal(err)
	}
	files := []string{"settings.json", "notes.json", snapshotFile("live"), snapshotFile("future"), snapshotFile("wrong-name"), snapshotFile("malformed")}
	before := make(map[string][]byte, len(files))
	for _, name := range files {
		var err error
		before[name], err = os.ReadFile(s.disk.Path(name))
		if err != nil {
			t.Fatal(err)
		}
	}
	s.mu.Lock()
	s.enqueueLocked(live, "live-query", false)
	s.enqueueLocked(Account{ID: "deleted"}, "deleted-query", false)
	s.mu.Unlock()
	if err := s.PruneDeleted(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.disk.Path(snapshotFile("deleted"))); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("deleted snapshot retained", err)
	}
	for _, name := range files {
		after, err := os.ReadFile(s.disk.Path(name))
		if err != nil || !bytes.Equal(before[name], after) {
			t.Fatal("unrelated file changed", name, err)
		}
	}
	if st, err := os.Stat(s.disk.Path(dirName)); err != nil || !st.IsDir() {
		t.Fatal("unknown directory removed", err)
	}
	if st, err := os.Lstat(s.disk.Path(snapshotFile("linked"))); err != nil || st.Mode()&os.ModeSymlink == 0 {
		t.Fatal("symlink changed", err)
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatal("symlink target removed", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.snapshots["deleted"].Snapshot.ID != "" || s.flights["deleted"] != nil || len(s.pending) != 1 || s.pending[0].id != "live" || s.flights["live"] == nil {
		t.Fatal("pruning changed current work or retained deleted work")
	}
}

func TestPruneDeletedReportsRemovalFailureAndRetries(t *testing.T) {
	s := newCleanupService(t, Provider{Accounts: func() []Account { return nil }})
	const previousWarning = "existing configuration read failure"
	s.lastError = previousWarning
	writeCleanupSnapshot(t, s, "failed")
	writeCleanupSnapshot(t, s, "removed")
	err := s.pruneDeleted(t.Context(), func(path string) error {
		if filepath.Base(path) == snapshotFile("failed") {
			return errors.New("synthetic-private-path-and-error")
		}
		return os.Remove(path)
	})
	if !errors.Is(err, ErrCacheCleanup) || strings.Contains(err.Error(), "synthetic-private") || !strings.Contains(s.Status().LastError, previousWarning) || !strings.Contains(s.Status().LastError, ErrCacheCleanup.Error()) {
		t.Fatal("cleanup failure was hidden or leaked", err)
	}
	if _, err := os.Stat(s.disk.Path(snapshotFile("failed"))); err != nil {
		t.Fatal("failed removal lost its retry artifact", err)
	}
	if _, err := os.Stat(s.disk.Path(snapshotFile("removed"))); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("one failure prevented independent cleanup", err)
	}
	if err := s.PruneDeleted(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.disk.Path(snapshotFile("failed"))); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("retry did not remove cache", err)
	}
	if s.Status().LastError != previousWarning {
		t.Fatal("successful retry retained its cleanup warning or erased another warning")
	}
}

func TestPruneDeletedRetriesOrphanCleanupOnStartup(t *testing.T) {
	live := Account{ID: "live", AuthorizationMethod: "chatgpt", Revision: "synthetic-v1", CanQuery: true}
	p := Provider{Accounts: func() []Account { return []Account{live} }, Acquire: func(context.Context, string) (*Lease, error) { return nil, errors.New("synthetic unavailable") }}
	s := newCleanupService(t, p)
	writeCleanupSnapshot(t, s, "live")
	writeCleanupSnapshot(t, s, "deleted-before-restart")
	if err := s.disk.WriteJSON(t.Context(), "settings.json", DefaultSettings()); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	restored, err := New(filepath.Dir(s.disk.Root), p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restored.Close(context.Background()) })
	if _, err := os.Stat(restored.disk.Path(snapshotFile("deleted-before-restart"))); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("startup retained orphaned cache", err)
	}
	if row, ok := restored.Snapshot("live"); !ok || row.ID != "live" || row.Usage == nil || !row.Stale {
		t.Fatal("startup cleanup removed a live historical observation")
	}
	if _, err := os.Stat(restored.disk.Path("settings.json")); err != nil {
		t.Fatal("startup cleanup removed settings", err)
	}
}

func TestPruneDeletedCancellationRetainsFilesAndRejectsReplacedRoot(t *testing.T) {
	s := newCleanupService(t, Provider{Accounts: func() []Account { return nil }})
	writeCleanupSnapshot(t, s, "deleted")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := s.PruneDeleted(ctx); !errors.Is(err, context.Canceled) || !errors.Is(err, ErrCacheCleanup) {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.disk.Path(snapshotFile("deleted"))); err != nil {
		t.Fatal("cancelled cleanup removed data", err)
	}
	moved := s.disk.Root + "-retained"
	if err := os.Rename(s.disk.Root, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(moved, s.disk.Root); err != nil {
		t.Fatal(err)
	}
	if err := s.PruneDeleted(t.Context()); !errors.Is(err, ErrCacheCleanup) {
		t.Fatal("replaced root was followed", err)
	}
	if _, err := os.Stat(filepath.Join(moved, snapshotFile("deleted"))); err != nil {
		t.Fatal("replaced root target was cleaned", err)
	}
}

func TestPruneDeletedCancelsWithoutReleasingActiveLeaseOrAcceptingLateWrite(t *testing.T) {
	var mu sync.Mutex
	exists := true
	entered, finish, released := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var finishOnce sync.Once
	p := Provider{Accounts: func() []Account {
		mu.Lock()
		defer mu.Unlock()
		if !exists {
			return nil
		}
		return []Account{{ID: "deleted", AuthorizationMethod: "chatgpt", Revision: "synthetic-v1", CanQuery: true}}
	}, Acquire: func(ctx context.Context, _ string) (*Lease, error) {
		return &Lease{Context: ctx, Validate: ctx.Err, Release: func() { close(released) }, Query: func(context.Context, protocol.InsightsOptions) protocol.InsightResult {
			close(entered)
			<-finish
			return protocol.InsightResult{Usage: &protocol.InsightUsage{Source: "synthetic", ObservedAt: time.Now().UTC()}}
		}}, nil
	}}
	s := newCleanupService(t, p)
	t.Cleanup(func() { finishOnce.Do(func() { close(finish) }) })
	s.tick = time.Millisecond
	s.Start(t.Context())
	enable(t, s, false)
	writeCleanupSnapshot(t, s, "deleted")
	if _, err := s.Refresh([]string{"deleted"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("query never entered")
	}
	mu.Lock()
	exists = false
	mu.Unlock()
	if err := s.PruneDeleted(t.Context()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-released:
		t.Fatal("cleanup released an active transport lease")
	default:
	}
	if s.Status().Active != 1 {
		t.Fatal("active transport ownership was lost")
	}
	finishOnce.Do(func() { close(finish) })
	select {
	case <-released:
	case <-time.After(time.Second):
		t.Fatal("late query did not release its own lease")
	}
	waitFor(t, func() bool { return s.Status().Active == 0 })
	if _, err := os.Stat(s.disk.Path(snapshotFile("deleted"))); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("late response recreated deleted cache", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.snapshots["deleted"]; exists || len(s.flights) != 0 || len(s.pending) != 0 {
		t.Fatal("late response resurrected deleted memory state")
	}
}

func TestPruneDeletedSerializesWithAnAlreadyStartedCommit(t *testing.T) {
	var mu sync.Mutex
	exists := true
	p := Provider{Accounts: func() []Account {
		mu.Lock()
		defer mu.Unlock()
		if !exists {
			return nil
		}
		return []Account{{ID: "deleted", AuthorizationMethod: "chatgpt", Revision: "synthetic-v1", CanQuery: true}}
	}, Acquire: func(ctx context.Context, _ string) (*Lease, error) {
		return &Lease{Context: ctx, Validate: ctx.Err, Release: func() {}, Query: func(context.Context, protocol.InsightsOptions) protocol.InsightResult {
			return protocol.InsightResult{Usage: &protocol.InsightUsage{Source: "synthetic", ObservedAt: time.Now().UTC()}}
		}}, nil
	}}
	s := newCleanupService(t, p)
	s.tick = time.Millisecond
	s.Start(t.Context())
	enable(t, s, false)
	entered, allow := make(chan struct{}), make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(allow) }) })
	s.persist = func(ctx context.Context, name string, v any) error {
		close(entered)
		<-allow
		return s.disk.WriteJSON(ctx, name, v)
	}
	if _, err := s.Refresh([]string{"deleted"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("query never reached commit")
	}
	mu.Lock()
	exists = false
	mu.Unlock()
	done := make(chan error, 1)
	go func() { done <- s.PruneDeleted(t.Context()) }()
	select {
	case err := <-done:
		t.Fatal("cleanup passed an in-progress disk commit", err)
	default:
	}
	once.Do(func() { close(allow) })
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("cleanup failed to finish after commit")
	}
	waitFor(t, func() bool { return s.Status().Active == 0 })
	if _, err := os.Stat(s.disk.Path(snapshotFile("deleted"))); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("serialized cleanup left an old committed cache", err)
	}
}
