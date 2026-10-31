package accountinsight

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestInsightAutomaticQueueEventuallyAdmitsOldestUnqueriedAccount(t *testing.T) {
	accounts := make([]Account, 1001)
	for i := range accounts {
		accounts[i] = Account{ID: fmt.Sprintf("synthetic-%04d", i), Revision: "one", AuthorizationMethod: "bps", CanQuery: true}
	}
	s, err := New(t.TempDir(), Provider{Accounts: func() []Account { return accounts }, Acquire: func(context.Context, string) (*Lease, error) {
		t.Fatal("no query should be admitted by this queue-only test")
		return nil, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	s.ctx, s.cancel = context.WithCancel(context.Background())
	defer s.cancel()
	s.settings = DefaultSettings()
	s.settings.Enabled = true
	s.settings.Automatic = true
	s.settings.IntervalMinutes = 1
	s.active = 2 // Both transport slots occupied: exercise queue order without network.
	now := time.Now().UTC()
	s.now = func() time.Time { return now }
	past := now.Add(-time.Minute)
	for _, a := range accounts {
		s.snapshots[a.ID] = storedSnapshot{SchemaVersion: 1, AccountRevision: a.Revision, Snapshot: Snapshot{ID: a.ID, NextCheckAt: &past, LastAttempt: Attempt{Status: "never"}}}
	}
	s.dispatch()
	if len(s.pending) != 1000 || s.flights[accounts[1000].ID] != nil {
		t.Fatal("expected one account behind the bounded queue")
	}
	// The first account has finished and is due again after its one-minute
	// interval. The account that never got a slot must still go first.
	first := accounts[0].ID
	delete(s.flights, first)
	s.pending = s.pending[1:]
	now = now.Add(2 * time.Minute)
	dueAgain := now.Add(-time.Minute)
	row := s.snapshots[first]
	row.Snapshot.LastAttempt.Status = "success"
	row.Snapshot.NextCheckAt = &dueAgain
	s.snapshots[first] = row
	s.dispatch()
	if s.flights[accounts[1000].ID] == nil || s.flights[first] != nil || len(s.pending) != 1000 {
		t.Fatal("oldest unqueried account was starved by a repeatedly due account")
	}
}
