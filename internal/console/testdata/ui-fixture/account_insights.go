package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"tosub2-go-port/internal/accountinsight"
	"tosub2-go-port/internal/protocol"
)

// This fixture owns synthetic, memory-only data. It never constructs an
// InsightsClient or calls authentication, account-info, or upstream services.
// Callers hold the fixture server mutex for every method.
type insightFixture struct {
	settings        accountinsight.Settings
	snapshots       map[string]accountinsight.Snapshot
	results         map[string]accountinsight.Snapshot
	pending         map[string]time.Time
	hold            bool
	statusError     bool
	responseLost    bool
	rowStages       map[string]string
	bpsOnly         bool
	layout          string
	sequence        uint64
	settingsOutcome string
	requests        []map[string]any
}

func fixturePointer[T any](value T) *T { return &value }

func newInsightFixture() *insightFixture {
	now := time.Now().UTC()
	old := now.Add(-48 * time.Hour)
	settings := accountinsight.DefaultSettings()
	settings.Revision = 1
	f := &insightFixture{settings: settings, snapshots: map[string]accountinsight.Snapshot{}, results: map[string]accountinsight.Snapshot{}, pending: map[string]time.Time{}, layout: "default", settingsOutcome: "success", requests: []map[string]any{}}
	for index, method := range []string{"chatgpt", "chatgpt", "bps", "bps", "chatgpt", "chatgpt"} {
		id := []string{"fixture-1", "fixture-2", "fixture-3", "fixture-4", "fixture-5", "fixture-6"}[index]
		s := accountinsight.Snapshot{ID: id, AuthorizationMethod: method, CanQuery: index == 0 || index == 2, Automatic: true, Persisted: true, Revision: 1, LastAttempt: accountinsight.Attempt{Status: "never"}, InsightResult: protocol.InsightResult{Errors: []protocol.InsightError{}}}
		if index != 1 {
			s.LastAttempt.StartedAt, s.LastAttempt.FinishedAt = &now, &now
		}
		switch index {
		case 0:
			s.LastAttempt.Status = "success"
			s.Usage = &protocol.InsightUsage{Source: "wham", ObservedAt: now, PlanType: "business_premium", Allowed: fixturePointer(true), LimitReached: fixturePointer(false), Windows: []protocol.InsightWindow{{Name: "primary", WindowSeconds: fixturePointer(int64(18000)), UsedPercent: fixturePointer(0.0), ResetsAt: fixturePointer(now.Add(2 * time.Hour))}, {Name: "secondary", WindowSeconds: fixturePointer(int64(604800)), UsedPercent: fixturePointer(61.25), ResetsAt: fixturePointer(now.Add(3 * 24 * time.Hour))}}, Credits: &protocol.InsightCredits{HasCredits: fixturePointer(true), Unlimited: fixturePointer(false), Balance: fixturePointer("12.50")}, ResetCredits: &protocol.InsightResetCredits{AvailableCount: fixturePointer(int64(2)), ObservedAt: now, Credits: []protocol.InsightResetCredit{{ExpiresAt: fixturePointer(now.Add(-time.Hour))}, {ExpiresAt: fixturePointer(now.Add(14 * 24 * time.Hour))}}}}
			s.Subscription = &protocol.InsightSubscription{Source: "subscriptions", ObservedAt: now, PlanType: "business_premium", ExpiresAt: fixturePointer(now.Add(40 * 24 * time.Hour).Format(time.RFC3339)), Reported: true, IdentityVerified: true, AccountID: "synthetic-account-1", Scope: "account"}
		case 1:
			s.UnavailableReason = "尚无已保存授权，不会自动登录"
		case 2:
			s.LastAttempt.Status, s.LastAttempt.Message = "partial", "BPS 权限已更新；Codex 额度接口暂不可读，保留旧观察"
			s.Usage = &protocol.InsightUsage{Source: "wham", ObservedAt: old, Windows: []protocol.InsightWindow{{Name: "primary", WindowSeconds: fixturePointer(int64(18000)), UsedPercent: fixturePointer(35.0), ResetsAt: fixturePointer(now.Add(-time.Hour))}, {Name: "secondary", WindowSeconds: fixturePointer(int64(604800))}}}
			s.BPS = &protocol.InsightBPS{Source: "bps_access", ObservedAt: now, Allowed: fixturePointer(true), Models: []protocol.InsightBPSModel{{ID: "synthetic-reasoning-model", Label: "Synthetic reasoning model with a deliberately long label", Available: fixturePointer(true), ReasoningEfforts: []string{"low", "high"}}, {ID: "synthetic-restricted", Available: fixturePointer(false), Restricted: true}}}
			s.Subscription = &protocol.InsightSubscription{Source: "subscriptions", ObservedAt: now, PlanType: "prolite", Reported: true, IdentityVerified: true, AccountID: "synthetic-account-3", Scope: "account"}
			s.Errors = []protocol.InsightError{{Component: "usage", Code: "permission_denied", Message: "合成额度接口拒绝访问；BPS 权限可用不代表存在独立余额", HTTPStatus: 403}}
		case 3:
			s.LastAttempt.Status, s.LastAttempt.Message, s.UnavailableReason = "skipped", "2FA 安全操作尚未完成，未发起查询", "2FA 安全操作尚未完成，暂不可查询"
			s.BPS = &protocol.InsightBPS{Source: "bps_access", ObservedAt: old, Models: []protocol.InsightBPSModel{{ID: "synthetic-unknown-model"}}}
			s.Stale = true
		case 4:
			s.LastAttempt.Status, s.LastAttempt.ErrorCode, s.LastAttempt.Message = "failed", "timeout", "合成来源超时，原有授权仍可用；本次未更新结果"
			s.UnavailableReason, s.Persisted, s.Stale = "2FA 修改结果未确认，需人工处理", false, true
			s.Errors = []protocol.InsightError{{Component: "query", Code: "timeout", Message: strings.Repeat("Synthetic source timeout; historical authorization is preserved. ", 4)}}
		case 5:
			s.LastAttempt.Status, s.LastAttempt.ErrorCode = "skipped", "authorization_expired"
			s.LastAttempt.Message, s.UnavailableReason = "当前 access token 已过期，请先刷新授权；查询不会自动登录", "当前 access token 已过期，请先刷新授权"
			s.Usage = &protocol.InsightUsage{Source: "wham", ObservedAt: old, Allowed: fixturePointer(false), LimitReached: fixturePointer(true), Windows: []protocol.InsightWindow{{Name: "primary", WindowSeconds: fixturePointer(int64(18000)), UsedPercent: fixturePointer(100.0), ResetsAt: fixturePointer(now.Add(-time.Hour))}}}
			s.Stale = true
		}
		f.snapshots[id], f.results[id] = s, s
	}
	return f
}

