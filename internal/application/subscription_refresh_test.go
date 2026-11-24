package application

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/protocol"
	"tosub2-go-port/internal/transport"
)

type subscriptionRefreshDoer func(context.Context, transport.Request) (*transport.Response, error)

func (f subscriptionRefreshDoer) Do(ctx context.Context, request transport.Request) (*transport.Response, error) {
	return f(ctx, request)
}

func subscriptionRefreshFixture(t *testing.T, plan string) *insightGrantFixture {
	t.Helper()
	f := newInsightGrantFixture(t, jobs.AuthorizationMethodChatGPT, "")
	f.account["credentials"].(map[string]any)["chatgpt_plan_type"] = plan
	f.write(t)
	f.manager.UpdateResult(f.job.ID, true, "synthetic saved authorization")
	return f
}

func subscriptionRefreshResponse(t *testing.T, plan string, expiry any) *transport.Response {
	t.Helper()
	data, err := json.Marshal(map[string]any{"accounts": map[string]any{"synthetic-workspace": map[string]any{"account": map[string]any{"account_id": "synthetic-workspace", "plan_type": plan}, "entitlement": map[string]any{"expires_at": expiry}}}})
	if err != nil {
		t.Fatal(err)
	}
	return &transport.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: data}
}

func TestRefreshAccountSubscriptionUsesSavedGrantAndCommitsCurrentPlan(t *testing.T) {
	for _, transition := range [][2]string{{"free", "plus"}, {"plus", "free"}} {
		t.Run(transition[0]+"-to-"+transition[1], func(t *testing.T) {
			f := subscriptionRefreshFixture(t, transition[0])
			before, _ := f.manager.Get(f.job.ID)
			authorization, _ := os.ReadFile(f.path)
			grant := f.load(t)
			calls := 0
			client := &protocol.InsightsClient{Transport: subscriptionRefreshDoer(func(_ context.Context, r transport.Request) (*transport.Response, error) {
				calls++
				if r.Method != http.MethodGet || r.URL != "https://chatgpt.com/backend-api/accounts/check/v4-2023-04-27" || r.Headers.Get("Authorization") != "Bearer synthetic-private-access" || r.Headers.Get("Chatgpt-Account-Id") != "synthetic-workspace" {
					t.Fatal("query did not use only the saved grant and subscription endpoint")
				}
				return subscriptionRefreshResponse(t, transition[1], "2030-01-01T00:00:00Z"), nil
			})}
			updated, result, err := RefreshAccountSubscription(t.Context(), grant, client, protocol.InsightsOptions{})
			if err != nil || calls != 1 || updated.PlanType != transition[1] || result.Subscription.PlanType != transition[1] || updated.SubscriptionPlanCheckedAt == nil || updated.SubscriptionPlanSource != "chatgpt_accounts" || updated.SubscriptionPlanStale {
				t.Fatalf("current plan did not replace old claim: %+v %v", updated, err)
			}
			if updated.Status != before.Status || updated.Operation != before.Operation || updated.Attempts != before.Attempts || !updated.LastOperationAt.Equal(before.LastOperationAt) {
				t.Fatal("read-only refresh changed login/operation history")
			}
			after, _ := os.ReadFile(f.path)
			if string(after) != string(authorization) {
				t.Fatal("query rewrote or rotated authorization")
			}
			public, _ := json.Marshal(updated)
			for _, secret := range []string{"synthetic-private-access", "synthetic-private-refresh", "synthetic-private-password"} {
				if strings.Contains(string(public), secret) {
					t.Fatal("query leaked authorization")
				}
			}
		})
	}
}

