package protocol

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"tosub2-go-port/internal/transport"
)

func TestQuerySubscriptionReadsCurrentPlanWithoutChangingHistoricalCredentials(t *testing.T) {
	for _, tc := range []struct {
		name, savedPlan, livePlan string
	}{
		{"upgrade", "free", "plus"},
		{"downgrade", "plus", "free"},
		{"missing-saved-plan", "", "pro"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			creds := insightTestCredentials()
			creds.PlanType = tc.savedPlan
			creds.AuthorizationMethod = AuthorizationMethodBPS
			saved := creds
			options := InsightsOptions{IncludeResetCredits: true, SkipComponents: []string{"subscription"}}
			calls := 0
			started := time.Now().UTC()
			client := &InsightsClient{Transport: insightFixtureDoer(func(_ context.Context, r transport.Request) (*transport.Response, error) {
				calls++
				if r.URL != insightAccountsURL || r.Method != http.MethodGet || len(r.Body) != 0 || !r.SingleAttempt {
					t.Fatal("subscription query dispatched a forbidden or mutable request")
				}
				if r.Headers.Get("Authorization") != "Bearer "+saved.AccessToken || r.Headers.Get("Chatgpt-Account-Id") != saved.AccountID || r.Headers.Get("Cookie") != "" || r.Headers.Get("Openai-Sentinel-Token") != "" {
					t.Fatal("query lost grant binding or borrowed an authentication session")
				}
				return insightResponse(200, strings.ReplaceAll(insightAccountsFixture("2030-01-01T00:00:00Z"), "plus", tc.livePlan)), nil
			})}
			got := client.QuerySubscription(context.Background(), creds, options)
			if len(got.Errors) != 0 || calls != 1 || got.Subscription == nil || got.Subscription.PlanType != tc.livePlan || !got.Subscription.IdentityVerified || got.Subscription.Scope != "account" || got.Subscription.AccountID != saved.AccountID {
				t.Fatalf("live plan rejected because of stale token metadata: %+v", got)
			}
			if got.Subscription.Source != "chatgpt_accounts" || got.Subscription.ObservedAt.Before(started) || got.Subscription.ObservedAt.After(time.Now().UTC()) || got.Usage != nil || got.BPS != nil || !reflect.DeepEqual(got.AttemptedComponents, []string{"subscription"}) {
				t.Fatalf("observation source, time, or component isolation lost: %+v", got)
			}
			if creds != saved || !options.IncludeResetCredits || !reflect.DeepEqual(options.SkipComponents, []string{"subscription"}) {
				t.Fatal("manual query changed the supplied grant or options")
			}
		})
	}
}

func TestQuerySubscriptionKeepsLiveSourceAndIdentityChecks(t *testing.T) {
	for _, tc := range []struct {
		name, accounts, subscription, wantCode, wantPlan string
		wantVerified                                     bool
	}{
		{"live-plan-conflict", insightAccountsFixture(nil), `{"account_id":"workspace","plan_type":"team","active_until":"2030-01-01T00:00:00Z"}`, "subscription_scope_mismatch", "team", false},
		{"both-cross-account", `{"accounts":{"other":{"account":{"account_id":"other","plan_type":"plus"}}}}`, `{"account_id":"other","plan_type":"plus"}`, "identity_mismatch", "", false},
		{"fallback-cross-account-keeps-only-matching-primary", insightAccountsFixture(nil), `{"account_id":"other","plan_type":"team"}`, "identity_mismatch", "plus", true},
		{"conflicting-fallback-alias", insightAccountsFixture(nil), `{"account_id":"workspace","chatgpt_account_id":"other","plan_type":"plus"}`, "identity_mismatch", "plus", true},
		{"matching-id-foreign-email", `{"accounts":{"workspace":{"account":{"account_id":"workspace","email":"foreign@example.test","plan_type":"plus"}}}}`, `{"account_id":"workspace","email":"foreign@example.test","plan_type":"plus"}`, "identity_mismatch", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &InsightsClient{Transport: insightFixtureDoer(func(_ context.Context, r transport.Request) (*transport.Response, error) {
				switch r.URL {
				case insightAccountsURL:
					return insightResponse(200, tc.accounts), nil
				case insightSubscriptionsURL + "?account_id=workspace":
					return insightResponse(200, tc.subscription), nil
				default:
					t.Fatal("unexpected non-subscription request")
					return nil, nil
				}
			})}
			got := client.QuerySubscription(context.Background(), insightTestCredentials(), InsightsOptions{})
			if !subscriptionRefreshHasCode(got, tc.wantCode) {
				t.Fatalf("missing %s: %+v", tc.wantCode, got.Errors)
			}
			if tc.wantPlan == "" {
				if got.Subscription != nil {
					t.Fatal("foreign subscription was published")
				}
			} else if got.Subscription == nil || got.Subscription.PlanType != tc.wantPlan || got.Subscription.IdentityVerified != tc.wantVerified || got.Subscription.ExpiresAt != nil {
				t.Fatalf("unverified or conflicting fallback changed primary observation: %+v", got.Subscription)
			}
		})
	}
}