func (f *insightFixture) configure(q url.Values) {
	if q.Has("insightExhausted") {
		now := time.Now().UTC()
		exhausted := q.Get("insightExhausted") == "true"
		s := f.snapshots["fixture-1"]
		s.LastAttempt = accountinsight.Attempt{Status: "success", StartedAt: &now, FinishedAt: &now}
		s.Usage.ObservedAt = now
		s.Usage.LimitReached, s.Usage.Allowed = fixturePointer(exhausted), fixturePointer(!exhausted)
		percent := 0.0
		if exhausted {
			percent = 100
		}
		s.Usage.Windows[0].UsedPercent = fixturePointer(percent)
		s.Usage.Windows[0].ResetsAt = fixturePointer(now.Add(2 * time.Hour))
		f.snapshots[s.ID], f.results[s.ID] = s, s
	}
	if q.Get("insightErrors") == "true" {
		now := time.Now().UTC()
		cooldown := now.Add(time.Hour)
		bps := f.snapshots["fixture-3"]
		bps.LastAttempt = accountinsight.Attempt{Status: "partial", StartedAt: &now, FinishedAt: &now, Message: "合成订阅信息已更新；额度与 BPS 权限查询被拒绝，保留旧观察"}
		bps.BPS.ObservedAt = now.Add(-48 * time.Hour)
		bps.Errors = []protocol.InsightError{
			{Component: "usage", Code: "upstream_error", HTTPStatus: 402, Message: "查询接口返回 HTTP 402，未取得该来源数据"},
			{Component: "bps", Code: "upstream_error", HTTPStatus: 402, Message: "查询接口返回 HTTP 402，未取得该来源数据"},
		}
		bps.Components = map[string]accountinsight.ComponentState{}
		for _, failure := range bps.Errors {
			err := failure
			bps.Components[err.Component] = accountinsight.ComponentState{NextAllowedAt: &cooldown, ConsecutiveFailures: 1, LastError: &err}
		}
		f.snapshots[bps.ID], f.results[bps.ID] = bps, bps
		chatgpt := f.snapshots["fixture-5"]
		chatgpt.LastAttempt = accountinsight.Attempt{Status: "failed", StartedAt: &now, FinishedAt: &now, ErrorCode: "authorization_rejected", Message: "合成查询接口拒绝当前授权；本地历史授权记录仍保留"}
		chatgpt.Errors = []protocol.InsightError{
			{Component: "usage", Code: "authorization_rejected", HTTPStatus: 401, Message: "当前授权被查询接口拒绝", UpstreamCode: "invalid_token", UpstreamType: "authentication_error"},
			{Component: "subscription", Code: "authorization_rejected", HTTPStatus: 401, Message: "当前授权被查询接口拒绝"},
		}
		f.snapshots[chatgpt.ID], f.results[chatgpt.ID] = chatgpt, chatgpt
	}
	if q.Has("insightEnabled") {
		f.settings.Enabled = q.Get("insightEnabled") == "true"
		f.settings.Revision++
	}
	if q.Has("insightAutomatic") {
		f.settings.Automatic = q.Get("insightAutomatic") == "true"
		f.settings.Revision++
	}
	if q.Has("insightHold") {
		f.hold = q.Get("insightHold") == "true"
	}
	if q.Has("insightStatusError") {
		f.statusError = q.Get("insightStatusError") == "true"
	}
	if q.Has("insightResponseLost") {
		f.responseLost = q.Get("insightResponseLost") == "true"
	}
	if q.Has("insightSettings") {
		f.settingsOutcome = q.Get("insightSettings")
	}
	if q.Has("insightBPSOnly") {
		f.bpsOnly = q.Get("insightBPSOnly") == "true"
	}
	if q.Has("insightLayout") {
		switch layout := q.Get("insightLayout"); layout {
		case "default", "unknown", "long", "undated", "countdowns", "pending", "dense", "weekly", "weekly-error":
			f.layout = layout
		}
	}
	for parameter, id := range map[string]string{"insightRow1": "fixture-1", "insightRow3": "fixture-3"} {
		if q.Has(parameter) {
			f.setRowStage(id, q.Get(parameter))
		}
	}
}

