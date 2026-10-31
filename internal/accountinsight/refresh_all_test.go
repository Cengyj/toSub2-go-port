package accountinsight

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"
)

func newRefreshAllQueueFixture(t *testing.T, accounts []Account) *Service {
	t.Helper()
	s, err := New(t.TempDir(), Provider{
		Accounts: func() []Account { return accounts },
		Acquire: func(context.Context, string) (*Lease, error) {
			t.Fatal("queue-only test must not query upstream")
			return nil, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Keep admission deterministic without starting the network dispatcher.
	s.ctx, s.cancel = context.WithCancel(t.Context())
	t.Cleanup(s.cancel)
	s.settings.Enabled = true
	s.settings.Automatic = false
	return s
}

func TestRefreshAllAccountsForInventoryBeyondQueueCapacity(t *testing.T) {
	accounts := make([]Account, 1005)
	for i := range accounts {
		accounts[i] = Account{ID: fmt.Sprintf("synthetic-%04d", i), Revision: "one", CanQuery: true}
	}
	// Duplicate provider entries must not consume slots or inflate the receipt.
	accounts = append(accounts, accounts[0], accounts[1004])
	s := newRefreshAllQueueFixture(t, accounts)
	providerCalls := 0
	s.provider.Accounts = func() []Account { providerCalls++; return accounts }
	receipt, err := s.RefreshAll()
	if err != nil {
		t.Fatal(err)
	}
	if providerCalls != 1 || receipt.QueryID == "" || len(receipt.Accepted) != 1000 || len(receipt.Merged) != 0 || len(receipt.Skipped) != 5 {
		t.Fatalf("full inventory not accounted for: calls=%d accepted=%d merged=%d skipped=%d", providerCalls, len(receipt.Accepted), len(receipt.Merged), len(receipt.Skipped))
	}
	seen := make(map[string]bool)
	for i, id := range receipt.Accepted {
		if id != accounts[i].ID || seen[id] {
			t.Fatalf("queue order or deduplication changed at %d: %q", i, id)
		}
		seen[id] = true
	}
	for i, skipped := range receipt.Skipped {
		if skipped.ID != accounts[1000+i].ID || skipped.Reason != "查询队列已满，请等待现有查询完成" || seen[skipped.ID] {
			t.Fatalf("overflow silently omitted or misreported: %+v", skipped)
		}
		seen[skipped.ID] = true
	}
	if len(seen) != 1005 || len(s.flights) != 1000 || len(s.pending) != 1000 {
		t.Fatal("all inventory must be represented without exceeding the existing queue bound")
	}
	second, err := s.RefreshAll()
	if err != nil || len(second.Accepted) != 0 || len(second.Merged) != 1000 || len(second.Skipped) != 5 || len(s.flights) != 1000 {
		t.Fatal("repeat submission bypassed deduplication or queue bounds", err)
	}
}

func TestRefreshAllPreservesManualEligibilityAndAutomaticExclusions(t *testing.T) {
	accounts := []Account{
		{ID: "synthetic-excluded", Revision: "one", CanQuery: true},
		{ID: "synthetic-flight", Revision: "one", CanQuery: true},
		{ID: "synthetic-busy", Revision: "one", Reason: "账号操作进行中，查询暂缓"},
		{ID: "synthetic-blocked", Revision: "one", CanQuery: true},
		{ID: "synthetic-cooldown", Revision: "one", CanQuery: true},
		{ID: "synthetic-new-grant", Revision: "two", CanQuery: true},
	}
	s := newRefreshAllQueueFixture(t, accounts)
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	future := now.Add(time.Hour)
	s.settings.ExcludedAccountIDs = []string{accounts[0].ID}
	s.enqueueLocked(accounts[1], "existing", false)
	s.snapshots[accounts[3].ID] = storedSnapshot{AccountRevision: "one", BlockedCode: "permission_denied", BlockedReason: "授权权限不足"}
	s.snapshots[accounts[4].ID] = storedSnapshot{AccountRevision: "one", Snapshot: Snapshot{NextAllowedAt: &future}}
	s.snapshots[accounts[5].ID] = storedSnapshot{AccountRevision: "one", BlockedCode: "permission_denied", BlockedReason: "旧授权权限不足", Snapshot: Snapshot{NextAllowedAt: &future}}
	receipt, err := s.RefreshAll()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(receipt.Accepted, []string{accounts[0].ID, accounts[5].ID}) || !reflect.DeepEqual(receipt.Merged, []string{accounts[1].ID}) {
		t.Fatalf("manual eligibility or flight deduplication changed: %+v", receipt)
	}
	wantSkipped := []Skip{{accounts[2].ID, accounts[2].Reason}, {accounts[3].ID, "授权权限不足"}, {accounts[4].ID, "查询冷却中，请稍后重试"}}
	if !reflect.DeepEqual(receipt.Skipped, wantSkipped) {
		t.Fatalf("eligibility skips = %+v; want %+v", receipt.Skipped, wantSkipped)
	}
	for _, ticket := range s.pending {
		if ticket.automatic {
			t.Fatal("manual all-account submission must preserve manual query semantics")
		}
	}
}

func TestRefreshAllPreservesLifecycleGates(t *testing.T) {
	for _, test := range []struct {
		name    string
		prepare func(*Service)
		want    error
	}{
		{"disabled", func(s *Service) { s.settings.Enabled = false }, ErrDisabled},
		{"closed", func(s *Service) { s.closed = true }, ErrClosed},
		{"not-started", func(s *Service) { s.ctx = nil }, ErrClosed},
		{"cancelled", func(s *Service) { s.cancel() }, ErrClosed},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := newRefreshAllQueueFixture(t, nil)
			test.prepare(s)
			receipt, err := s.RefreshAll()
			if !errors.Is(err, test.want) || receipt.QueryID != "" || len(s.flights) != 0 {
				t.Fatalf("lifecycle gate bypassed: receipt=%+v error=%v", receipt, err)
			}
		})
	}
}

func TestRefreshAllEmptyInventoryDoesNotChangeSelectedIDContract(t *testing.T) {
	s := newRefreshAllQueueFixture(t, nil)
	receipt, err := s.RefreshAll()
	if err != nil || receipt.QueryID == "" || receipt.Accepted == nil || receipt.Merged == nil || receipt.Skipped == nil || len(s.flights) != 0 {
		t.Fatalf("empty all-account inventory is not a valid empty receipt: %+v %v", receipt, err)
	}
	for _, ids := range [][]string{nil, {}, make([]string, 1001)} {
		if _, err := s.Refresh(ids); err == nil {
			t.Fatalf("selected-ID request unexpectedly accepted %d IDs", len(ids))
		}
	}
	if len(s.flights) != 0 {
		t.Fatal("invalid selected-ID request queued work")
	}
}