func TestQuerySubscriptionRetainsPlanWhenExpiryQueryFails(t *testing.T) {
	for _, tc := range []struct {
		name, body, code string
		status           int
		err              error
	}{
		{"http", `{"error":{"message":"synthetic-access-secret"}}`, "upstream_error", 503, nil},
		{"network", "", "network_error", 0, errors.New("synthetic-access-secret")},
		{"malformed", `{"active_until":123}`, "protocol_error", 200, nil},
		{"html", "<html>synthetic-access-secret</html>", "challenge_or_html", 200, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &InsightsClient{Transport: insightFixtureDoer(func(_ context.Context, r transport.Request) (*transport.Response, error) {
				if r.URL == insightAccountsURL {
					return insightResponse(200, insightAccountsFixture(nil)), nil
				}
				if r.URL != insightSubscriptionsURL+"?account_id=workspace" {
					t.Fatal("expiry failure triggered non-subscription request")
				}
				if tc.err != nil {
					return nil, tc.err
				}
				return insightResponse(tc.status, tc.body), nil
			})}
			got := client.QuerySubscription(context.Background(), insightTestCredentials(), InsightsOptions{})
			if got.Subscription == nil || got.Subscription.PlanType != "plus" || got.Subscription.Source != "chatgpt_accounts" || !got.Subscription.IdentityVerified || got.Subscription.ExpiresAt != nil || !subscriptionRefreshHasCode(got, tc.code) {
				t.Fatalf("expiry failure discarded valid plan or manufactured date: %+v", got)
			}
			encoded, _ := json.Marshal(got)
			if strings.Contains(string(encoded), "secret") {
				t.Fatal("upstream details or grant leaked in public result")
			}
		})
	}
}

func TestQuerySubscriptionRetainsKnownPlanWhenFallbackCannotBindItsTier(t *testing.T) {
	for _, body := range []string{
		`{"active_until":"2030-01-01T00:00:00Z"}`,
		`{"account_id":"workspace","active_until":null}`,
		`{"account_id":"workspace","plan_type":"unknown"}`,
		`{"account_id":"workspace","plan_type":"future-tier","active_until":"2030-01-01T00:00:00Z"}`,
	} {
		t.Run(body, func(t *testing.T) {
			client := &InsightsClient{Transport: insightFixtureDoer(func(_ context.Context, r transport.Request) (*transport.Response, error) {
				if r.URL == insightAccountsURL {
					return insightResponse(200, insightAccountsFixture(nil)), nil
				}
				if r.URL != insightSubscriptionsURL+"?account_id=workspace" {
					t.Fatal("unexpected non-subscription request")
				}
				return insightResponse(200, body), nil
			})}
			got := client.QuerySubscription(context.Background(), insightTestCredentials(), InsightsOptions{})
			if got.Subscription == nil || got.Subscription.PlanType != "plus" || !got.Subscription.IdentityVerified || got.Subscription.Source != "chatgpt_accounts" || got.Subscription.ExpiresAt != nil || got.Subscription.Scope != "account" || !subscriptionRefreshHasCode(got, "subscription_scope_mismatch") {
				t.Fatalf("optional expiry source erased verified plan or donated unbound date: %+v", got)
			}
		})
	}
}

func TestQuerySubscriptionDoesNotRecoverRejectedAuthorizationOrAssumeFree(t *testing.T) {
	for _, tc := range []struct {
		name           string
		status         int
		body, wantCode string
	}{
		{"unauthorized", 401, `{"error":{"message":"synthetic-access-secret refresh_token=private"}}`, "authorization_rejected"},
		{"forbidden", 403, `{"error":{"message":"synthetic-access-secret cookie=private"}}`, "permission_denied"},
		{"unknown-fields", 200, `{"future_subscription":"free"}`, "protocol_error"},
		{"unknown-tier", 200, `{"account_id":"workspace","plan_type":"future-tier"}`, "protocol_error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			client := &InsightsClient{Transport: insightFixtureDoer(func(_ context.Context, r transport.Request) (*transport.Response, error) {
				calls++
				if r.Method != http.MethodGet || (r.URL != insightAccountsURL && r.URL != insightSubscriptionsURL+"?account_id=workspace") || len(r.Body) != 0 {
					t.Fatal("query attempted login, refresh, or another component")
				}
				return insightResponse(tc.status, tc.body), nil
			})}
			got := client.QuerySubscription(context.Background(), insightTestCredentials(), InsightsOptions{IncludeResetCredits: true})
			if calls != 2 || !subscriptionRefreshHasCode(got, tc.wantCode) || got.Usage != nil || got.BPS != nil || got.Subscription != nil && got.Subscription.PlanType != "unknown" {
				t.Fatalf("rejection was recovered or unknown became a known tier: %+v calls=%d", got, calls)
			}
			encoded, _ := json.Marshal(got)
			if strings.Contains(string(encoded), "secret") || strings.Contains(string(encoded), "private") || strings.Contains(string(encoded), "refresh_token") {
				t.Fatal("rejection leaked upstream credentials")
			}
		})
	}
}

func TestQuerySubscriptionRejectsAuthenticationClientAndInvalidGrantBeforeDispatch(t *testing.T) {
	got := (&InsightsClient{Transport: &Client{}}).QuerySubscription(context.Background(), insightTestCredentials(), InsightsOptions{})
	if !subscriptionRefreshHasCode(got, "unsafe_transport") || len(got.AttemptedComponents) != 0 {
		t.Fatal("subscription wrapper bypassed the authentication client guard")
	}
	client := &InsightsClient{Transport: insightFixtureDoer(func(context.Context, transport.Request) (*transport.Response, error) {
		t.Fatal("invalid credentials dispatched")
		return nil, nil
	})}
	creds := insightTestCredentials()
	creds.AccessToken = ""
	got = client.QuerySubscription(context.Background(), creds, InsightsOptions{})
	if !subscriptionRefreshHasCode(got, "invalid_credentials") || len(got.AttemptedComponents) != 0 {
		t.Fatal("invalid grant was admitted")
	}
}

func subscriptionRefreshHasCode(result InsightResult, code string) bool {
	for _, failure := range result.Errors {
		if failure.Code == code {
			return true
		}
	}
	return false
}
