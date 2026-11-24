package application

import (
	"context"
	"errors"
	"sync"

	"tosub2-go-port/internal/accountinsight"
	"tosub2-go-port/internal/config"
	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/protocol"
)

// AccountInsightController owns observation queries against committed grants.
// Service owns scheduling and history; the controller binds account ownership,
// fixed transport identity and diagnostics without authentication or refresh.
type AccountInsightController struct {
	cfg              config.Config
	manager          *jobs.Manager
	transportFactory readOnlyTransportFactory
}

func NewAccountInsightController(cfg config.Config, manager *jobs.Manager) *AccountInsightController {
	return &AccountInsightController{cfg: cfg, manager: manager}
}

func (c *AccountInsightController) Provider(timezone func() string) accountinsight.Provider {
	return accountinsight.Provider{Accounts: c.accounts, RetainedIDs: c.manager.RetainedAccountIDs, Acquire: c.acquire, Timezone: timezone}
}

func (c *AccountInsightController) accounts() []accountinsight.Account {
	list := ReadOnlyAccountInventory(c.cfg.OutputRoot, c.manager)
	out := make([]accountinsight.Account, 0, len(list))
	for _, j := range list {
		a := accountinsight.Account{ID: j.ID, AuthorizationMethod: j.AuthorizationMethod, Revision: j.Revision, CanQuery: true}
		switch {
		case !j.ResultSaved:
			a.CanQuery = false
			a.Reason = "尚无已保存授权，不会自动登录"
		case j.RecoveryBlocked:
			a.CanQuery = false
			a.Reason = "2FA 安全操作尚未完成，暂不可查询"
		case j.Status == jobs.StatusQueued || jobs.IsActiveStatus(j.Status) || j.Executing:
			a.CanQuery = false
			a.Reason = "账号操作进行中，查询暂缓"
		case !j.ExportRegular:
			a.CanQuery = false
			a.Reason = "已保存授权文件暂不可读取"
		}
		out = append(out, a)
	}
	return out
}
func (c *AccountInsightController) acquire(ctx context.Context, id string) (*accountinsight.Lease, error) {
	g, client, release, err := c.AcquireSavedQuery(ctx, id)
	if err != nil {
		return nil, err
	}
	return &accountinsight.Lease{Context: g.Context(), Identity: g.IdentityKey, Validate: g.Validate, Release: release, Query: func(ctx context.Context, opts protocol.InsightsOptions) protocol.InsightResult {
		if ctx.Err() != nil || g.Validate() != nil {
			return protocol.InsightResult{Errors: []protocol.InsightError{{Component: "query", Code: "query_superseded", Message: "账号或授权已改变，本次未发起查询"}}}
		}
		recorders := beginInsightRequestObservations(c.manager, id)
		result := client.Query(ctx, g.Credentials, opts)
		completeInsightRequestObservations(ctx, g.Validate, recorders, result)
		return result
	}}, nil
}

func beginInsightRequestObservations(manager *jobs.Manager, id string) map[string]*jobs.AccountRequestRecorder {
	recorders := make(map[string]*jobs.AccountRequestRecorder, 4)
	for _, source := range []string{"usage", "bps", "subscription", "resetCredits"} {
		recorders[source] = manager.BeginAccountRequest(id, source)
	}
	return recorders
}

func completeInsightRequestObservations(ctx context.Context, validate func() error, recorders map[string]*jobs.AccountRequestRecorder, result protocol.InsightResult) {
	if ctx.Err() != nil || validate() != nil {
		return
	}
	for _, source := range result.AttemptedComponents {
		recorder := recorders[source]
		if recorder == nil {
			continue
		}
		var observations []jobs.AccountRequestObservation
		canceled := false
		for _, failure := range result.Errors {
			if failure.Component != source {
				continue
			}
			switch failure.Code {
			case "canceled", "cancelled", "superseded", "query_superseded":
				canceled = true
			default:
				observations = append(observations, jobs.AccountRequestObservation{Status: "failed", HTTPStatus: failure.HTTPStatus, ErrorCode: failure.Code})
			}
		}
		if canceled {
			continue
		}
		if len(observations) == 0 {
			confirmed := source == "usage" && result.Usage != nil || source == "bps" && result.BPS != nil || source == "subscription" && result.Subscription != nil && result.Subscription.IdentityVerified || source == "resetCredits" && result.Usage != nil && result.Usage.ResetCredits != nil
			if !confirmed {
				continue
			}
			observations = []jobs.AccountRequestObservation{{Status: "success"}}
		}
		// Display persistence never changes the query outcome or starts a retry.
		_ = recorder.Complete(observations...)
	}
}

