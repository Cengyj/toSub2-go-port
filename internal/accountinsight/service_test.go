package accountinsight

import (
	"context"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"tosub2-go-port/internal/protocol"
	"tosub2-go-port/internal/store"
)

type fixture struct {
	mu      sync.Mutex
	account Account
	calls   atomic.Int32
	query   func(context.Context) protocol.InsightResult
}

func (f *fixture) provider() Provider {
	return Provider{Accounts: func() []Account { f.mu.Lock(); defer f.mu.Unlock(); return []Account{f.account} }, Acquire: func(ctx context.Context, id string) (*Lease, error) {
		f.calls.Add(1)
		f.mu.Lock()
		revision := f.account.Revision
		f.mu.Unlock()
		return &Lease{Context: ctx, Validate: func() error {
			f.mu.Lock()
			defer f.mu.Unlock()
			if revision != f.account.Revision {
				return errors.New("changed")
			}
			return ctx.Err()
		}, Release: func() {}, Query: func(ctx context.Context, _ protocol.InsightsOptions) protocol.InsightResult { return f.query(ctx) }}, nil
	}}
}
func newFixture(t *testing.T) (*Service, *fixture) {
	t.Helper()
	f := &fixture{account: Account{ID: "synthetic", AuthorizationMethod: "bps", Revision: "one", CanQuery: true}}
	f.query = func(ctx context.Context) protocol.InsightResult {
		used := 0.0
		return protocol.InsightResult{Usage: &protocol.InsightUsage{Source: "codex", ObservedAt: time.Now().UTC(), Windows: []protocol.InsightWindow{{Name: "5h", UsedPercent: &used}}}, Errors: []protocol.InsightError{}}
	}
	s, err := New(t.TempDir(), f.provider())
	if err != nil {
		t.Fatal(err)
	}
	s.tick = 5 * time.Millisecond
	s.budget = time.Second
	s.Start(context.Background())
	t.Cleanup(func() {
		ctx, c := context.WithTimeout(context.Background(), time.Second)
		defer c()
		if err := s.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	return s, f
}
func enable(t *testing.T, s *Service, auto bool) {
	t.Helper()
	c := s.Settings()
	c.Enabled = true
	c.Automatic = auto
	if _, err := s.Update(context.Background(), c); err != nil {
		t.Fatal(err)
	}
}
func waitFor(t *testing.T, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition not reached")
}
func waitResult(t *testing.T, s *Service, status string) Snapshot {
	t.Helper()
	waitFor(t, func() bool {
		v, _ := s.Snapshot("synthetic")
		return v.LastAttempt.Status == status && s.Status().Active == 0
	})
	v, _ := s.Snapshot("synthetic")
	return v
}
func TestInsightDisabledAndCacheReadsNeverQuery(t *testing.T) {
	s, f := newFixture(t)
	if _, e := s.Refresh([]string{"synthetic"}); !errors.Is(e, ErrDisabled) {
		t.Fatal(e)
	}
	for range 5 {
		s.Snapshot("synthetic")
		s.Status()
		s.Settings()
	}
	time.Sleep(20 * time.Millisecond)
	if f.calls.Load() != 0 {
		t.Fatal("read triggered upstream")
	}
}
func TestInsightManualDedupAndPersistentZero(t *testing.T) {
	s, f := newFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	base := f.query
	f.query = func(ctx context.Context) protocol.InsightResult {
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
		}
		return base(ctx)
	}
	enable(t, s, false)
	r, e := s.Refresh([]string{"synthetic", "synthetic"})
	if e != nil || len(r.Accepted) != 1 {
		t.Fatal(r, e)
	}
	<-entered
	r, e = s.Refresh([]string{"synthetic"})
	if e != nil || len(r.Merged) != 1 {
		t.Fatal(r, e)
	}
	close(release)
	v := waitResult(t, s, "success")
	if f.calls.Load() != 1 || !v.Persisted || v.Usage == nil || *v.Usage.Windows[0].UsedPercent != 0 {
		t.Fatal(v, f.calls.Load())
	}
	c := s.Settings()
	c.Enabled = false
	if _, e = s.Update(context.Background(), c); e != nil {
		t.Fatal(e)
	}
	reloaded, e := New(s.disk.Root+"/..", f.provider())
	if e != nil {
		t.Fatal(e)
	}
	got, _ := reloaded.Snapshot("synthetic")
	if reloaded.Settings().Enabled || got.Usage == nil || !got.Stale {
		t.Fatal(got)
	}
}
func TestInsightFailedAttemptRetainsLastObservation(t *testing.T) {
	s, f := newFixture(t)
	enable(t, s, false)
	s.Refresh([]string{"synthetic"})
	old := waitResult(t, s, "success")
	f.query = func(context.Context) protocol.InsightResult {
		return protocol.InsightResult{Errors: []protocol.InsightError{{Component: "usage", Code: "network_error", Message: "查询网络失败"}}}
	}
	s.mu.Lock()
	r := s.snapshots["synthetic"]
	r.Snapshot.NextAllowedAt = nil
	s.snapshots["synthetic"] = r
	s.mu.Unlock()
	s.Refresh([]string{"synthetic"})
	v := waitResult(t, s, "failed")
	if v.Usage == nil || !v.Usage.ObservedAt.Equal(old.Usage.ObservedAt) || !v.LastSuccessAt.Equal(*old.LastSuccessAt) {
		t.Fatal("lost old observation", v)
	}
}
func TestInsightPartialHasIndependentSources(t *testing.T) {
	s, f := newFixture(t)
	f.query = func(context.Context) protocol.InsightResult {
		yes := true
		return protocol.InsightResult{BPS: &protocol.InsightBPS{Source: "bps_access", ObservedAt: time.Now(), Allowed: &yes}, Errors: []protocol.InsightError{{Component: "usage", Code: "permission_denied", Message: "额度查询受限"}}}
	}
	enable(t, s, false)
	s.Refresh([]string{"synthetic"})
	v := waitResult(t, s, "partial")
	if v.BPS == nil || v.Usage != nil || len(v.Errors) != 1 {
		t.Fatal(v)
	}
}
func TestInsightOldGrantResponseCannotCommit(t *testing.T) {
	s, f := newFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	base := f.query
	f.query = func(ctx context.Context) protocol.InsightResult { close(entered); <-release; return base(ctx) }
	enable(t, s, false)
	s.Refresh([]string{"synthetic"})
	<-entered
	f.mu.Lock()
	f.account.Revision = "two"
	f.mu.Unlock()
	close(release)
	waitFor(t, func() bool { return s.Status().Active == 0 })
	v, _ := s.Snapshot("synthetic")
	if v.Usage != nil || v.LastAttempt.Status != "never" {
		t.Fatal(v)
	}
	if _, e := os.Stat(s.disk.Path(snapshotFile("synthetic"))); !os.IsNotExist(e) {
		t.Fatal("stale result saved", e)
	}
}
func TestInsightDisableCancelsAndManualModeNeverAutoQueries(t *testing.T) {
	s, f := newFixture(t)
	entered := make(chan struct{})
	f.query = func(ctx context.Context) protocol.InsightResult {
		close(entered)
		<-ctx.Done()
		return protocol.InsightResult{}
	}
	enable(t, s, false)
	time.Sleep(25 * time.Millisecond)
	if f.calls.Load() != 0 {
		t.Fatal("manual mode queried")
	}
	s.Refresh([]string{"synthetic"})
	<-entered
	c := s.Settings()
	c.Enabled = false
	s.Update(context.Background(), c)
	waitResult(t, s, "cancelled")
	if f.calls.Load() != 1 {
		t.Fatal(f.calls.Load())
	}
}
func TestInsightConfigConflictAndDurabilityFailure(t *testing.T) {
	s, _ := newFixture(t)
	original := s.Settings()
	enable(t, s, false)
	if _, e := s.Update(context.Background(), original); !errors.Is(e, ErrConflict) {
		t.Fatal(e)
	}
	s.persist = func(context.Context, string, any) error { return store.ErrDurabilityUncertain }
	c := s.Settings()
	c.Enabled = false
	if _, e := s.Update(context.Background(), c); e == nil {
		t.Fatal("uncertain write accepted")
	}
	if s.Settings().Enabled || s.Settings().PersistenceWarning == "" {
		t.Fatal(s.Settings())
	}
}
func TestInsightAutomaticExclusionAndRestartCooldown(t *testing.T) {
	s, f := newFixture(t)
	enable(t, s, true)
	future := time.Now().Add(time.Hour)
	s.mu.Lock()
	s.snapshots["synthetic"] = storedSnapshot{SchemaVersion: 1, AccountRevision: "one", Snapshot: Snapshot{ID: "synthetic", NextCheckAt: &future, NextAllowedAt: &future}}
	s.mu.Unlock()
	time.Sleep(20 * time.Millisecond)
	if f.calls.Load() != 0 {
		t.Fatal("ignored cooldown")
	}
	c := s.Settings()
	c.ExcludedAccountIDs = []string{"synthetic"}
	s.Update(context.Background(), c)
	s.mu.Lock()
	delete(s.snapshots, "synthetic")
	s.mu.Unlock()
	time.Sleep(20 * time.Millisecond)
	if f.calls.Load() != 0 {
		t.Fatal("excluded account queried")
	}
}
