package accountinsight

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"tosub2-go-port/internal/protocol"
	"tosub2-go-port/internal/store"
)

type reliabilityClock struct{ nanos atomic.Int64 }

func newReliabilityClock(s *Service) *reliabilityClock {
	c := &reliabilityClock{}
	c.nanos.Store(time.Now().UTC().Truncate(time.Second).UnixNano())
	s.mu.Lock()
	s.now = c.now
	s.mu.Unlock()
	return c
}
func (c *reliabilityClock) now() time.Time          { return time.Unix(0, c.nanos.Load()).UTC() }
func (c *reliabilityClock) advance(d time.Duration) { c.nanos.Add(int64(d)) }

func reliabilityRefresh(t *testing.T, s *Service) {
	t.Helper()
	receipt, err := s.Refresh([]string{"synthetic"})
	if err != nil || len(receipt.Accepted) != 1 || len(receipt.Skipped) != 0 {
		t.Fatalf("query was not admitted: %+v %v", receipt, err)
	}
}

func TestInsightReliabilityComponentCooldownLeavesBPSFresh(t *testing.T) {
	s, f := newFixture(t)
	clock := newReliabilityClock(s)
	baseAcquire := s.provider.Acquire
	var rounds atomic.Int32
	options := make(chan protocol.InsightsOptions, 2)
	s.provider.Acquire = func(ctx context.Context, id string) (*Lease, error) {
		lease, err := baseAcquire(ctx, id)
		if err != nil {
			return nil, err
		}
		lease.Query = func(_ context.Context, opts protocol.InsightsOptions) protocol.InsightResult {
			options <- opts
			yes := true
			result := protocol.InsightResult{BPS: &protocol.InsightBPS{Source: "bps_access", ObservedAt: clock.now(), Allowed: &yes}, AttemptedComponents: []string{"bps"}}
			if rounds.Add(1) == 1 {
				result.AttemptedComponents = append(result.AttemptedComponents, "usage")
				result.Errors = []protocol.InsightError{{Component: "usage", Code: "unsupported_authorization", Message: "当前授权不支持额度查询"}}
			}
			return result
		}
		return lease, nil
	}
	enable(t, s, false)
	reliabilityRefresh(t, s)
	first := waitResult(t, s, "partial")
	<-options
	usageState := first.Components["usage"]
	if usageState.NextAllowedAt == nil || !usageState.NextAllowedAt.After(clock.now().Add(time.Hour)) {
		t.Fatal("explicit unsupported query did not receive a durable component cooldown")
	}
	clock.advance(10 * time.Second)
	reliabilityRefresh(t, s)
	waitFor(t, func() bool { return rounds.Load() == 2 && s.Status().Active == 0 })
	second, _ := s.Snapshot("synthetic")
	opts := <-options
	if !slices.Contains(opts.SkipComponents, "usage") || slices.Contains(opts.SkipComponents, "bps") {
		t.Fatalf("component cooldown blocked a healthy BPS source: %+v", opts)
	}
	if f.calls.Load() != 2 || second.BPS == nil || !second.BPS.ObservedAt.Equal(clock.now()) || !second.BPS.ObservedAt.After(first.BPS.ObservedAt) {
		t.Fatalf("BPS was not independently refreshed: %+v", second)
	}
	if state := second.Components["usage"]; state.NextAllowedAt == nil || !state.NextAllowedAt.Equal(*usageState.NextAllowedAt) || state.ConsecutiveFailures != usageState.ConsecutiveFailures {
		t.Fatal("skipped quota component extended or cleared its existing cooldown")
	}
}