// Both scheduled observations and an explicit plan refresh use the same
// committed grant, account lease, expiry checks and fixed proxy selection.
func (c *AccountInsightController) AcquireSavedQuery(ctx context.Context, id string) (*AccountInsightGrant, *protocol.InsightsClient, func(), error) {
	g, err := LoadAccountInsightGrant(ctx, c.cfg.OutputRoot, c.manager, id)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, nil, nil, err
		}
		code, message := "authorization_unavailable", "当前已保存授权暂不可读取，请检查账号或恢复状态"
		switch {
		case errors.Is(err, ErrInsightGrantIdentity):
			code, message = "identity_mismatch", "保存的授权身份不一致，未发起查询"
		case errors.Is(err, ErrInsightProxyUnavailable):
			code, message = "proxy_unavailable", "账号代理配置暂不可读取，未改用直连"
		case errors.Is(err, ErrInsightRecoveryPending):
			code, message = "recovery_pending", "已有授权刷新结果待保存，请先完成恢复"
		case errors.Is(err, jobs.ErrTOTPRecoveryPending), errors.Is(err, jobs.ErrTOTPSetupVerificationPending), errors.Is(err, jobs.ErrTOTPRecoveryRequired):
			code, message = "recovery_pending", "账号安全操作尚未完成，未发起查询"
		case errors.Is(err, ErrInsightCredentialsUnavailable):
			code, message = "credentials_unavailable", "安全存储暂不可读取，无法确认账号状态，未发起查询"
		case errors.Is(err, jobs.ErrAccountQueryBusy):
			code, message = "account_busy", "账号操作进行中，查询暂缓"
		case errors.Is(err, jobs.ErrAccountQuerySuperseded):
			code, message = "query_superseded", "账号或授权已改变，本次未发起查询"
		case errors.Is(err, jobs.ErrAccountQueryLockUnavailable):
			code, message = "ownership_unavailable", "本机账号互斥保护不可用，未发起查询"
		}
		return nil, nil, nil, &accountinsight.QueryError{Code: code, Message: message}
	}
	fail := func(code, message string) (*AccountInsightGrant, *protocol.InsightsClient, func(), error) {
		g.Release()
		return nil, nil, nil, &accountinsight.QueryError{Code: code, Message: message}
	}
	doer, closeTransport, transportErr := openReadOnlyTransport(c.cfg, g, c.transportFactory)
	if transportErr != nil {
		message := "查询传输配置不可用"
		switch transportErr.Code {
		case readOnlyAuthorizationExpired:
			message = "当前 access token 已过期，请先刷新授权；查询不会自动登录"
		case readOnlyProxySelectionRequired:
			message = "请保存明确的账号代理后查询"
			if !g.ProxyOverride && c.cfg.ProxyURL == "" && len(c.cfg.ProxyURLs) > 0 {
				message = "原授权未保存明确代理，无法安全选择代理池中的出口"
			}
		case readOnlyTransportUnavailable:
			if !readOnlyUsesNative(c.cfg) {
				message = "所选 TLS 身份不可用，未替换配置"
			}
		}
		return fail(string(transportErr.Code), message)
	}
	client := &protocol.InsightsClient{Transport: doer}
	var once sync.Once
	return g, client, func() { once.Do(func() { closeTransport(); g.Release() }) }, nil
}