func (f *insightFixture) state() map[string]any {
	return map[string]any{"settings": f.settings, "pending": len(f.pending), "hold": f.hold, "statusError": f.statusError, "responseLost": f.responseLost, "rowStages": f.rowStages, "bpsOnly": f.bpsOnly, "layout": f.layout, "settingsOutcome": f.settingsOutcome, "requests": f.requests}
}

// Explicit visual controls pin only the selected synthetic row. Normal query
// timing and every other fixture row stay unchanged until a control is used.
func (f *insightFixture) setRowStage(id, stage string) {
	if stage != "queued" && stage != "running" && stage != "terminal" && stage != "idle" {
		return
	}
	if f.rowStages == nil {
		f.rowStages = map[string]string{}
	}
	now := time.Now().UTC()
	if stage == "idle" {
		delete(f.pending, id)
		delete(f.rowStages, id)
		s := f.results[id]
		s.Revision = f.snapshots[id].Revision + 1
		f.snapshots[id] = s
		return
	}
	s := f.snapshots[id]
	if _, pending := f.pending[id]; !pending {
		f.sequence++
		s.LastAttempt = accountinsight.Attempt{QueryID: fmt.Sprintf("synthetic-row-query-%d", f.sequence)}
	}
	s.NextAllowedAt = nil
	s.Revision++
	if stage == "running" && s.LastAttempt.StartedAt == nil {
		s.LastAttempt.StartedAt = &now
	}
	s.LastAttempt.Status = stage
	s.LastAttempt.FinishedAt = nil
	f.snapshots[id], f.rowStages[id] = s, stage
	if stage == "terminal" {
		f.finishAt(id, now)
		return
	}
	f.pending[id] = now.Add(2 * time.Second)
}

