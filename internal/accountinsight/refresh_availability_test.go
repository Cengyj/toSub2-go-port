package accountinsight

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"tosub2-go-port/internal/protocol"
)

func TestRefreshAvailabilityQueuedSnapshotIsNotRefreshable(t *testing.T) {
	s := newRefreshAllQueueFixture(t, []Account{{ID: "synthetic", Revision: "one", CanQuery: true}})
	if receipt, err := s.Refresh([]string{"synthetic"}); err != nil || len(receipt.Accepted) != 1 {
		t.Fatal("synthetic query was not admitted", err)
	}
	snapshot, ok := s.Snapshot("synthetic")
	if !ok || snapshot.CanQuery || snapshot.UnavailableReason != "查询已排队，请等待完成" {
		t.Fatal("queued query remained refreshable", snapshot.CanQuery, snapshot.UnavailableReason)
	}
	snapshotJSON, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	var live struct {
		RefreshState string `json:"refreshState"`
	}
	if json.Unmarshal(snapshotJSON, &live) != nil || live.RefreshState != "queued" {
		t.Fatal("snapshot did not expose its actual queued flight")
	}
	if s.snapshots["synthetic"].Snapshot.RefreshState != "" {
		t.Fatal("public live state mutated the stored observation")
	}
	data, err := json.Marshal(s.Status())
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Refresh *struct{ Total, Eligible, Queued, Active int } `json:"refresh"`
	}
	if json.Unmarshal(data, &wire) != nil || wire.Refresh == nil || wire.Refresh.Total != 1 || wire.Refresh.Queued != 1 || wire.Refresh.Eligible != 0 || wire.Refresh.Active != 0 {
		t.Fatal("status omitted the complete manual refresh availability")
	}
	if receipt, err := s.Refresh([]string{"synthetic"}); err != nil || len(receipt.Accepted) != 0 || len(receipt.Merged) != 1 || len(receipt.Skipped) != 0 {
		t.Fatal("availability projection changed repeat-admission semantics", err)
	}
}

func TestRefreshAvailabilityUsesOneInventoryAndClockForBothScopes(t *testing.T) {
	accounts := []Account{
		{ID: "ready", Revision: "one", CanQuery: true},
		{ID: "pending", Revision: "one", CanQuery: true},
		{ID: "cooling", Revision: "one", CanQuery: true},
		{ID: "blocked", Revision: "one", CanQuery: true},
		{ID: "busy-account", Revision: "one", Reason: "账号操作进行中"},
	}
	accounts = append(accounts, accounts[0])
	s := newRefreshAllQueueFixture(t, accounts)
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	next := now.Add(time.Minute)
	s.now = func() time.Time { return now }
	s.enqueueLocked(accounts[1], "pending-query", false)
	s.snapshots["cooling"] = storedSnapshot{AccountRevision: "one", Snapshot: Snapshot{NextAllowedAt: &next}}
	s.snapshots["blocked"] = storedSnapshot{AccountRevision: "one", BlockedCode: "authorization_expired", BlockedReason: "当前授权已过期"}
	// Automatic exclusions do not prohibit explicit manual queries.
	s.settings.ExcludedAccountIDs = []string{"ready"}
	providerCalls, clockCalls := 0, 0
	s.provider.Accounts = func() []Account { providerCalls++; return accounts }
	s.now = func() time.Time { clockCalls++; return now }
	got := s.StatusForSelection([]string{"ready", "ready", "cooling", "missing", "missing"})
	if providerCalls != 1 || clockCalls != 1 {
		t.Fatalf("status used inconsistent observations: inventory=%d clock=%d", providerCalls, clockCalls)
	}
	if got.Refresh.Total != 5 || got.Refresh.Eligible != 1 || got.Refresh.Queued != 1 || got.Refresh.Active != 0 || got.Refresh.CoolingDown != 1 || got.Refresh.Unavailable != 2 {
		t.Fatalf("global availability = %+v", got.Refresh)
	}
	if got.Selection == nil || got.Selection.Total != 3 || got.Selection.Eligible != 1 || got.Selection.CoolingDown != 1 || got.Selection.Unavailable != 1 || got.Selection.Queued != 0 || got.Selection.RetryAfterSeconds != 60 || got.Selection.NextAllowedAt == nil || !got.Selection.NextAllowedAt.Equal(next) {
		t.Fatalf("selected availability = %+v", got.Selection)
	}
	if s.Status().Selection != nil {
		t.Fatal("unscoped status included a selection")
	}
	if snapshot, _ := s.Snapshot("blocked"); snapshot.RefreshState != "unavailable" {
		t.Fatal("blocked grant did not expose unavailable refresh state")
	}
	if empty := s.StatusForSelection(nil).Selection; empty == nil || empty.Total != 0 {
		t.Fatal("empty internal selection became a global selection")
	}
	// A caller cannot mutate the service's cooldown through a returned pointer.
	*got.Selection.NextAllowedAt = next.Add(time.Hour)
	if current := s.StatusForSelection([]string{"cooling"}).Selection; current.NextAllowedAt == nil || !current.NextAllowedAt.Equal(next) {
		t.Fatal("returned cooldown aliased internal state")
	}
}

