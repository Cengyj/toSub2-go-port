package accountquality

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestStaleReasonSeparatesExpiryFromIndependentOptionsAndNeverChangesTheGate(t *testing.T) {
	for _, tc := range []struct {
		name, want                       string
		expired, options, auth, inactive bool
		oldRule, unmeasured              bool
	}{
		{name: "fresh"},
		{name: "unmeasured", unmeasured: true},
		{name: "independent-options", options: true, want: "options_changed"},
		{name: "old-rules", options: true, oldRule: true, want: "rule_changed"},
		{name: "authorization-before-options", auth: true, options: true, want: "authorization_changed"},
		{name: "account-before-other-identity", inactive: true, auth: true, options: true, oldRule: true, want: "account_unavailable"},
		{name: "expired", expired: true, want: "expired"},
		{name: "expiry-before-all-other-causes", expired: true, inactive: true, auth: true, options: true, oldRule: true, want: "expired"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, f, _ := newFixture(t)
			settings, err := s.Settings()
			if err != nil {
				t.Fatal(err)
			}
			result := resultFor(options(settings), 1)
			result.CheckedAt = time.Now().Add(-time.Second).UTC()
			if tc.expired {
				result.CheckedAt = time.Now().Add(-2 * time.Hour).UTC()
			}
			if tc.oldRule {
				result.RuleVersion, result.Total, result.Score = "reasoning-six-v1", 6, 6
			}
			row := storedRow{SchemaVersion: 1, AccountRevision: "r1", OptionsHash: optionsHash(settings), Snapshot: Snapshot{ID: "a", LastResult: &result, StaleReason: "untrusted-stored-reason", Persisted: true, LastAttempt: Attempt{Status: "success"}}}
			if tc.unmeasured {
				row.Snapshot.LastResult = nil
			}
			if tc.options {
				row.OptionsHash = digest("different-execution-options")
			}
			if tc.auth {
				row.AccountRevision = "older-authorization"
			}
			if tc.inactive {
				f.mu.Lock()
				f.accounts = nil
				f.mu.Unlock()
			}
			s.mu.Lock()
			s.rows["a"] = row
			s.mu.Unlock()
			rows, _ := s.Snapshots([]string{"a"})
			snapshot := rows["a"]
			wantStale := tc.expired || tc.options || tc.auth || tc.inactive || tc.unmeasured
			if snapshot.Stale != wantStale || snapshot.StaleReason != tc.want {
				t.Fatalf("projection stale=%t reason=%q; want stale=%t reason=%q", snapshot.Stale, snapshot.StaleReason, wantStale, tc.want)
			}
			data, err := json.Marshal(snapshot)
			if err != nil || (tc.want == "" && strings.Contains(string(data), "staleReason")) {
				t.Fatal("empty projection reason was not optional in JSON", err)
			}
			s.mu.Lock()
			stored := s.rows["a"].Snapshot.StaleReason
			s.mu.Unlock()
			if stored != "untrusted-stored-reason" {
				t.Fatal("read-only projection modified stored snapshot data")
			}
		})
	}
}