func (f *insightFixture) finishAt(id string, now time.Time) {
	s, result := f.snapshots[id], f.results[id]
	result.LastAttempt.QueryID = s.LastAttempt.QueryID
	result.LastAttempt.StartedAt = s.LastAttempt.StartedAt
	result.LastAttempt.FinishedAt = &now
	result.NextAllowedAt = fixturePointer(now.Add(5 * time.Second))
	result.Revision = s.Revision + 1
	f.snapshots[id] = result
	delete(f.pending, id)
}

func (f *insightFixture) current(id string) (accountinsight.Snapshot, bool) {
	return f.currentAt(id, time.Now().UTC())
}

func (f *insightFixture) currentAt(id string, now time.Time) (accountinsight.Snapshot, bool) {
	stage := f.rowStages[id]
	if due, ok := f.pending[id]; ok && !f.hold && stage != "queued" && stage != "running" {
		s := f.snapshots[id]
		if !now.Before(due) {
			f.finishAt(id, now)
		} else if s.LastAttempt.Status != "running" {
			s.LastAttempt.Status = "running"
			s.LastAttempt.StartedAt = &now
			f.snapshots[id] = s
		}
	}
	s, ok := f.snapshots[id]
	if !ok {
		return s, false
	}
	if f.bpsOnly && id == "fixture-3" {
		// Hide numeric usage only in the returned view, retaining the BPS
		// permission observation and the original result for easy reset.
		s.Usage = nil
	}
	if id == "fixture-1" && s.Usage != nil && f.layout != "default" {
		// Layout controls alter only the returned projection. Copy both levels
		// before replacing fields so snapshots and completed results stay intact.
		usage := *s.Usage
		usage.Windows = append([]protocol.InsightWindow(nil), s.Usage.Windows...)
		switch f.layout {
		case "weekly", "weekly-error":
			usage.ObservedAt = now.Add(-30 * time.Minute)
			usage.Windows = []protocol.InsightWindow{{Name: "secondary", WindowSeconds: fixturePointer(int64(604800)), UsedPercent: fixturePointer(68.0), ResetsAt: fixturePointer(now.Add(5*24*time.Hour + 2*time.Hour))}}
			s.Stale = true
			if f.layout == "weekly-error" {
				s.Errors = []protocol.InsightError{{Component: "usage", Code: "permission_denied", HTTPStatus: 403, Message: "合成历史周窗口，当前查询无权限"}}
			}
		case "unknown":
			for index := range usage.Windows {
				usage.Windows[index].UsedPercent = nil
			}
		case "long":
			if len(usage.Windows) > 0 {
				usage.Windows[0].Name = "合成长额度窗口名称 Synthetic long quota window for narrow-column layout verification"
				usage.Windows[0].WindowSeconds = nil
				usage.Windows[0].UsedPercent = fixturePointer(33.333333333333336)
			}
			if len(usage.Windows) > 1 {
				usage.Windows[1].UsedPercent = fixturePointer(33.333333333333336)
			}
		case "undated":
			usage.ObservedAt = time.Time{}
		case "countdowns", "pending":
			firstReset, secondReset := now.Add(4*time.Hour+59*time.Minute), now.Add(6*24*time.Hour+23*time.Hour)
			if f.layout == "pending" {
				firstReset, secondReset = now.Add(-time.Minute), now.Add(-time.Hour)
			}
			usage.LimitReached = fixturePointer(false)
			usage.Windows = []protocol.InsightWindow{
				{Name: "primary", WindowSeconds: fixturePointer(int64(18000)), UsedPercent: fixturePointer(0.0), ResetsAt: &firstReset},
				{Name: "secondary", WindowSeconds: fixturePointer(int64(604800)), UsedPercent: fixturePointer(61.25), ResetsAt: &secondReset},
				{Name: "additional", WindowSeconds: fixturePointer(int64(3600)), UsedPercent: fixturePointer(8.0), ResetsAt: fixturePointer(now.Add(time.Hour))},
			}
		case "dense":
			usage.ObservedAt = now.Add(-48 * time.Hour)
			usage.LimitReached = fixturePointer(true)
			if len(usage.Windows) > 0 {
				usage.Windows[0].UsedPercent = fixturePointer(100.0)
			}
			s.Persisted, s.Stale = false, true
			s.Errors = []protocol.InsightError{{Component: "usage", Code: "authorization_rejected", HTTPStatus: 401, Message: "合成布局样本：保留历史额度，查询返回 HTTP 401，保存状态待确认"}}
			if s.LastAttempt.Status != "running" && s.LastAttempt.Status != "queued" {
				s.LastAttempt.Status, s.LastAttempt.ErrorCode = "failed", "authorization_rejected"
			}
		}
		s.Usage = &usage
	}
	s.CanQuery, s.UnavailableReason = f.results[id].CanQuery, f.results[id].UnavailableReason
	s.RefreshState = "unavailable"
	if s.CanQuery && f.settings.Enabled {
		s.RefreshState = "eligible"
	}
	if _, pending := f.pending[id]; pending {
		s.CanQuery, s.UnavailableReason = false, "查询已在队列中或执行中"
		s.RefreshState = "queued"
		if s.LastAttempt.Status == "running" {
			s.RefreshState = "active"
		}
	} else if s.NextAllowedAt != nil && now.Before(*s.NextAllowedAt) {
		s.CanQuery, s.UnavailableReason = false, "查询冷却中，请稍后重试"
		if f.settings.Enabled && f.results[id].CanQuery {
			s.RefreshState = "coolingDown"
		}
	} else if !f.settings.Enabled {
		s.CanQuery, s.UnavailableReason = false, "账号查询功能未启用"
	}
	s.Automatic = true
	for _, excluded := range f.settings.ExcludedAccountIDs {
		if excluded == id {
			s.Automatic = false
		}
	}
	return s, ok
}