func TestRefreshAvailabilityCancelledActiveFlightAndChangedGrantStayBusy(t *testing.T) {
	s, f := newFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	finish := func() { once.Do(func() { close(release) }) }
	defer finish()
	f.query = func(context.Context) protocol.InsightResult {
		close(entered)
		<-release // Model transport cleanup that has not released its lease yet.
		return protocol.InsightResult{}
	}
	enable(t, s, false)
	if _, err := s.Refresh([]string{"synthetic"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("query did not start")
	}
	if snapshot, _ := s.Snapshot("synthetic"); snapshot.RefreshState != "active" || snapshot.LastAttempt.Status != "running" {
		t.Fatal("running query did not expose its active flight")
	}
	if !s.Cancel("synthetic") {
		t.Fatal("active query was not cancellable")
	}
	f.mu.Lock()
	f.account.Revision = "new-grant"
	f.mu.Unlock()
	snapshot, _ := s.Snapshot("synthetic")
	if snapshot.LastAttempt.Status != "never" || snapshot.RefreshState != "active" || snapshot.CanQuery || snapshot.UnavailableReason != "查询正在进行，请等待完成" {
		t.Fatal("new grant made an unreleased old flight appear refreshable", snapshot.LastAttempt.Status, snapshot.CanQuery, snapshot.UnavailableReason)
	}
	if got := s.StatusForSelection([]string{"synthetic"}); got.Active != 1 || got.Refresh.Active != 1 || got.Refresh.Eligible != 0 || got.Selection.Active != 1 {
		t.Fatalf("cancel request prematurely released busy availability: %+v", got)
	}
	if receipt, err := s.Refresh([]string{"synthetic"}); err != nil || len(receipt.Merged) != 1 || f.calls.Load() != 1 {
		t.Fatal("cancelled in-flight query was duplicated", err)
	}
	settings := s.Settings()
	settings.Enabled = false
	if _, err := s.Update(t.Context(), settings); err != nil {
		t.Fatal(err)
	}
	if got := s.Status().Refresh; got.Active != 1 || got.Eligible != 0 {
		t.Fatal("disabling settings hid unfinished work", got)
	}
	finish()
	waitFor(t, func() bool { return s.Status().Active == 0 })
	if got := s.Status().Refresh; got.Total != 1 || got.Active != 0 || got.Unavailable != 1 {
		t.Fatal("released disabled service did not become unavailable", got)
	}
	if snapshot, _ := s.Snapshot("synthetic"); snapshot.RefreshState != "unavailable" {
		t.Fatal("released disabled query retained active row state")
	}
}

func TestRefreshAvailabilityCooldownBoundaryAndChangedGrant(t *testing.T) {
	accounts := []Account{{ID: "short", Revision: "one", CanQuery: true}, {ID: "long", Revision: "one", CanQuery: true}, {ID: "new-grant", Revision: "two", CanQuery: true}}
	s := newRefreshAllQueueFixture(t, accounts)
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	short, long := now.Add(1100*time.Millisecond), now.Add(5*time.Second)
	s.now = func() time.Time { return now }
	s.snapshots["short"] = storedSnapshot{AccountRevision: "one", Snapshot: Snapshot{NextAllowedAt: &short}}
	s.snapshots["long"] = storedSnapshot{AccountRevision: "one", Snapshot: Snapshot{NextAllowedAt: &long}}
	s.snapshots["new-grant"] = storedSnapshot{AccountRevision: "one", BlockedCode: "authorization_expired", Snapshot: Snapshot{NextAllowedAt: &long}}
	got := s.StatusForSelection([]string{"short", "long"})
	if got.Refresh.Eligible != 1 || got.Selection.CoolingDown != 2 || got.Selection.RetryAfterSeconds != 2 || got.Selection.NextAllowedAt == nil || !got.Selection.NextAllowedAt.Equal(short) {
		t.Fatal("cooldown projection rounded down or retained the old grant block", got)
	}
	if snapshot, _ := s.Snapshot("short"); snapshot.RefreshState != "coolingDown" {
		t.Fatal("cooldown did not expose a separate refresh state")
	}
	now = short.Add(-time.Second)
	if got := s.StatusForSelection([]string{"short"}).Selection; got.RetryAfterSeconds != 1 || got.Eligible != 0 {
		t.Fatal("exact remaining second was not represented", got)
	}
	now = short
	if got := s.StatusForSelection([]string{"short"}).Selection; got.Eligible != 1 || got.CoolingDown != 0 || got.NextAllowedAt != nil || got.RetryAfterSeconds != 0 {
		t.Fatal("manual refresh stayed blocked at the cooldown boundary", got)
	}
	if snapshot, _ := s.Snapshot("short"); snapshot.RefreshState != "eligible" || !snapshot.CanQuery || snapshot.UnavailableReason != "" {
		t.Fatal("row and status disagree at the cooldown boundary")
	}
	if receipt, err := s.Refresh([]string{"short"}); err != nil || len(receipt.Accepted) != 1 {
		t.Fatal("normal later manual refresh was prevented", err)
	}
}

func TestRefreshAvailabilityPreservesCompletionCooldowns(t *testing.T) {
	for _, test := range []struct {
		name, outcome string
		wait          time.Duration
	}{
		{"success", "success", 5 * time.Second},
		{"failure", "failed", time.Minute},
	} {
		t.Run(test.name, func(t *testing.T) {
			s, fixture := newFixture(t)
			clock := newReliabilityClock(s)
			if test.outcome == "failed" {
				fixture.query = func(context.Context) protocol.InsightResult { return protocol.InsightResult{} }
			}
			enable(t, s, false)
			reliabilityRefresh(t, s)
			waitResult(t, s, test.outcome)
			got := s.StatusForSelection([]string{"synthetic"}).Selection
			if got.CoolingDown != 1 || got.Eligible != 0 || got.RetryAfterSeconds != int(test.wait/time.Second) || got.NextAllowedAt == nil || !got.NextAllowedAt.Equal(clock.now().Add(test.wait)) {
				t.Fatal("existing completion cooldown changed", got)
			}
			if receipt, err := s.Refresh([]string{"synthetic"}); err != nil || len(receipt.Accepted) != 0 || len(receipt.Skipped) != 1 {
				t.Fatal("cooling query was resubmitted", err)
			}
			clock.advance(test.wait - time.Millisecond)
			if got := s.Status().Refresh; got.RetryAfterSeconds != 1 || got.Eligible != 0 {
				t.Fatal("last cooldown millisecond was rounded away", got)
			}
			clock.advance(time.Millisecond)
			if got := s.Status().Refresh; got.RetryAfterSeconds != 0 || got.Eligible != 1 || got.NextAllowedAt != nil {
				t.Fatal("later manual refresh remained disabled", got)
			}
			reliabilityRefresh(t, s)
		})
	}
}

func TestRefreshAvailabilityQueueCapacityAndQueuedCancellation(t *testing.T) {
	accounts := make([]Account, 1001)
	for i := range accounts {
		accounts[i] = Account{ID: fmt.Sprintf("synthetic-%04d", i), Revision: "one", CanQuery: true}
	}
	s := newRefreshAllQueueFixture(t, accounts)
	for _, a := range accounts[:1000] {
		s.enqueueLocked(a, "synthetic-batch", false)
	}
	got := s.StatusForSelection([]string{accounts[1000].ID})
	if got.Refresh.Total != 1001 || got.Refresh.Queued != 1000 || got.Refresh.Eligible != 0 || got.Refresh.Unavailable != 1 || got.Selection.Unavailable != 1 {
		t.Fatal("full queue advertised new admission", got.Refresh, got.Selection)
	}
	if snapshot, _ := s.Snapshot(accounts[1000].ID); snapshot.RefreshState != "unavailable" || snapshot.CanQuery || snapshot.UnavailableReason != "查询队列已满，请等待现有查询完成" {
		t.Fatal("row ignored queue capacity")
	}
	if !s.Cancel(accounts[0].ID) {
		t.Fatal("queued cancellation failed")
	}
	if got := s.Status().Refresh; got.Eligible != 2 || got.Queued != 999 {
		t.Fatal("queued cancellation retained a manual refresh lock", got)
	}
	if receipt, err := s.Refresh([]string{accounts[0].ID}); err != nil || len(receipt.Accepted) != 1 {
		t.Fatal("explicit manual resubmission after queued cancellation was blocked", err)
	}
}

func TestRefreshStateRemainsActiveUntilCompletedFlightReleases(t *testing.T) {
	s, _ := newFixture(t)
	releasing, released := make(chan struct{}), make(chan struct{})
	var once sync.Once
	finish := func() { once.Do(func() { close(released) }) }
	defer finish()
	acquire := s.provider.Acquire
	s.provider.Acquire = func(ctx context.Context, id string) (*Lease, error) {
		lease, err := acquire(ctx, id)
		if err == nil {
			lease.Release = func() {
				close(releasing)
				<-released
			}
		}
		return lease, err
	}
	enable(t, s, false)
	reliabilityRefresh(t, s)
	select {
	case <-releasing:
	case <-time.After(2 * time.Second):
		t.Fatal("completed query did not reach lease cleanup")
	}
	snapshot, _ := s.Snapshot("synthetic")
	if snapshot.LastAttempt.Status != "success" || snapshot.Usage == nil || snapshot.RefreshState != "active" || snapshot.CanQuery {
		t.Fatal("published terminal outcome prematurely hid a live flight", snapshot.LastAttempt.Status, snapshot.RefreshState)
	}
	finish()
	waitFor(t, func() bool { return s.Status().Active == 0 })
	snapshot, _ = s.Snapshot("synthetic")
	if snapshot.LastAttempt.Status != "success" || snapshot.RefreshState != "coolingDown" {
		t.Fatal("cleanup changed the observation or failed to release live state")
	}
	s.mu.Lock()
	stored := s.snapshots["synthetic"]
	s.mu.Unlock()
	if stored.Snapshot.RefreshState != "" {
		t.Fatal("derived refresh state entered the stored observation")
	}
	var persisted storedSnapshot
	if err := s.read(snapshotFile("synthetic"), &persisted); err != nil || persisted.Snapshot.RefreshState != "" {
		t.Fatal("derived refresh state changed cache persistence", err)
	}
}

func TestRefreshStateIsOptionalAndAlwaysRecomputed(t *testing.T) {
	data, err := json.Marshal(Snapshot{})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil || fields["refreshState"] != nil {
		t.Fatal("unprojected snapshots no longer omit refresh state")
	}
	s := newRefreshAllQueueFixture(t, []Account{{ID: "synthetic", Revision: "one", CanQuery: true}})
	s.snapshots["synthetic"] = storedSnapshot{AccountRevision: "one", Snapshot: Snapshot{RefreshState: "active", LastAttempt: Attempt{Status: "failed"}}}
	snapshot, _ := s.Snapshot("synthetic")
	if snapshot.RefreshState != "eligible" || snapshot.LastAttempt.Status != "failed" {
		t.Fatal("row trusted a stale persisted phase or rewrote observation history")
	}
}

func TestRefreshAvailabilityConcurrentAllAndSelectedRequestsPreserveSingleFlight(t *testing.T) {
	s := newRefreshAllQueueFixture(t, []Account{{ID: "first", Revision: "one", CanQuery: true}, {ID: "second", Revision: "one", CanQuery: true}})
	type response struct {
		receipt Receipt
		err     error
	}
	responses := make(chan response, 32)
	var wg sync.WaitGroup
	for i := 0; i < cap(responses); i++ {
		wg.Add(1)
		go func(all bool) {
			defer wg.Done()
			var r Receipt
			var err error
			if all {
				r, err = s.RefreshAll()
			} else {
				r, err = s.Refresh([]string{"first", "second", "first"})
			}
			responses <- response{r, err}
		}(i%2 == 0)
	}
	wg.Wait()
	close(responses)
	accepted, merged := 0, 0
	for result := range responses {
		if result.err != nil || len(result.receipt.Skipped) != 0 {
			t.Fatal("concurrent valid submission failed", result.err)
		}
		accepted += len(result.receipt.Accepted)
		merged += len(result.receipt.Merged)
	}
	if accepted != 2 || merged != 62 || len(s.flights) != 2 || len(s.pending) != 2 {
		t.Fatalf("concurrent submission duplicated work: accepted=%d merged=%d", accepted, merged)
	}
	if got := s.StatusForSelection([]string{"second"}); got.Refresh.Queued != 2 || got.Selection.Queued != 1 || got.Selection.Eligible != 0 {
		t.Fatal("concurrent queue state was not projected", got)
	}
}
