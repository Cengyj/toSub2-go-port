package recyclebin

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/store"
)

type fakeRepository struct {
	mu      sync.Mutex
	entries []*jobs.TrashEntry
	calls   []string
	purge   func(context.Context, string) (int, error)
}

func (r *fakeRepository) ListTrash() []*jobs.TrashEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]*jobs.TrashEntry(nil), r.entries...)
}
func (r *fakeRepository) PurgeTrash(ctx context.Context, ids []string) (int, error) {
	r.mu.Lock()
	r.calls = append(r.calls, ids...)
	fn := r.purge
	r.mu.Unlock()
	if fn != nil {
		return fn(ctx, ids[0])
	}
	return len(ids), nil
}
func newService(t *testing.T, r *fakeRepository) *Service {
	t.Helper()
	s, err := New(t.TempDir(), r)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	return s
}
func TestRetentionDisabledByDefaultAndPersistedRevision(t *testing.T) {
	r := &fakeRepository{entries: []*jobs.TrashEntry{{ID: "old", State: "trashed", DeletedAt: time.Now().Add(-400 * 24 * time.Hour)}}}
	root := t.TempDir()
	s, err := New(root, r)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Settings(); got.RetentionDays != 0 || got.Revision != 1 {
		t.Fatalf("defaults=%+v", got)
	}
	if err = s.Sweep(t.Context(), time.Now()); err != nil || len(r.calls) != 0 {
		t.Fatal("default policy deleted history", err)
	}
	next, err := s.Update(t.Context(), Settings{SchemaVersion: 1, Revision: 1, RetentionDays: 30})
	if err != nil || next.Revision != 2 {
		t.Fatal(next, err)
	}
	if _, err = s.Update(t.Context(), Settings{SchemaVersion: 1, Revision: 1, RetentionDays: 0}); !errors.Is(err, ErrConflict) {
		t.Fatal("stale write accepted", err)
	}
	reopened, err := New(root, r)
	if err != nil || reopened.Settings().RetentionDays != 30 {
		t.Fatal("policy not durable", err)
	}
	if err = s.Sweep(t.Context(), time.Now()); err != nil || len(r.calls) != 1 {
		t.Fatal("existing expiry not applied", err)
	}
}
func TestRetentionBoundaryAndUTCDays(t *testing.T) {
	s := newService(t, &fakeRepository{})
	for _, n := range []int{-1, 3651} {
		if _, err := s.Update(t.Context(), Settings{SchemaVersion: 1, Revision: 1, RetentionDays: n}); err == nil {
			t.Fatal("invalid retention accepted", n)
		}
	}
	zone := time.FixedZone("test", -8*3600)
	deleted := time.Date(2026, 11, 1, 23, 30, 0, 0, zone)
	expiry := ExpiresAt(&jobs.TrashEntry{DeletedAt: deleted}, 1)
	if expiry == nil || expiry.Sub(deleted) != 24*time.Hour || expiry.Location() != time.UTC {
		t.Fatal("expiry is not elapsed UTC day", expiry)
	}
	if ExpiresAt(&jobs.TrashEntry{DeletedAt: deleted}, 0) != nil {
		t.Fatal("zero enabled deletion")
	}
	r := &fakeRepository{entries: []*jobs.TrashEntry{{ID: "boundary", State: "trashed", DeletedAt: deleted}}}
	s = newService(t, r)
	_, _ = s.Update(t.Context(), Settings{SchemaVersion: 1, Revision: 1, RetentionDays: 1})
	if err := s.Sweep(t.Context(), expiry.Add(-time.Nanosecond)); err != nil || len(r.calls) != 0 {
		t.Fatal("deleted early", err)
	}
	if err := s.Sweep(t.Context(), *expiry); err != nil || len(r.calls) != 1 {
		t.Fatal("did not delete at expiry", err)
	}
}
func TestRetentionWriteFailureAndUncertainPause(t *testing.T) {
	r := &fakeRepository{entries: []*jobs.TrashEntry{{ID: "old", State: "trashed", DeletedAt: time.Now().Add(-10 * 24 * time.Hour)}}}
	s := newService(t, r)
	original := s.writeSettings
	s.writeSettings = func(context.Context, Settings) error { return errors.New("private synthetic path") }
	if _, err := s.Update(t.Context(), Settings{SchemaVersion: 1, Revision: 1, RetentionDays: 1}); err == nil || s.Settings().RetentionDays != 0 {
		t.Fatal("failed write enabled deletion", err)
	}
	s.writeSettings = func(context.Context, Settings) error { return store.ErrDurabilityUncertain }
	got, err := s.Update(t.Context(), Settings{SchemaVersion: 1, Revision: 1, RetentionDays: 1})
	if !errors.Is(err, ErrUncertain) || got.Revision != 2 || got.PersistenceWarning == "" {
		t.Fatal(got, err)
	}
	if err = s.Sweep(t.Context(), time.Now()); !errors.Is(err, ErrUncertain) || len(r.calls) != 0 {
		t.Fatal("uncertain policy deleted", err)
	}
	s.writeSettings = original
	if _, err = s.Update(t.Context(), Settings{SchemaVersion: 1, Revision: 2, RetentionDays: 0}); err != nil {
		t.Fatal(err)
	}
	if s.Settings().PersistenceWarning != "" {
		t.Fatal("warning not cleared after explicit durable update")
	}
}
func TestRetentionCancellationReleasesGateAndClose(t *testing.T) {
	s := newService(t, &fakeRepository{})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	for i := 0; i < 100; i++ {
		if err := s.Sweep(ctx, time.Now()); !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	}
	deadline, stop := context.WithTimeout(t.Context(), time.Second)
	defer stop()
	if err := s.Sweep(deadline, time.Now()); err != nil {
		t.Fatal("cancel leaked gate", err)
	}
	s.Start(t.Context())
	if err := s.Close(deadline); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Update(deadline, Settings{SchemaVersion: 1, Revision: 1, RetentionDays: 1}); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
}
func TestRetentionPurgingResumesWhileDisabledBoundedAndStopsOnFailure(t *testing.T) {
	r := &fakeRepository{}
	for i := 0; i < 25; i++ {
		r.entries = append(r.entries, &jobs.TrashEntry{ID: fmt.Sprint(i), State: "purging"})
	}
	s := newService(t, r)
	if err := s.Sweep(t.Context(), time.Now()); err != nil || len(r.calls) != 20 || s.Status().LastPurged != 20 {
		t.Fatal("purge resume not bounded", err, len(r.calls))
	}
	r.calls = nil
	r.purge = func(context.Context, string) (int, error) { return 0, errors.New("synthetic storage failure") }
	if err := s.Sweep(t.Context(), time.Now()); err == nil || len(r.calls) != 1 || s.Status().LastError == "" || s.Status().Running {
		t.Fatal("failed sweep misrepresented", err)
	}
}
func TestRetentionSerializesPolicyAndPurge(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	r := &fakeRepository{entries: []*jobs.TrashEntry{{ID: "purging", State: "purging"}}}
	r.purge = func(ctx context.Context, _ string) (int, error) {
		close(entered)
		select {
		case <-release:
			return 1, nil
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	s := newService(t, r)
	done := make(chan error, 1)
	go func() { done <- s.Sweep(t.Context(), time.Now()) }()
	<-entered
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if _, err := s.Update(ctx, Settings{SchemaVersion: 1, Revision: 1, RetentionDays: 1}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("policy raced in-flight purge", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