func fixtureInsightSelection(q url.Values) ([]string, error) {
	values, present := q["ids"]
	if !present {
		return nil, nil
	}
	if len(values) != 1 {
		return nil, fmt.Errorf("合成查询选择只能指定一次")
	}
	parts := strings.Split(values[0], ",")
	if len(parts) > 1000 {
		return nil, fmt.Errorf("一次最多查询1000个合成账号的状态")
	}
	ids, seen := []string{}, map[string]bool{}
	for _, id := range parts {
		if id == "" || id == "." || id == ".." || len(id) > 128 {
			return nil, fmt.Errorf("合成查询选择包含无效账号")
		}
		for _, c := range id {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
				return nil, fmt.Errorf("合成查询选择包含无效账号")
			}
		}
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	return ids, nil
}

func (f *insightFixture) availability(ids []string, now time.Time) accountinsight.RefreshAvailability {
	out := accountinsight.RefreshAvailability{}
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		out.Total++
		s, ok := f.currentAt(id, now)
		if !ok {
			out.Unavailable++
			continue
		}
		if _, pending := f.pending[id]; pending {
			if s.LastAttempt.Status == "running" {
				out.Active++
			} else {
				out.Queued++
			}
			continue
		}
		if !f.settings.Enabled || !f.results[id].CanQuery {
			out.Unavailable++
			continue
		}
		if at := s.NextAllowedAt; at != nil && now.Before(*at) {
			out.CoolingDown++
			if out.NextAllowedAt == nil || at.Before(*out.NextAllowedAt) {
				out.NextAllowedAt = at
			}
			continue
		}
		out.Eligible++
	}
	if out.NextAllowedAt != nil {
		out.RetryAfterSeconds = int((out.NextAllowedAt.Sub(now) + time.Second - 1) / time.Second)
	}
	return out
}

