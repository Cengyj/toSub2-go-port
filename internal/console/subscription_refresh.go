package console

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"tosub2-go-port/internal/accountinsight"
	"tosub2-go-port/internal/application"
	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/protocol"
)

// This is only a local eligibility hint. Acquisition validates the private
// grant, account ownership, safety quarantine and expiry again before dispatch.
func (s *Server) subscriptionRefreshUnavailable(j *jobs.Job) string {
	if j == nil || !j.ResultSaved {
		return "尚无可用的已保存授权，不能查询套餐；不会自动登录"
	}
	if j.TOTPRecoveryPending || j.TOTPCredentialInvalidated {
		return "2FA 尚未完成，套餐查询保持暂停"
	}
	if j.Status == jobs.StatusQueued || isActiveStatus(j.Status) {
		return "请等待当前账号操作结束后查询套餐"
	}
	if !safePathID(j.ID) {
		return "账号标识无效"
	}
	path := filepath.Join(s.cfg.OutputRoot, j.ID, "sub2api-import-oauth.json")
	if info, err := os.Lstat(path); err != nil || !info.Mode().IsRegular() {
		return "已保存授权文件不可用，未重新登录"
	}
	if _, err := os.Lstat(filepath.Join(s.cfg.OutputRoot, j.ID, ".sub2api-import-oauth.json.refresh-grant-recovery.json")); !errors.Is(err, os.ErrNotExist) {
		return "已有授权刷新结果待保存，请先完成本地恢复"
	}
	return ""
}

func (s *Server) addSubscriptionRefreshState(out map[string]any, j *jobs.Job) {
	reason := s.subscriptionRefreshUnavailable(j)
	out["canRefreshSubscription"], out["subscriptionRefreshUnavailableReason"] = reason == "", reason
	out["subscriptionPlanCheckedAt"], out["subscriptionPlanSource"], out["subscriptionPlanStale"] = j.SubscriptionPlanCheckedAt, j.SubscriptionPlanSource, j.SubscriptionPlanStale
}

func (s *Server) refreshSubscription(w http.ResponseWriter, r *http.Request, id string) {
	var body map[string]any
	if !decodeJSON(w, r, &body) {
		return
	}
	if body == nil || len(body) != 0 {
		writeError(w, http.StatusBadRequest, "刷新套餐仅接受空对象，使用当前已保存授权")
		return
	}
	j, exists := s.jobs.Get(id)
	if !exists {
		writeError(w, http.StatusNotFound, "账号不存在")
		return
	}
	if reason := s.subscriptionRefreshUnavailable(j); reason != "" {
		writeJSON(w, http.StatusConflict, map[string]any{"error": reason, "code": "subscription_unavailable", "outcome": "failed"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	grant, client, release, err := s.acquireSavedAccountQuery(ctx, id)
	if err != nil {
		s.subscriptionRefreshError(w, id, err)
		return
	}
	defer release()
	s.monitorMu.Lock()
	timezone := s.monitorConfig.OpenAIRequestTimezone
	s.monitorMu.Unlock()
	updated, result, err := application.RefreshAccountSubscription(ctx, grant, client, protocol.InsightsOptions{Timezone: timezone})
	if err != nil {
		s.subscriptionRefreshError(w, id, err)
		return
	}
	if result.Errors == nil {
		result.Errors = []protocol.InsightError{}
	}
	// Commit returns a sanitized value. Re-read the current job so publicJob
	// derives credential-availability flags exactly as the ordinary list does.
	if current, ok := s.jobs.Get(id); ok {
		updated = current
	}
	writeJSON(w, http.StatusOK, map[string]any{"job": s.publicJob(updated), "subscription": result.Subscription, "warnings": result.Errors})
}

func (s *Server) subscriptionRefreshError(w http.ResponseWriter, id string, err error) {
	status := http.StatusServiceUnavailable
	result := map[string]any{"error": "套餐结果未能可靠保存，请重新读取账号状态；未重新登录", "code": "subscription_persistence_failed", "outcome": "failed"}
	var acquisition *accountinsight.QueryError
	var query *application.SubscriptionRefreshError
	switch {
	case errors.Is(err, jobs.ErrSubscriptionPersistenceUncertain):
		result["error"], result["code"], result["outcome"] = "套餐已查询，本地保存结果待核实；请重新读取账号状态", "persistence_uncertain", "unknown"
		if current, ok := s.jobs.Get(id); ok {
			result["job"] = s.publicJob(current)
		}
	case errors.As(err, &acquisition):
		status = http.StatusConflict
		result["error"], result["code"] = acquisition.Message, acquisition.Code
	case errors.As(err, &query):
		status = http.StatusBadGateway
		result["error"], result["code"] = query.Error()+"；已保留上次套餐，未重新登录", query.Failure.Code
		result["httpStatus"], result["retryAfter"] = query.Failure.HTTPStatus, query.Failure.RetryAfter
	case errors.Is(err, jobs.ErrAccountQuerySuperseded):
		status = http.StatusConflict
		result["error"], result["code"] = "账号或授权已变化，本次查询结果未采用；请重新读取账号状态", "query_superseded"
	case errors.Is(err, application.ErrSubscriptionAuthorizationExpired), errors.Is(err, application.ErrInsightGrantUnavailable):
		status = http.StatusConflict
		result["error"], result["code"] = "已保存授权不可用或已过期，本次未查询套餐，也未重新登录", "authorization_unavailable"
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		status = http.StatusGatewayTimeout
		result["error"], result["code"] = "套餐查询已取消或超时，已保留上次结果；未重新登录", "query_timeout"
	}
	writeJSON(w, status, result)
}
