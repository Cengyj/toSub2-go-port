package accountinsight

import (
	"sort"
	"time"
	"tosub2-go-port/internal/protocol"
)

// Snapshots takes one account inventory per page rather than rescanning every
// account and export for each visible row. It never acquires a grant.
func (s *Service) Snapshots(ids []string) (map[string]Snapshot, uint64) {
	accounts := accountMap(s.provider.Accounts())
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]Snapshot, len(ids))
	for _, id := range ids {
		if a, ok := accounts[id]; ok {
			out[id] = s.snapshotLocked(a)
		}
	}
	return out, s.version
}
func (s *Service) Snapshot(id string) (Snapshot, bool) {
	all, _ := s.Snapshots([]string{id})
	v, ok := all[id]
	return v, ok
}
func (s *Service) snapshotLocked(a Account) Snapshot {
	row := s.snapshots[a.ID]
	snap := clone(row.Snapshot)
	if row.AccountRevision != a.Revision {
		row = storedSnapshot{}
		snap = Snapshot{ID: a.ID, AuthorizationMethod: a.AuthorizationMethod, LastAttempt: Attempt{Status: "never", Message: "当前授权尚未查询"}, Stale: true}
	}
	snap.ID = a.ID
	snap.AuthorizationMethod = a.AuthorizationMethod
	running := s.ctx != nil && !s.closed && s.ctx.Err() == nil
	snap.Automatic = s.settings.Enabled && s.settings.Automatic && !excluded(s.settings, a.ID) && running && a.CanQuery && row.BlockedCode == ""
	snap.CanQuery = s.settings.Enabled && running && a.CanQuery && row.BlockedCode == ""
	snap.UnavailableReason = a.Reason
	if row.BlockedCode != "" {
		snap.UnavailableReason = row.BlockedReason
	}
	if !s.settings.Enabled {
		snap.UnavailableReason = "账号查询功能未启用"
	} else if !running {
		snap.UnavailableReason = "账号查询服务未运行"
	}
	if snap.CanQuery && snap.NextAllowedAt != nil && s.now().Before(*snap.NextAllowedAt) {
		snap.CanQuery = false
		snap.UnavailableReason = "查询冷却中，请稍后重试"
	}
	if snap.LastAttempt.Status == "" {
		snap.LastAttempt.Status = "never"
	}
	if snap.LastSuccessAt == nil || s.now().Sub(*snap.LastSuccessAt) > time.Duration(s.settings.IntervalMinutes)*2*time.Minute {
		snap.Stale = true
	}
	if !snap.Automatic {
		snap.NextCheckAt = nil
	}
	if snap.Errors == nil {
		snap.Errors = []protocol.InsightError{}
	}
	snap.Revision = s.version
	return snap
}
func (s *Service) Status() Status {
	accounts := s.provider.Accounts()
	s.mu.Lock()
	defer s.mu.Unlock()
	out := Status{Enabled: s.settings.Enabled, Automatic: s.settings.Automatic, Running: s.ctx != nil && !s.closed && s.ctx.Err() == nil, Queued: len(s.pending), Active: s.active, LastError: s.lastError, Revision: s.version}
	for _, a := range accounts {
		v := s.snapshotLocked(a)
		switch v.LastAttempt.Status {
		case "success":
			out.Counts.Success++
		case "partial":
			out.Counts.Partial++
		case "failed":
			out.Counts.Failed++
		case "skipped":
			out.Counts.Skipped++
		case "cancelled":
			out.Counts.Cancelled++
		}
		if next := v.NextCheckAt; next != nil && (out.NextCheckAt == nil || next.Before(*out.NextCheckAt)) {
			n := *next
			out.NextCheckAt = &n
		}
	}
	return out
}
func (s *Service) rescheduleLocked() {
	now := s.now().UTC()
	for id, row := range s.snapshots {
		from := now
		if row.Snapshot.LastAttempt.FinishedAt != nil {
			from = *row.Snapshot.LastAttempt.FinishedAt
		}
		next := from.Add(time.Duration(s.settings.IntervalMinutes) * time.Minute)
		if next.Before(now) {
			next = now
		}
		if cooldown := row.Snapshot.NextAllowedAt; cooldown != nil && cooldown.After(next) {
			next = *cooldown
		}
		row.Snapshot.NextCheckAt = &next
		s.snapshots[id] = row
	}
}
func expectedComponents(method string, credits bool) []string {
	list := []string{"usage", "subscription"}
	if method == protocol.AuthorizationMethodBPS {
		list = append(list, "bps")
	}
	if credits {
		list = append(list, "resetCredits")
	}
	return list
}
func componentSkips(snap Snapshot, now time.Time) []string {
	var skipped []string
	for name, c := range snap.Components {
		if c.NextAllowedAt != nil && now.Before(*c.NextAllowedAt) {
			skipped = append(skipped, name)
		}
	}
	sort.Strings(skipped)
	return skipped
}
func componentBackoff(failures int, e protocol.InsightError) time.Duration {
	switch e.Code {
	case "unsupported_authorization":
		return 24 * time.Hour
	case "permission_denied", "authorization_rejected", "invalid_response", "identity_mismatch", "challenge_or_html":
		return time.Hour
	}
	delay := time.Minute
	for i := 1; i < failures && delay < time.Hour; i++ {
		delay *= 2
	}
	if delay > time.Hour {
		delay = time.Hour
	}
	return delay
}
func mergeComponents(snap *Snapshot, result protocol.InsightResult, now time.Time) {
	if snap.Components == nil {
		snap.Components = map[string]ComponentState{}
	}
	attempted := map[string]bool{}
	for _, name := range result.AttemptedComponents {
		attempted[name] = true
	}
	// Keep adapters and isolated tests that return a normalized result usable.
	if result.Usage != nil {
		attempted["usage"] = true
		if result.Usage.ResetCredits != nil {
			attempted["resetCredits"] = true
		}
	}
	if result.BPS != nil {
		attempted["bps"] = true
	}
	if result.Subscription != nil {
		attempted["subscription"] = true
	}
	failures := map[string]protocol.InsightError{}
	for _, e := range result.Errors {
		failures[e.Component] = e
		attempted[e.Component] = true
	}
	for name := range attempted {
		if e, ok := failures[name]; ok {
			c := snap.Components[name]
			c.ConsecutiveFailures++
			if c.ConsecutiveFailures > 16 {
				c.ConsecutiveFailures = 16
			}
			next := now.Add(componentBackoff(c.ConsecutiveFailures, e))
			if e.RetryAfter != nil && e.RetryAfter.After(next) {
				next = *e.RetryAfter
			}
			c.NextAllowedAt = &next
			c.LastError = &e
			snap.Components[name] = c
		} else {
			delete(snap.Components, name)
		}
	}
	snap.Errors = []protocol.InsightError{}
	names := make([]string, 0, len(snap.Components))
	for name := range snap.Components {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if e := snap.Components[name].LastError; e != nil {
			snap.Errors = append(snap.Errors, *e)
		}
	}
}
