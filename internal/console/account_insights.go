package console

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
	"tosub2-go-port/internal/accountinsight"
	"tosub2-go-port/internal/application"
	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/protocol"
	"tosub2-go-port/internal/transport"
)

func (s *Server) initializeAccountInsights() {
	p := accountinsight.Provider{Accounts: s.insightAccounts, Acquire: s.acquireInsight, Timezone: func() string {
		s.monitorMu.Lock()
		defer s.monitorMu.Unlock()
		return s.monitorConfig.OpenAIRequestTimezone
	}}
	service, err := accountinsight.New(s.cfg.OutputRoot, p)
	if err != nil {
		if s.logger != nil {
			s.logger.Warn("account query storage unavailable")
		}
		return
	}
	s.insights = service
}
func (s *Server) StartAccountInsights(ctx context.Context) {
	if s.insights != nil {
		s.insights.Start(ctx)
	}
}
func (s *Server) insightAccounts() []accountinsight.Account {
	list := s.jobs.List()
	out := make([]accountinsight.Account, 0, len(list))
	for _, j := range list {
		revision := fmt.Sprintf("%s|%s|%d|%s|%s|%t|%t", j.CreatedAt.UTC().Format(time.RFC3339Nano), j.AuthorizationMethod, j.Attempts, j.LastOperationAt.UTC().Format(time.RFC3339Nano), j.UpdatedAt.UTC().Format(time.RFC3339Nano), j.ResultSaved, j.ProxyConfigured)
		st, err := os.Lstat(filepath.Join(s.cfg.OutputRoot, j.ID, "sub2api-import-oauth.json"))
		if err == nil {
			revision += fmt.Sprintf("|%d|%d", st.Size(), st.ModTime().UnixNano())
		}
		sum := sha256.Sum256([]byte(revision))
		a := accountinsight.Account{ID: j.ID, AuthorizationMethod: j.AuthorizationMethod, Revision: hex.EncodeToString(sum[:]), CanQuery: true}
		switch {
		case !j.ResultSaved:
			a.CanQuery = false
			a.Reason = "尚无已保存授权，不会自动登录"
		case j.TOTPRecoveryPending || j.TOTPCredentialInvalidated:
			a.CanQuery = false
			a.Reason = "安全操作待恢复，暂不可查询"
		case j.Status == jobs.StatusQueued || isActiveStatus(j.Status):
			a.CanQuery = false
			a.Reason = "账号操作进行中，查询暂缓"
		case err != nil || !st.Mode().IsRegular():
			a.CanQuery = false
			a.Reason = "已保存授权文件暂不可读取"
		}
		out = append(out, a)
	}
	return out
}
func (s *Server) acquireInsight(ctx context.Context, id string) (*accountinsight.Lease, error) {
	g, err := application.LoadAccountInsightGrant(ctx, s.cfg.OutputRoot, s.jobs, id)
	if err != nil {
		code, message := "authorization_unavailable", "当前已保存授权暂不可读取，请检查账号或恢复状态"
		switch {
		case errors.Is(err, application.ErrInsightGrantIdentity):
			code, message = "identity_mismatch", "保存的授权身份不一致，未发起查询"
		case errors.Is(err, application.ErrInsightProxyUnavailable):
			code, message = "proxy_unavailable", "账号代理配置暂不可读取，未改用直连"
		case errors.Is(err, application.ErrInsightRecoveryPending):
			code, message = "recovery_pending", "已有授权刷新结果待保存，请先完成恢复"
		case errors.Is(err, jobs.ErrAccountQueryBusy):
			code, message = "account_busy", "账号操作进行中，查询暂缓"
		}
		return nil, &accountinsight.QueryError{Code: code, Message: message}
	}
	fail := func(code, message string) (*accountinsight.Lease, error) {
		g.Release()
		return nil, &accountinsight.QueryError{Code: code, Message: message}
	}
	if g.ExpiresAt != nil && !time.Now().Before(*g.ExpiresAt) {
		return fail("authorization_expired", "当前 access token 已过期，请先刷新授权；查询不会自动登录")
	}
	proxy := g.ProxyURL
	if !g.ProxyOverride {
		proxy = s.cfg.ProxyURL
		if proxy == "" && len(s.cfg.ProxyURLs) > 0 {
			return fail("proxy_selection_required", "原授权未保存明确代理，无法安全选择代理池中的出口")
		}
	}
	// Never create a fresh rotating proxy identity to make a metadata query work.
	if strings.Contains(proxy, "{session}") || strings.Contains(proxy, "{sid}") || strings.Contains(proxy, "{random}") {
		return fail("proxy_selection_required", "请保存明确的账号代理后查询")
	}
	var doer protocol.Doer
	var closeTransport func()
	if s.cfg.CLI.NativeHTTP || s.cfg.TLSProfile == transport.NativeProfile {
		c, e := transport.NewNativeClient(proxy, transport.NewCookieJar())
		if e != nil {
			return fail("transport_unavailable", "查询传输配置不可用")
		}
		doer = c
		closeTransport = c.Close
	} else {
		profile := s.cfg.AuthTLSProfile
		if profile == "" {
			profile = "chrome146"
		}
		c, e := transport.NewCurlCompatibleFingerprintClientExact(profile, proxy, transport.NewCookieJar())
		if e != nil {
			return fail("transport_unavailable", "所选 TLS 身份不可用，未替换配置")
		}
		doer = c
		closeTransport = c.Close
	}
	client := protocol.InsightsClient{Transport: doer}
	return &accountinsight.Lease{Context: g.Context(), Identity: g.IdentityKey, Validate: g.Validate, Release: func() { closeTransport(); g.Release() }, Query: func(ctx context.Context, opts protocol.InsightsOptions) protocol.InsightResult {
		return client.Query(ctx, g.Credentials, opts)
	}}, nil
}
func (s *Server) accountInsightsAPI(w http.ResponseWriter, r *http.Request) {
	if s.insights == nil {
		writeError(w, http.StatusServiceUnavailable, "账号查询存储不可用")
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/account-insights")
	switch {
	case path == "/settings" && r.Method == http.MethodGet:
		writeJSON(w, 200, s.insights.Settings())
	case path == "/settings" && r.Method == http.MethodPut:
		var c accountinsight.Settings
		if !decodeJSON(w, r, &c) {
			return
		}
		settings, err := s.insights.Update(r.Context(), c)
		if err != nil {
			status := 400
			outcome := "failed"
			uncertain := errors.Is(err, accountinsight.ErrPersistenceUncertain)
			if errors.Is(err, accountinsight.ErrConflict) {
				status = 409
			}
			if uncertain || errors.Is(err, accountinsight.ErrPersistence) || errors.Is(err, accountinsight.ErrClosed) {
				status = 503
			}
			if uncertain {
				outcome = "unknown"
			}
			writeJSON(w, status, map[string]any{"error": err.Error(), "settings": settings, "uncertain": uncertain, "outcome": outcome})
			return
		}
		writeJSON(w, 200, settings)
	case path == "/status" && r.Method == http.MethodGet:
		writeJSON(w, 200, s.insights.Status())
	case path == "/refresh" && r.Method == http.MethodPost:
		var body struct {
			IDs []string `json:"ids"`
		}
		if !decodeJSON(w, r, &body) {
			return
		}
		receipt, err := s.insights.Refresh(body.IDs)
		if err != nil {
			status := 400
			if errors.Is(err, accountinsight.ErrDisabled) || errors.Is(err, accountinsight.ErrClosed) {
				status = 409
			}
			writeError(w, status, err.Error())
			return
		}
		writeJSON(w, 202, receipt)
	case strings.HasPrefix(path, "/queries/") && strings.HasSuffix(path, "/cancel") && r.Method == http.MethodPost:
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/queries/"), "/cancel")
		if id == "" || strings.Contains(id, "/") {
			writeError(w, 400, "invalid query identifier")
			return
		}
		writeJSON(w, 200, map[string]bool{"cancelled": s.insights.Cancel(id)})
	case path == "" && r.Method == http.MethodGet:
		ids := strings.Split(r.URL.Query().Get("ids"), ",")
		if len(ids) > 100 {
			writeError(w, 400, "一次最多读取100个账号")
			return
		}
		out, revision := s.insights.Snapshots(ids)
		writeJSON(w, 200, map[string]any{"snapshots": out, "revision": revision})
	case strings.HasPrefix(path, "/") && len(path) > 1 && !strings.Contains(path[1:], "/") && r.Method == http.MethodGet:
		v, ok := s.insights.Snapshot(path[1:])
		if !ok {
			writeError(w, 404, "账号不存在")
			return
		}
		writeJSON(w, 200, v)
	default:
		writeError(w, 404, "unknown account query route")
	}
}
