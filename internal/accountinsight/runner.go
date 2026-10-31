package accountinsight

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"
	"tosub2-go-port/internal/protocol"
)

// QueryError is deliberately safe: adapters map private errors into closed codes.
type QueryError struct{ Code, Message string }

func (e *QueryError) Error() string { return e.Message }
func (s *Service) dispatch() {
	s.mu.Lock()
	if s.closed || s.ctx == nil || s.ctx.Err() != nil || !s.settings.Enabled || !s.settings.Automatic && len(s.pending) == 0 {
		s.mu.Unlock()
		return
	}
	automatic := s.settings.Automatic
	s.mu.Unlock()
	var accounts []Account
	if automatic {
		accounts = s.provider.Accounts()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.ctx.Err() != nil || !s.settings.Enabled {
		return
	}
	now := s.now().UTC()
	if s.settings.Automatic {
		due := make([]Account, 0, len(accounts))
		for _, a := range accounts {
			if !a.CanQuery || excluded(s.settings, a.ID) || s.flights[a.ID] != nil {
				continue
			}
			row, exists := s.snapshots[a.ID]
			if !exists || row.AccountRevision != a.Revision {
				// Spread new and restored identities; a browser refresh never starts a query.
				sum := snapshotFile(a.ID)
				delay := time.Duration(int(sum[0])%20) * time.Second
				next := now.Add(delay)
				s.snapshots[a.ID] = storedSnapshot{SchemaVersion: 1, AccountRevision: a.Revision, Snapshot: Snapshot{ID: a.ID, AuthorizationMethod: a.AuthorizationMethod, NextCheckAt: &next, LastAttempt: Attempt{Status: "never"}}}
				continue
			}
			if row.BlockedCode != "" {
				continue
			}
			if row.Snapshot.NextAllowedAt != nil && now.Before(*row.Snapshot.NextAllowedAt) {
				continue
			}
			if row.Snapshot.NextCheckAt != nil && now.Before(*row.Snapshot.NextCheckAt) {
				continue
			}
			due = append(due, a)
		}
		// An account left behind by the bounded queue remains older than
		// refreshed accounts, even when the configured interval is shorter
		// than the time needed to sweep the entire inventory.
		sort.Slice(due, func(i, j int) bool {
			a, b := s.snapshots[due[i].ID].Snapshot.NextCheckAt, s.snapshots[due[j].ID].Snapshot.NextCheckAt
			if a == nil && b != nil {
				return true
			}
			if b == nil && a != nil {
				return false
			}
			if a != nil && b != nil && !a.Equal(*b) {
				return a.Before(*b)
			}
			return due[i].ID < due[j].ID
		})
		for _, a := range due {
			if len(s.flights) >= 1000 {
				break
			}
			s.sequence++
			s.enqueueLocked(a, fmt.Sprintf("auto-%d-%d", now.UnixNano(), s.sequence), true)
		}
	}
	// Wakes must respect the same admission rate as timer ticks.
	if s.active >= 2 || len(s.pending) == 0 || now.Before(s.nextAdmission) {
		return
	}
	t := s.pending[0]
	s.pending = s.pending[1:]
	ctx, cancel := context.WithTimeout(s.ctx, s.budget)
	t.cancel = cancel
	s.active++
	s.nextAdmission = now.Add(s.admissionInterval)
	row := s.snapshots[t.id]
	row.Snapshot.LastAttempt.Status = "running"
	row.Snapshot.LastAttempt.StartedAt = &now
	s.snapshots[t.id] = row
	cfg := clone(s.settings)
	s.version++
	s.wg.Add(1)
	go s.run(ctx, t, cfg, row.AccountRevision)
}
func (s *Service) run(ctx context.Context, t *ticket, cfg Settings, accountRevision string) {
	defer s.wg.Done()
	defer t.cancel()
	defer func() {
		s.mu.Lock()
		if s.flights[t.id] == t {
			delete(s.flights, t.id)
		}
		s.active--
		s.version++
		s.mu.Unlock()
	}()
	var lease *Lease
	released := false
	release := func() {
		if lease != nil && !released && lease.Release != nil {
			released = true
			lease.Release()
		}
	}
	defer release()
	finished := false
	commitCtx := ctx
	var validate func() error
	defer func() {
		if recover() != nil && !finished {
			s.complete(commitCtx, t, cfg, accountRevision, protocol.InsightResult{}, "query_internal", "查询未完成，请稍后重试", validate)
		}
	}()
	var err error
	lease, err = s.provider.Acquire(ctx, t.id)
	if err != nil {
		code, message := "query_unavailable", "当前授权或账号操作暂不允许查询"
		var safe *QueryError
		if errors.As(err, &safe) {
			code, message = safe.Code, safe.Message
		}
		if ctx.Err() != nil {
			code, message = "cancelled", "查询已取消"
		}
		s.complete(ctx, t, cfg, accountRevision, protocol.InsightResult{}, code, message, nil)
		finished = true
		return
	}
	if lease == nil || lease.Query == nil || lease.Validate == nil || lease.Context == nil {
		s.complete(ctx, t, cfg, accountRevision, protocol.InsightResult{}, "query_unavailable", "查询会话不可用", nil)
		finished = true
		return
	}
	commitCtx, validate = lease.Context, lease.Validate
	zone := "America/Los_Angeles"
	if s.provider.Timezone != nil {
		if z := s.provider.Timezone(); z != "" {
			zone = z
		}
	}
	s.mu.Lock()
	skipped := componentSkips(s.snapshots[t.id].Snapshot, s.now().UTC())
	s.mu.Unlock()
	result := lease.Query(lease.Context, protocol.InsightsOptions{IncludeResetCredits: cfg.IncludeResetCredits, Timezone: zone, SkipComponents: skipped})
	code, message := "", ""
	if ctx.Err() != nil || lease.Context.Err() != nil || lease.Validate() != nil {
		result = protocol.InsightResult{}
		code, message = "cancelled", "账号操作或授权已改变，本次查询结果已丢弃"
	}
	if current, ok := accountMap(s.provider.Accounts())[t.id]; !ok || current.Revision != accountRevision {
		result = protocol.InsightResult{}
		code, message = "superseded", "账号或授权已改变，本次查询结果已丢弃"
	}
	s.complete(commitCtx, t, cfg, accountRevision, result, code, message, validate)
	finished = true
}
func (s *Service) complete(ctx context.Context, t *ticket, cfg Settings, accountRevision string, result protocol.InsightResult, code, message string, validate func() error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	valid := func() bool {
		if ctx.Err() != nil || validate != nil && validate() != nil {
			return false
		}
		a, ok := accountMap(s.provider.Accounts())[t.id]
		return ok && a.Revision == accountRevision
	}
	if !valid() {
		code, message = "superseded", "账号操作或授权已改变，本次查询结果已丢弃"
		result = protocol.InsightResult{}
	}
	s.mu.Lock()
	row := clone(s.snapshots[t.id])
	now := s.now().UTC()
	if s.closed || ctx.Err() != nil || s.settings.Revision != t.revision || !s.settings.Enabled {
		code, message = "cancelled", "查询已取消或配置已变更"
		result = protocol.InsightResult{}
		if !s.closed && s.settings.Revision == t.revision && s.settings.Enabled && errors.Is(ctx.Err(), context.DeadlineExceeded) {
			code, message = "query_timeout", "本次查询超时，历史结果保留，请稍后重试"
		}
	}
	if row.Snapshot.LastAttempt.QueryID != t.queryID {
		s.mu.Unlock()
		return
	}
	snap := &row.Snapshot
	snap.LastAttempt.FinishedAt = &now
	snap.LastAttempt.ErrorCode = code
	snap.LastAttempt.Message = message
	if code == "authorization_expired" || code == "identity_mismatch" {
		row.BlockedCode, row.BlockedReason = code, message
	}
	if code == "" {
		mergeComponents(snap, result, now)
	}
	successful := result.Usage != nil || result.BPS != nil || result.Subscription != nil
	switch {
	case code == "cancelled" || code == "superseded":
		snap.LastAttempt.Status = "cancelled"
	case code == "query_timeout" || code == "query_internal":
		snap.LastAttempt.Status = "failed"
	case code != "":
		snap.LastAttempt.Status = "skipped"
	case len(snap.Errors) > 0 && successful:
		snap.LastAttempt.Status = "partial"
		snap.LastAttempt.Message = "部分信息已更新，请查看各来源结果"
	case len(snap.Errors) > 0:
		snap.LastAttempt.Status = "failed"
		snap.LastAttempt.ErrorCode = snap.Errors[0].Code
		snap.LastAttempt.Message = snap.Errors[0].Message
	case successful:
		snap.LastAttempt.Status = "success"
		snap.LastAttempt.Message = "查询结果已更新"
	default:
		snap.LastAttempt.Status = "failed"
		snap.LastAttempt.ErrorCode = "empty_response"
		snap.LastAttempt.Message = "未取得有效查询信息"
	}
	if result.Usage != nil {
		// A successful usage read does not make an omitted or failed voucher
		// request authoritative. Keep the old voucher's original observation time.
		resetSucceeded := false
		for _, name := range result.AttemptedComponents {
			if name == "resetCredits" {
				resetSucceeded = true
			}
		}
		for _, e := range result.Errors {
			if e.Component == "resetCredits" {
				resetSucceeded = false
			}
		}
		if result.Usage.ResetCredits == nil && !resetSucceeded && snap.Usage != nil {
			result.Usage.ResetCredits = snap.Usage.ResetCredits
		}
		snap.Usage = result.Usage
	}
	if result.BPS != nil {
		snap.BPS = result.BPS
	}
	if result.Subscription != nil {
		snap.Subscription = mergeSubscriptionObservation(snap.Subscription, result.Subscription, result.Errors)
	}
	if successful {
		snap.LastSuccessAt = &now
		snap.Stale = false
	}
	delay := time.Duration(cfg.IntervalMinutes) * time.Minute
	cooldown := now.Add(5 * time.Second)
	if snap.LastAttempt.Status == "failed" || snap.LastAttempt.Status == "skipped" {
		if cooldown.Before(now.Add(time.Minute)) {
			cooldown = now.Add(time.Minute)
		}
	}
	// Only block the whole account when every relevant component is cooling.
	var earliest *time.Time
	allCooling := true
	for _, name := range expectedComponents(snap.AuthorizationMethod, false) {
		c := snap.Components[name]
		if c.NextAllowedAt == nil || !c.NextAllowedAt.After(now) {
			allCooling = false
			break
		}
		if earliest == nil || c.NextAllowedAt.Before(*earliest) {
			earliest = c.NextAllowedAt
		}
	}
	if allCooling && earliest != nil && earliest.After(cooldown) {
		cooldown = *earliest
	}
	next := now.Add(delay)
	if cooldown.After(next) {
		next = cooldown
	}
	snap.NextCheckAt = &next
	snap.NextAllowedAt = &cooldown
	snap.Persisted = true
	s.version++
	snap.Revision = s.version
	row.AccountRevision = accountRevision
	s.mu.Unlock()
	// Canceled reads never replace an already saved observation on disk.
	var err error
	if code == "cancelled" || code == "superseded" || !valid() {
		snap.Persisted = false
	} else {
		err = s.persist(ctx, snapshotFile(t.id), row)
	}
	stillValid := valid()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.snapshots[t.id].Snapshot.LastAttempt.QueryID != t.queryID {
		return
	}
	if err != nil {
		snap.Persisted = false
		s.lastError = "查询已完成，但结果未可靠保存"
	}
	if s.closed || !stillValid || s.settings.Revision != t.revision || code == "cancelled" || code == "superseded" {
		old := s.snapshots[t.id]
		old.Snapshot.LastAttempt.Status = "cancelled"
		old.Snapshot.LastAttempt.FinishedAt = &now
		old.Snapshot.LastAttempt.Message = "查询已取消，未发布新结果"
		old.Snapshot.LastAttempt.ErrorCode = "cancelled"
		if !s.closed && s.settings.Revision == t.revision && s.settings.Enabled && errors.Is(ctx.Err(), context.DeadlineExceeded) {
			old.Snapshot.LastAttempt.Status = "failed"
			old.Snapshot.LastAttempt.ErrorCode = "query_timeout"
			old.Snapshot.LastAttempt.Message = "本次查询超时，历史结果保留，请稍后重试"
		}
		old.Snapshot.Persisted = false
		next := now.Add(time.Duration(s.settings.IntervalMinutes) * time.Minute)
		old.Snapshot.NextCheckAt = &next
		old.Snapshot.NextAllowedAt = &cooldown
		s.snapshots[t.id] = old
		return
	}
	s.snapshots[t.id] = row
}
