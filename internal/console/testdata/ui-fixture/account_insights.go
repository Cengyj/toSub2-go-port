package main

import (
	"encoding/json"
	"net/http"
	"net/url"
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
	settingsOutcome string
	requests        []map[string]any
}

func fixturePointer[T any](value T) *T { return &value }

func newInsightFixture() *insightFixture {
	now := time.Now().UTC()
	old := now.Add(-48 * time.Hour)
	settings := accountinsight.DefaultSettings()
	settings.Revision = 1
	f := &insightFixture{settings: settings, snapshots: map[string]accountinsight.Snapshot{}, results: map[string]accountinsight.Snapshot{}, pending: map[string]time.Time{}, settingsOutcome: "success", requests: []map[string]any{}}
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
			s.LastAttempt.Status, s.LastAttempt.Message, s.UnavailableReason = "skipped", "安全操作待恢复，未发起查询", "安全操作待恢复，暂不可查询"
			s.BPS = &protocol.InsightBPS{Source: "bps_access", ObservedAt: old, Models: []protocol.InsightBPSModel{{ID: "synthetic-unknown-model"}}}
			s.Stale = true
		case 4:
			s.LastAttempt.Status, s.LastAttempt.ErrorCode, s.LastAttempt.Message = "failed", "timeout", "合成来源超时，原有授权仍可用；本次未更新结果"
			s.UnavailableReason, s.Persisted, s.Stale = "2FA 安全操作待核实，请先恢复", false, true
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
	if q.Has("insightSettings") {
		f.settingsOutcome = q.Get("insightSettings")
	}
}

func (f *insightFixture) state() map[string]any {
	return map[string]any{"settings": f.settings, "pending": len(f.pending), "hold": f.hold, "settingsOutcome": f.settingsOutcome, "requests": f.requests}
}

func (f *insightFixture) current(id string) (accountinsight.Snapshot, bool) {
	if due, ok := f.pending[id]; ok && !f.hold && !time.Now().Before(due) {
		f.snapshots[id] = f.results[id]
		delete(f.pending, id)
	}
	s, ok := f.snapshots[id]
	s.Automatic = true
	for _, excluded := range f.settings.ExcludedAccountIDs {
		if excluded == id {
			s.Automatic = false
		}
	}
	return s, ok
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
		var body map[string][]string
		if json.NewDecoder(r.Body).Decode(&body) != nil || len(body["ids"]) == 0 {
			write(w, 400, map[string]any{"error": "缺少合成账号"})
			return true
		}
		f.requests = append(f.requests, map[string]any{"method": r.Method, "path": path, "ids": body["ids"]})
		receipt := accountinsight.Receipt{QueryID: "synthetic-query", Accepted: []string{}, Merged: []string{}, Skipped: []accountinsight.Skip{}}
		seen := map[string]bool{}
		for _, id := range body["ids"] {
			if seen[id] {
				continue
			}
			seen[id] = true
			s, ok := f.current(id)
			if !ok || !s.CanQuery {
				receipt.Skipped = append(receipt.Skipped, accountinsight.Skip{ID: id, Reason: s.UnavailableReason})
				continue
			}
			if _, ok := f.pending[id]; ok {
				receipt.Merged = append(receipt.Merged, id)
				continue
			}
			now := time.Now().UTC()
			s.LastAttempt = accountinsight.Attempt{QueryID: receipt.QueryID, Status: "queued", StartedAt: &now}
			f.snapshots[id], f.pending[id] = s, now.Add(2*time.Second)
			receipt.Accepted = append(receipt.Accepted, id)
		}
		write(w, 202, receipt)
	case strings.HasPrefix(path, "/queries/") && strings.HasSuffix(path, "/cancel") && r.Method == http.MethodPost:
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/queries/"), "/cancel")
		f.requests = append(f.requests, map[string]any{"method": r.Method, "path": path})
		write(w, 200, map[string]bool{"cancelled": f.cancel(id)})
	case path == "/status" && r.Method == http.MethodGet:
		counts := accountinsight.Counts{}
		for id := range f.snapshots {
			s, _ := f.current(id)
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
		write(w, 200, accountinsight.Status{Enabled: f.settings.Enabled, Automatic: f.settings.Automatic, Running: len(f.pending) > 0, Queued: len(f.pending), Counts: counts, Revision: f.settings.Revision, LastError: f.settings.PersistenceWarning})
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
	s := f.snapshots[id]
	s.LastAttempt.Status, s.LastAttempt.Message = "cancelled", "合成查询已取消，保留历史结果"
	f.snapshots[id] = s
	return true
}
