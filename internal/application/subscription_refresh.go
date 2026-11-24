package application

import (
	"context"
	"encoding/hex"
	"errors"
	"time"

	"tosub2-go-port/internal/accountplan"
	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/protocol"
)

// SubscriptionRefreshError contains only the protocol's closed, public-safe
// diagnostic fields. Raw upstream responses and authorization never escape.
type SubscriptionRefreshError struct{ Failure protocol.InsightError }

func (e *SubscriptionRefreshError) Error() string { return e.Failure.Message }

var ErrSubscriptionAuthorizationExpired = errors.New("当前 access token 已过期，未查询套餐，也未重新登录或请求邮箱验证码")

// RefreshAccountSubscription performs a read-only upstream query and commits a
// verified observation under the original account reservation. It never writes
// the OAuth export, refreshes a token or falls back to authentication.
func RefreshAccountSubscription(ctx context.Context, grant *AccountInsightGrant, client *protocol.InsightsClient, options protocol.InsightsOptions) (*jobs.Job, protocol.InsightResult, error) {
	var result protocol.InsightResult
	if grant == nil || grant.Lease == nil || client == nil {
		return nil, result, ErrInsightGrantUnavailable
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if job := grant.Lease.Job(); job == nil || !job.ResultSaved {
		return nil, result, ErrInsightGrantUnavailable
	}
	if err := ctx.Err(); err != nil {
		return nil, result, err
	}
	recorder := beginGrantRequest(grant, "subscription")
	if grant.ExpiresAt != nil && !time.Now().Before(*grant.ExpiresAt) {
		completeGrantRequest(grant, recorder, jobs.AccountRequestObservation{Status: "failed", ErrorCode: "authorization_expired"})
		return nil, result, ErrSubscriptionAuthorizationExpired
	}
	if err := grant.Validate(); err != nil {
		return nil, result, err
	}
	lease, ok := grant.Lease.(*jobs.AccountQueryLease)
	if !ok {
		return nil, result, jobs.ErrAccountQuerySuperseded
	}
	queryCtx, cancel := context.WithCancel(ctx)
	stopLeaseCancel := context.AfterFunc(grant.Context(), cancel)
	defer cancel()
	defer stopLeaseCancel()
	result = client.QuerySubscription(queryCtx, grant.Credentials, options)
	// A canceled or replaced query can return no subscription. Classify the
	// actual interruption before interpreting a missing business result.
	if err := ctx.Err(); err != nil {
		return nil, result, err
	}
	if queryCtx.Err() != nil {
		return nil, result, jobs.ErrAccountQuerySuperseded
	}
	subscription := result.Subscription
	if subscription == nil || !subscription.IdentityVerified || subscription.Scope != "account" || subscription.AccountID != grant.Credentials.AccountID || accountplan.Normalize(subscription.PlanType) == "unknown" {
		failure := protocol.InsightError{Component: "subscription", Code: "subscription_unconfirmed", Message: "未取得可确认的当前套餐，已保留上次结果；未重新登录"}
		if len(result.Errors) != 0 {
			failure = result.Errors[0]
		}
		if observations := subscriptionRequestObservations(result, false); len(observations) != 0 {
			completeGrantRequest(grant, recorder, observations...)
		}
		return nil, result, &SubscriptionRefreshError{Failure: failure}
	}
	if err := ctx.Err(); err != nil {
		return nil, result, err
	}
	if err := grant.Validate(); err != nil {
		return nil, result, err
	}
	// Record the verified upstream outcome while the query grant still matches.
	// The subsequent local plan commit updates metadata and invalidates this lease.
	if observations := subscriptionRequestObservations(result, true); len(observations) != 0 {
		completeGrantRequest(grant, recorder, observations...)
	}
	observation := jobs.SubscriptionObservation{PlanType: subscription.PlanType, Source: subscription.Source, ObservedAt: subscription.ObservedAt, AccountID: subscription.AccountID, ExpiresAt: subscription.ExpiresAt}
	updated, err := lease.CommitSubscription(ctx, observation, hex.EncodeToString(grant.digest[:]))
	if err != nil {
		return nil, result, err
	}
	return updated, result, nil
}