func TestRefreshAccountSubscriptionFailurePreservesHistoryAndNeverAuthenticates(t *testing.T) {
	for _, scenario := range []string{"expired", "invalidated-export", "forbidden", "unknown-plan", "replaced-grant"} {
		t.Run(scenario, func(t *testing.T) {
			f := subscriptionRefreshFixture(t, "plus")
			if scenario == "expired" {
				f.account["credentials"].(map[string]any)["expires_at"] = time.Now().Add(-time.Hour).Unix()
				f.write(t)
			}
			if scenario == "invalidated-export" {
				f.manager.UpdateResult(f.job.ID, false, "synthetic invalidated authorization")
			}
			before, _ := f.manager.Get(f.job.ID)
			authorization, _ := os.ReadFile(f.path)
			grant := f.load(t)
			calls := 0
			client := &protocol.InsightsClient{Transport: subscriptionRefreshDoer(func(_ context.Context, r transport.Request) (*transport.Response, error) {
				calls++
				if r.Method != http.MethodGet || !strings.HasPrefix(r.URL, "https://chatgpt.com/backend-api/") || strings.Contains(r.URL, "usage") {
					t.Fatal("refresh attempted authentication or another query component")
				}
				if scenario == "replaced-grant" {
					f.account["credentials"].(map[string]any)["access_token"] = "synthetic-new-grant"
					f.write(t)
					return subscriptionRefreshResponse(t, "pro", "2030-01-01T00:00:00Z"), nil
				}
				if scenario == "unknown-plan" {
					return subscriptionRefreshResponse(t, "future-unrecognized-plan", nil), nil
				}
				return &transport.Response{StatusCode: http.StatusForbidden, Header: http.Header{}, Body: []byte(`{"error":{"message":"synthetic-private-access synthetic-private-refresh"}}`)}, nil
			})}
			updated, _, err := RefreshAccountSubscription(t.Context(), grant, client, protocol.InsightsOptions{})
			if err == nil || updated != nil {
				t.Fatal("unconfirmed result was committed")
			}
			if (scenario == "expired" || scenario == "invalidated-export") && calls != 0 {
				t.Fatal("unusable saved authorization contacted upstream")
			}
			if scenario == "replaced-grant" && !errors.Is(err, jobs.ErrAccountQuerySuperseded) {
				t.Fatal("replaced grant did not invalidate observation", err)
			}
			if strings.Contains(err.Error(), "synthetic-private-") {
				t.Fatal("failure exposed upstream credentials")
			}
			after, _ := f.manager.Get(f.job.ID)
			// Public request diagnostics are the only added metadata on failure.
			before.AccountRequestObservations, after.AccountRequestObservations = nil, nil
			if !reflect.DeepEqual(before, after) {
				t.Fatal("failed query changed metadata or erased historical plan")
			}
			if scenario != "replaced-grant" {
				current, _ := os.ReadFile(f.path)
				if string(current) != string(authorization) {
					t.Fatal("failure modified saved authorization")
				}
			}
		})
	}
}

func TestRefreshAccountSubscriptionKeepsValidPlanWhenExpiryUnavailable(t *testing.T) {
	f := subscriptionRefreshFixture(t, "free")
	grant := f.load(t)
	client := &protocol.InsightsClient{Transport: subscriptionRefreshDoer(func(_ context.Context, r transport.Request) (*transport.Response, error) {
		if strings.Contains(r.URL, "accounts/check") {
			return subscriptionRefreshResponse(t, "plus", nil), nil
		}
		return &transport.Response{StatusCode: http.StatusServiceUnavailable, Header: http.Header{}, Body: []byte(`{"error":"temporary"}`)}, nil
	})}
	updated, result, err := RefreshAccountSubscription(t.Context(), grant, client, protocol.InsightsOptions{})
	if err != nil || updated.PlanType != "plus" || updated.SubscriptionExpiresAt != "" || len(result.Errors) != 1 || result.Errors[0].Code != "upstream_error" {
		t.Fatal("expiry outage erased the independently verified current plan", err)
	}
}

func TestRefreshAccountSubscriptionHonorsCallerAndLeaseCancellation(t *testing.T) {
	for _, cause := range []string{"caller", "lease"} {
		t.Run(cause, func(t *testing.T) {
			f := subscriptionRefreshFixture(t, "plus")
			grant := f.load(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			calls := 0
			client := &protocol.InsightsClient{Transport: subscriptionRefreshDoer(func(requestCtx context.Context, _ transport.Request) (*transport.Response, error) {
				calls++
				if cause == "caller" {
					cancel()
				} else {
					grant.Release()
				}
				select {
				case <-requestCtx.Done():
					return nil, requestCtx.Err()
				case <-time.After(time.Second):
					t.Fatal("upstream request did not inherit cancellation")
					return nil, errors.New("synthetic cancellation timeout")
				}
			})}
			want := error(context.Canceled)
			if cause == "lease" {
				want = jobs.ErrAccountQuerySuperseded
			}
			if updated, _, err := RefreshAccountSubscription(ctx, grant, client, protocol.InsightsOptions{}); !errors.Is(err, want) || updated != nil || calls != 1 {
				t.Fatal("wrong interruption outcome or request count", err, calls)
			}
			current, _ := f.manager.Get(f.job.ID)
			if current.PlanType != "plus" || current.SubscriptionPlanCheckedAt != nil {
				t.Fatal("canceled query replaced historical metadata")
			}
		})
	}
}
