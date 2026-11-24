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
	return s.snapshotLockedAt(a, s.now().UTC())
}
func (s *Service) snapshotLockedAt(a Account, now time.Time) Snapshot {
	row := s.snapshots[a.ID]
	snap := clone(row.Snapshot)
	if row.AccountRevision != a.Revision {
		row = storedSnapshot{}
		snap = Snapshot{ID: a.ID, AuthorizationMethod: a.AuthorizationMethod, LastAttempt: Attempt{Status: "never", Message: "当前授权尚未查询"}, Stale: true}
	}
	snap.ID = a.ID
	snap.AuthorizationMethod = a.AuthorizationMethod
	running := s.ctx != nil && !s.closed && s.ctx.Err() == nil
	snap.Automatic = s.settings.Enabled && s.settings.Automatic && !s.automaticPaused && !excluded(s.settings, a.ID) && running && a.CanQuery && row.BlockedCode == ""
	state, reason, _ := s.refreshStateLocked(a, now)
	snap.RefreshState = state
	snap.CanQuery, snap.UnavailableReason = state == "eligible", reason
	if snap.LastAttempt.Status == "" {
		snap.LastAttempt.Status = "never"
	}
	if snap.LastSuccessAt == nil || now.Sub(*snap.LastSuccessAt) > time.Duration(s.settings.IntervalMinutes)*2*time.Minute {
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
	return s.status(nil, false)
}

// StatusForSelection returns global and selected availability from one account
// inventory, one clock reading and one service-lock snapshot. Unknown selected
// accounts remain visible as unavailable; duplicate IDs count only once.
func (s *Service) StatusForSelection(ids []string) Status {
	return s.status(ids, true)
}

func (s *Service) status(ids []string, selected bool) Status {
	accounts := s.provider.Accounts()
	byID := accountMap(accounts)
	allIDs := make([]string, 0, len(byID))
	for id := range byID {
		allIDs = append(allIDs, id)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now().UTC()
	out := Status{Enabled: s.settings.Enabled, Automatic: s.settings.Automatic, Running: s.ctx != nil && !s.closed && s.ctx.Err() == nil, Queued: len(s.pending), Active: s.active, LastError: s.lastError, Revision: s.version}
	out.Refresh = s.refreshAvailabilityLocked(byID, allIDs, now)
	if selected {
		selection := s.refreshAvailabilityLocked(byID, ids, now)
		out.Selection = &selection
	}
	for _, a := range byID {
		v := s.snapshotLockedAt(a, now)
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

// flights owns live work until its runner releases the account reservation.
// LastAttempt may already be cancelled, or reset after a grant revision change,
// while that reservation still prevents another query from being admitted.
func (s *Service) refreshStateLocked(a Account, now time.Time) (state, reason string, next *time.Time) {
	if flight := s.flights[a.ID]; flight != nil {
		if flight.cancel != nil {
			return "active", "查询正在进行，请等待完成", nil
		}
		return "queued", "查询已排队，请等待完成", nil
	}
	if !s.settings.Enabled {
		return "unavailable", "账号查询功能未启用", nil
	}
	if s.ctx == nil || s.closed || s.ctx.Err() != nil {
		return "unavailable", "账号查询服务未运行", nil
	}
	if !a.CanQuery {
		reason := a.Reason
		if reason == "" {
			reason = "当前账号暂不可查询"
		}
		return "unavailable", reason, nil
	}
	if len(s.flights) >= 1000 {
		return "unavailable", "查询队列已满，请等待现有查询完成", nil
	}
	row := s.snapshots[a.ID]
	if row.AccountRevision == a.Revision {
		if row.BlockedCode != "" {
			reason := row.BlockedReason
			if reason == "" {
				reason = "当前授权暂不可查询"
			}
			return "unavailable", reason, nil
		}
		if at := row.Snapshot.NextAllowedAt; at != nil && now.Before(*at) {
			copy := *at
			return "coolingDown", "查询冷却中，请稍后重试", &copy
		}
	}
	return "eligible", "", nil
}

func (s *Service) refreshAvailabilityLocked(accounts map[string]Account, ids []string, now time.Time) RefreshAvailability {
	out := RefreshAvailability{}
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		out.Total++
		a, ok := accounts[id]
		if !ok {
			out.Unavailable++
			continue
		}
		state, _, next := s.refreshStateLocked(a, now)
		switch state {
		case "eligible":
			out.Eligible++
		case "queued":
			out.Queued++
		case "active":
			out.Active++
		case "coolingDown":
			out.CoolingDown++
			if next != nil && (out.NextAllowedAt == nil || next.Before(*out.NextAllowedAt)) {
				out.NextAllowedAt = next
			}
		default:
			out.Unavailable++
		}
	}
	if out.NextAllowedAt != nil {
		wait := out.NextAllowedAt.Sub(now)
		out.RetryAfterSeconds = int(wait / time.Second)
		if wait%time.Second != 0 {
			out.RetryAfterSeconds++
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