func TestInsightReliabilityResetFailureAndSkipKeepHistoricalDetails(t *testing.T) {
	s, _ := newFixture(t)
	clock := newReliabilityClock(s)
	firstObserved := clock.now()
	expiry := firstObserved.Add(24 * time.Hour)
	baseAcquire := s.provider.Acquire
	var rounds atomic.Int32
	options := make(chan protocol.InsightsOptions, 3)
	s.provider.Acquire = func(ctx context.Context, id string) (*Lease, error) {
		lease, err := baseAcquire(ctx, id)
		if err != nil {
			return nil, err
		}
		lease.Query = func(_ context.Context, opts protocol.InsightsOptions) protocol.InsightResult {
			options <- opts
			result := protocol.InsightResult{Usage: &protocol.InsightUsage{Source: "codex_wham", ObservedAt: clock.now()}, AttemptedComponents: []string{"usage"}}
			switch rounds.Add(1) {
			case 1:
				n := int64(3)
				result.Usage.ResetCredits = &protocol.InsightResetCredits{AvailableCount: &n, ObservedAt: firstObserved, Credits: []protocol.InsightResetCredit{{ExpiresAt: &expiry}}}
				result.AttemptedComponents = append(result.AttemptedComponents, "resetCredits")
			case 2:
				result.AttemptedComponents = append(result.AttemptedComponents, "resetCredits")
				result.Errors = []protocol.InsightError{{Component: "resetCredits", Code: "network_error", Message: "重置券暂不可读取"}}
			}
			return result
		}
		return lease, nil
	}
	cfg := s.Settings()
	cfg.Enabled, cfg.Automatic, cfg.IncludeResetCredits = true, false, true
	if _, err := s.Update(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	reliabilityRefresh(t, s)
	waitResult(t, s, "success")
	<-options
	var cooldown *time.Time
	for round := int32(2); round <= 3; round++ {
		clock.advance(10 * time.Second)
		reliabilityRefresh(t, s)
		waitFor(t, func() bool { return rounds.Load() == round && s.Status().Active == 0 })
		got, _ := s.Snapshot("synthetic")
		opts := <-options
		if got.Usage == nil || !got.Usage.ObservedAt.Equal(clock.now()) || got.Usage.ResetCredits == nil {
			t.Fatalf("round %d discarded independently dated reset credits: %+v", round, got)
		}
		details := got.Usage.ResetCredits
		if details.AvailableCount == nil || *details.AvailableCount != 3 || !details.ObservedAt.Equal(firstObserved) || len(details.Credits) != 1 || details.Credits[0].ExpiresAt == nil || !details.Credits[0].ExpiresAt.Equal(expiry) {
			t.Fatalf("round %d changed historical reset details: %+v", round, details)
		}
		state := got.Components["resetCredits"]
		if state.NextAllowedAt == nil || state.ConsecutiveFailures != 1 {
			t.Fatal("failed or skipped reset credits incorrectly became successful")
		}
		if round == 2 {
			cooldown = state.NextAllowedAt
		}
		if round == 3 && (!slices.Contains(opts.SkipComponents, "resetCredits") || slices.Contains(opts.SkipComponents, "usage") || !state.NextAllowedAt.Equal(*cooldown)) {
			t.Fatalf("reset-credit cooldown did not preserve independent usage refresh: %+v", opts)
		}
	}
}

func TestInsightReliabilityIntervalChangesRescheduleExistingRows(t *testing.T) {
	s, _ := newFixture(t)
	clock := newReliabilityClock(s)
	cfg := s.Settings()
	cfg.Enabled, cfg.Automatic, cfg.IntervalMinutes = true, false, 60
	if _, err := s.Update(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	reliabilityRefresh(t, s)
	first := waitResult(t, s, "success")
	finished := *first.LastAttempt.FinishedAt
	for _, interval := range []int{1, 30} {
		cfg = s.Settings()
		cfg.Automatic, cfg.IntervalMinutes = true, interval
		if _, err := s.Update(context.Background(), cfg); err != nil {
			t.Fatal(err)
		}
		got, _ := s.Snapshot("synthetic")
		want := finished.Add(time.Duration(interval) * time.Minute)
		if got.NextCheckAt == nil || !got.NextCheckAt.Equal(want) {
			t.Fatalf("new interval %d did not replace the old due time: got=%v want=%v", interval, got.NextCheckAt, want)
		}
	}
	cooldown := clock.now().Add(5 * time.Minute)
	s.mu.Lock()
	row := s.snapshots["synthetic"]
	row.Snapshot.NextAllowedAt = &cooldown
	s.snapshots["synthetic"] = row
	s.mu.Unlock()
	cfg = s.Settings()
	cfg.IntervalMinutes = 1
	if _, err := s.Update(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Snapshot("synthetic")
	if got.NextCheckAt == nil || got.NextCheckAt.Before(cooldown) {
		t.Fatal("interval update bypassed an existing upstream cooldown")
	}
}

func TestInsightReliabilityAutoCancelDoesNotImmediatelyRequeue(t *testing.T) {
	for _, queued := range []bool{false, true} {
		t.Run(fmt.Sprintf("queued=%t", queued), func(t *testing.T) {
			s, f := newFixture(t)
			clock := newReliabilityClock(s)
			f.query = func(ctx context.Context) protocol.InsightResult { <-ctx.Done(); return protocol.InsightResult{} }
			enable(t, s, false)
			finished := clock.now().Add(-16 * time.Minute)
			s.mu.Lock()
			s.snapshots["synthetic"] = storedSnapshot{SchemaVersion: 1, AccountRevision: "one", Snapshot: Snapshot{ID: "synthetic", AuthorizationMethod: "bps", LastAttempt: Attempt{Status: "success", FinishedAt: &finished}}}
			if queued {
				s.nextAdmission = clock.now().Add(time.Hour)
			}
			s.mu.Unlock()
			cfg := s.Settings()
			cfg.Automatic = true
			if _, err := s.Update(context.Background(), cfg); err != nil {
				t.Fatal(err)
			}
			waitFor(t, func() bool {
				status := s.Status()
				if queued {
					return status.Queued == 1 && status.Active == 0
				}
				return status.Active == 1 && f.calls.Load() == 1
			})
			if !s.Cancel("synthetic") {
				t.Fatal("active or pending automatic query was not cancellable")
			}
			got := waitResult(t, s, "cancelled")
			if got.NextCheckAt == nil || !got.NextCheckAt.After(clock.now().Add(10*time.Second)) {
				t.Fatal("cancel did not defer the next automatic query")
			}
			calls := f.calls.Load()
			clock.advance(10 * time.Second)
			s.notify()
			time.Sleep(30 * time.Millisecond)
			if status := s.Status(); f.calls.Load() != calls || status.Queued != 0 || status.Active != 0 {
				t.Fatalf("cancelled automatic query immediately returned: calls=%d status=%+v", f.calls.Load(), status)
			}
		})
	}
}

func TestInsightReliabilityExpiredGrantRemainsBlockedUntilRevisionChanges(t *testing.T) {
	s, f := newFixture(t)
	clock := newReliabilityClock(s)
	baseAcquire := s.provider.Acquire
	s.provider.Acquire = func(ctx context.Context, id string) (*Lease, error) {
		f.mu.Lock()
		revision := f.account.Revision
		f.mu.Unlock()
		if revision == "one" {
			f.calls.Add(1)
			return nil, &QueryError{Code: "authorization_expired", Message: "当前授权已过期，请先刷新授权"}
		}
		return baseAcquire(ctx, id)
	}
	enable(t, s, false)
	reliabilityRefresh(t, s)
	waitResult(t, s, "skipped")
	clock.advance(2 * time.Minute)
	blocked, _ := s.Snapshot("synthetic")
	if blocked.CanQuery || blocked.UnavailableReason != "当前授权已过期，请先刷新授权" {
		t.Fatalf("known expiry was replaced by a transient cooldown: %+v", blocked)
	}
	receipt, err := s.Refresh([]string{"synthetic"})
	if err != nil || len(receipt.Accepted) != 0 || len(receipt.Skipped) != 1 || f.calls.Load() != 1 {
		t.Fatalf("same expired grant was reacquired: %+v %v", receipt, err)
	}
	cfg := s.Settings()
	cfg.Automatic = true
	if _, err := s.Update(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	clock.advance(time.Hour)
	s.notify()
	time.Sleep(30 * time.Millisecond)
	if f.calls.Load() != 1 {
		t.Fatal("automatic scheduling repeatedly acquired a known expired grant")
	}
	cfg = s.Settings()
	cfg.Automatic = false
	if _, err := s.Update(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.account.Revision = "two"
	f.mu.Unlock()
	ready, _ := s.Snapshot("synthetic")
	if !ready.CanQuery || ready.UnavailableReason != "" {
		t.Fatalf("new grant retained old expiry block: %+v", ready)
	}
	reliabilityRefresh(t, s)
	waitResult(t, s, "success")
	if f.calls.Load() != 2 {
		t.Fatal("new authorization revision did not admit exactly one query")
	}
}

func TestInsightReliabilityStatusIgnoresDeletedAndSupersededRows(t *testing.T) {
	s, f := newFixture(t)
	enable(t, s, false)
	s.mu.Lock()
	s.snapshots["synthetic"] = storedSnapshot{SchemaVersion: 1, AccountRevision: "one", Snapshot: Snapshot{ID: "synthetic", LastAttempt: Attempt{Status: "success"}}}
	s.snapshots["deleted-account"] = storedSnapshot{SchemaVersion: 1, AccountRevision: "one", Snapshot: Snapshot{ID: "deleted-account", LastAttempt: Attempt{Status: "failed"}}}
	s.mu.Unlock()
	if got := s.Status(); got.Counts.Success != 1 || got.Counts.Failed != 0 {
		t.Fatalf("deleted account polluted status counts: %+v", got)
	}
	f.mu.Lock()
	f.account.Revision = "two"
	f.mu.Unlock()
	if got := s.Status(); got.Counts != (Counts{}) {
		t.Fatalf("obsolete authorization polluted status counts: %+v", got)
	}
}

func TestInsightReliabilitySnapshotsReadAccountInventoryOncePerPage(t *testing.T) {
	accounts := make([]Account, 100)
	ids := make([]string, len(accounts))
	for i := range accounts {
		ids[i] = fmt.Sprintf("synthetic-%03d", i)
		accounts[i] = Account{ID: ids[i], AuthorizationMethod: "chatgpt", Revision: "one", CanQuery: true}
	}
	var reads, acquisitions atomic.Int32
	s, err := New(t.TempDir(), Provider{Accounts: func() []Account { reads.Add(1); return accounts }, Acquire: func(context.Context, string) (*Lease, error) {
		acquisitions.Add(1)
		return nil, errors.New("not expected")
	}})
	if err != nil {
		t.Fatal(err)
	}
	reads.Store(0)
	got, _ := s.Snapshots(ids)
	if reads.Load() != 1 || acquisitions.Load() != 0 || len(got) != len(ids) {
		t.Fatalf("page read rescanned accounts or acquired credentials: reads=%d acquired=%d rows=%d", reads.Load(), acquisitions.Load(), len(got))
	}
	for _, id := range ids {
		if got[id].ID != id {
			t.Fatal("page snapshots lost account ordering keys")
		}
	}
}

func TestInsightReliabilityStartupAdmissionRemainsBoundedAcrossWakeups(t *testing.T) {
	accounts := make([]Account, 20)
	for i := range accounts {
		accounts[i] = Account{ID: fmt.Sprintf("synthetic-%02d", i), AuthorizationMethod: "chatgpt", Revision: "one", CanQuery: true}
	}
	var calls atomic.Int32
	var clock *reliabilityClock
	s, err := New(t.TempDir(), Provider{Accounts: func() []Account { return accounts }, Acquire: func(ctx context.Context, id string) (*Lease, error) {
		calls.Add(1)
		return &Lease{Context: ctx, Validate: func() error { return ctx.Err() }, Release: func() {}, Query: func(context.Context, protocol.InsightsOptions) protocol.InsightResult {
			return protocol.InsightResult{Usage: &protocol.InsightUsage{Source: "codex_wham", ObservedAt: clock.now()}, AttemptedComponents: []string{"usage"}}
		}}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	clock = newReliabilityClock(s)
	s.tick = 2 * time.Millisecond
	s.admissionInterval = time.Second
	enable(t, s, true)
	past := clock.now().Add(-time.Minute)
	for _, account := range accounts {
		s.snapshots[account.ID] = storedSnapshot{SchemaVersion: 1, AccountRevision: account.Revision, Snapshot: Snapshot{ID: account.ID, AuthorizationMethod: account.AuthorizationMethod, NextCheckAt: &past}}
	}
	s.Start(context.Background())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := s.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	waitFor(t, func() bool { return calls.Load() == 1 && s.Status().Active == 0 })
	for range 100 {
		s.notify()
	}
	time.Sleep(30 * time.Millisecond)
	if calls.Load() != 1 {
		t.Fatal("wakeups bypassed startup admission spacing")
	}
	clock.advance(500 * time.Millisecond)
	s.notify()
	time.Sleep(20 * time.Millisecond)
	if calls.Load() != 1 {
		t.Fatal("next admission started before its bounded interval")
	}
	clock.advance(500 * time.Millisecond)
	s.notify()
	waitFor(t, func() bool { return calls.Load() == 2 && s.Status().Active == 0 })
	if status := s.Status(); status.Queued != len(accounts)-2 {
		t.Fatalf("startup discarded or replayed pending accounts: %+v", status)
	}
}

func TestInsightReliabilityUncertainSettingsReadBackAndFailClosed(t *testing.T) {
	s, f := newFixture(t)
	enable(t, s, false)
	before := s.Settings()
	nativePersist := s.persist
	s.persist = func(ctx context.Context, name string, value any) error {
		if err := nativePersist(ctx, name, value); err != nil {
			return err
		}
		return store.ErrDurabilityUncertain
	}
	requested := before
	requested.IntervalMinutes = 30
	got, err := s.Update(context.Background(), requested)
	if !errors.Is(err, ErrPersistenceUncertain) || got.Revision != before.Revision+1 || got.IntervalMinutes != 30 || got.Enabled || got.PersistenceWarning == "" {
		t.Fatalf("uncertain visible settings were ignored or silently enabled: %+v %v", got, err)
	}
	var disk Settings
	if err := s.read("settings.json", &disk); err != nil {
		t.Fatal(err)
	}
	if disk.Revision != got.Revision || disk.IntervalMinutes != got.IntervalMinutes || !disk.Enabled {
		t.Fatal("test did not model rename succeeding before durability confirmation")
	}
	if _, err := s.Refresh([]string{"synthetic"}); !errors.Is(err, ErrDisabled) || f.calls.Load() != 0 {
		t.Fatal("uncertain configuration allowed collection")
	}
	s.persist = nativePersist
	if _, err := s.Update(context.Background(), before); !errors.Is(err, ErrConflict) {
		t.Fatal("stale revision could overwrite reconciled settings")
	}
	got.Enabled = true
	saved, err := s.Update(context.Background(), got)
	if err != nil || !saved.Enabled || saved.Revision != disk.Revision+1 || saved.PersistenceWarning != "" {
		t.Fatalf("explicit reconciled save did not restore collection: %+v %v", saved, err)
	}
}

func TestInsightReliabilityDefiniteSettingsFailureKeepsPreviousConfiguration(t *testing.T) {
	s, _ := newFixture(t)
	enable(t, s, false)
	before := s.Settings()
	s.persist = func(context.Context, string, any) error { return errors.New("synthetic disk full") }
	requested := before
	requested.IntervalMinutes, requested.IncludeResetCredits = 30, true
	got, err := s.Update(context.Background(), requested)
	if !errors.Is(err, ErrPersistence) || got.Revision != before.Revision || got.Enabled != before.Enabled || got.IntervalMinutes != before.IntervalMinutes || got.IncludeResetCredits != before.IncludeResetCredits || got.PersistenceWarning == "" {
		t.Fatalf("failed save changed live business configuration: %+v %v", got, err)
	}
	reloaded, err := New(filepath.Dir(s.disk.Root), s.provider)
	if err != nil {
		t.Fatal(err)
	}
	disk := reloaded.Settings()
	if disk.Revision != before.Revision || disk.Enabled != before.Enabled || disk.IntervalMinutes != before.IntervalMinutes || disk.IncludeResetCredits != before.IncludeResetCredits {
		t.Fatalf("failed save changed durable settings: %+v", disk)
	}
}