func (f *insightFixture) handle(w http.ResponseWriter, r *http.Request, write func(http.ResponseWriter, int, any)) bool {
	if !strings.HasPrefix(r.URL.Path, "/api/account-insights") {
		return false
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/account-insights")
	switch {
	case path == "/settings" && r.Method == http.MethodGet:
		write(w, 200, f.settings)
	case path == "/settings" && r.Method == http.MethodPut:
		var next accountinsight.Settings
		if json.NewDecoder(r.Body).Decode(&next) != nil || next.IntervalMinutes < 1 || next.IntervalMinutes > 1440 {
			write(w, 400, map[string]any{"error": "合成配置格式错误"})
			return true
		}
		f.requests = append(f.requests, map[string]any{"method": r.Method, "path": path, "settings": next})
		if next.Revision != f.settings.Revision || f.settingsOutcome == "conflict" {
			write(w, 409, map[string]any{"error": "合成版本冲突，请重新读取", "settings": f.settings})
			return true
		}
		next.Revision++
		f.settings = next
		if f.settingsOutcome == "unknown" {
			f.settings.PersistenceWarning = "合成目录同步结果未知，请先核对再明确保存"
			write(w, 503, map[string]any{"error": f.settings.PersistenceWarning, "settings": f.settings, "uncertain": true})
			return true
		}
		if !next.Enabled {
			for id := range f.pending {
				f.cancel(id)
			}
		}
		write(w, 200, f.settings)
	case path == "/refresh" && r.Method == http.MethodPost:
		if !f.settings.Enabled {
			write(w, 409, map[string]any{"error": "账号查询功能未启用"})
			return true
		}
		var body struct {
			IDs json.RawMessage `json:"ids"`
			All bool            `json:"all"`
		}
		var ids []string
		if json.NewDecoder(r.Body).Decode(&body) != nil || body.All && body.IDs != nil || !body.All && (json.Unmarshal(body.IDs, &ids) != nil || len(ids) == 0 || len(ids) > 1000) {
			write(w, 400, map[string]any{"error": "缺少合成账号"})
			return true
		}
		f.requests = append(f.requests, map[string]any{"method": r.Method, "path": path, "ids": ids, "all": body.All})
		if body.All {
			for id := range f.snapshots {
				ids = append(ids, id)
			}
			sort.Strings(ids)
		}
		f.sequence++
		receipt := accountinsight.Receipt{QueryID: fmt.Sprintf("synthetic-query-%d", f.sequence), Accepted: []string{}, Merged: []string{}, Skipped: []accountinsight.Skip{}}
		seen := map[string]bool{}
		for _, id := range ids {
			if seen[id] {
				continue
			}
			seen[id] = true
			s, ok := f.current(id)
			if !ok {
				receipt.Skipped = append(receipt.Skipped, accountinsight.Skip{ID: id, Reason: "合成账号不存在"})
				continue
			}
			if _, ok := f.pending[id]; ok {
				receipt.Merged = append(receipt.Merged, id)
				continue
			}
			if !s.CanQuery {
				receipt.Skipped = append(receipt.Skipped, accountinsight.Skip{ID: id, Reason: s.UnavailableReason})
				continue
			}
			// Eligibility uses the public view; queue state uses the stored
			// snapshot so visual layout projections never become source data.
			s = f.snapshots[id]
			now := time.Now().UTC()
			s.LastAttempt = accountinsight.Attempt{QueryID: receipt.QueryID, Status: "queued"}
			s.Revision++
			delete(f.rowStages, id)
			f.snapshots[id], f.pending[id] = s, now.Add(2*time.Second)
			receipt.Accepted = append(receipt.Accepted, id)
		}
		if f.responseLost {
			write(w, 503, map[string]any{"error": "合成查询回执丢失，请通过查询状态核对，勿重复提交", "outcome": "unknown"})
			return true
		}
		write(w, 202, receipt)
	case strings.HasPrefix(path, "/queries/") && strings.HasSuffix(path, "/cancel") && r.Method == http.MethodPost:
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/queries/"), "/cancel")
		f.requests = append(f.requests, map[string]any{"method": r.Method, "path": path})
		write(w, 200, map[string]bool{"cancelled": f.cancel(id)})
	case path == "/status" && r.Method == http.MethodGet:
		selected, err := fixtureInsightSelection(r.URL.Query())
		if err != nil {
			write(w, 400, map[string]any{"error": err.Error()})
			return true
		}
		if f.statusError {
			write(w, 503, map[string]any{"error": "合成查询状态暂不可用；历史结果仍保留"})
			return true
		}
		counts := accountinsight.Counts{}
		ids := make([]string, 0, len(f.snapshots))
		now := time.Now().UTC()
		for id := range f.snapshots {
			ids = append(ids, id)
			s, _ := f.currentAt(id, now)
			switch s.LastAttempt.Status {
			case "success":
				counts.Success++
			case "partial":
				counts.Partial++
			case "failed":
				counts.Failed++
			case "skipped":
				counts.Skipped++
			case "cancelled":
				counts.Cancelled++
			}
		}
		sort.Strings(ids)
		refresh := f.availability(ids, now)
		status := accountinsight.Status{Enabled: f.settings.Enabled, Automatic: f.settings.Automatic, Running: refresh.Queued+refresh.Active > 0, Queued: refresh.Queued, Active: refresh.Active, Counts: counts, Revision: f.settings.Revision, LastError: f.settings.PersistenceWarning, Refresh: refresh}
		if selected != nil {
			selection := f.availability(selected, now)
			status.Selection = &selection
		}
		write(w, 200, status)
	case path == "" && r.Method == http.MethodGet:
		out := map[string]accountinsight.Snapshot{}
		for _, id := range strings.Split(r.URL.Query().Get("ids"), ",") {
			if s, ok := f.current(id); ok {
				out[id] = s
			}
		}
		write(w, 200, map[string]any{"snapshots": out, "revision": f.settings.Revision})
	case strings.HasPrefix(path, "/") && r.Method == http.MethodGet:
		if s, ok := f.current(strings.TrimPrefix(path, "/")); ok {
			write(w, 200, s)
		} else {
			write(w, 404, map[string]any{"error": "合成账号不存在"})
		}
	default:
		write(w, 404, map[string]any{"error": "unknown synthetic insight route"})
	}
	return true
}

func (f *insightFixture) cancel(id string) bool {
	if _, exists := f.pending[id]; !exists {
		return false
	}
	delete(f.pending, id)
	delete(f.rowStages, id)
	s := f.snapshots[id]
	s.LastAttempt.Status, s.LastAttempt.Message = "cancelled", "合成查询已取消，保留历史结果"
	s.LastAttempt.FinishedAt = fixturePointer(time.Now().UTC())
	s.Revision++
	f.snapshots[id] = s
	return true
}
