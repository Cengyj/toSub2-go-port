package protocol

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"tosub2-go-port/internal/transport"
)

func TestInsightAccountsBindsOnlyExactWorkspace(t *testing.T) {
	creds := insightTestCredentials()
	body := `{"accounts":{
		"default":{"account":{"account_id":"personal","plan_type":"pro"},"entitlement":{"expires_at":"2035-01-01T00:00:00Z"}},
		"workspace":{"account":{"account_id":"workspace","plan_type":"plus","email":"OWNER@example.test"},"entitlement":{"expires_at":"2030-01-01T08:00:00+08:00"}}
	}}`
	got, err := parseInsightAccounts([]byte(body), creds, time.Now())
	if err != nil || !got.IdentityVerified || got.AccountID != creds.AccountID || got.PlanType != "plus" || got.ExpiresAt == nil || *got.ExpiresAt != "2030-01-01T00:00:00Z" || !got.Reported {
		t.Fatalf("wrong workspace or expiry selected: %+v %v", got, err)
	}
	aliased := strings.Replace(body, `"workspace":{"account"`, `"row-alias":{"account"`, 1)
	if got, err := parseInsightAccounts([]byte(aliased), creds, time.Now()); err != nil || got.AccountID != creds.AccountID {
		t.Fatalf("matching nested account ID not selected: %+v %v", got, err)
	}
	for _, body := range []string{
		`{"accounts":{"default":{"account":{"account_id":"personal","plan_type":"plus"},"entitlement":{"expires_at":"2030-01-01T00:00:00Z"}}}}`,
		`{"accounts":{"workspace":{"account":{"account_id":"other","plan_type":"plus"}}}}`,
		`{"accounts":{"workspace":{"account":{"account_id":"workspace","email":"other@example.test"}}}}`,
		`{"account_id":"other","accounts":{"workspace":{"account":{"plan_type":"plus"}}}}`,
	} {
		if _, err := parseInsightAccounts([]byte(body), creds, time.Now()); !errors.Is(err, errInsightIdentity) {
			t.Fatalf("foreign workspace accepted: %s %v", body, err)
		}
	}
	duplicate := `{"accounts":{"workspace":{"account":{"account_id":"workspace"}},"alias":{"account":{"account_id":"workspace"}}}}`
	if _, err := parseInsightAccounts([]byte(duplicate), creds, time.Now()); err == nil {
		t.Fatal("ambiguous matching account entries accepted")
	}
}

func TestInsightSubscriptionUnknownExpiryDoesNotBecomeAbsent(t *testing.T) {
	for _, tc := range []struct {
		body     string
		reported bool
	}{
		{`{"account_id":"workspace","plan_type":"plus"}`, false},
		{`{"account_id":"workspace","plan_type":"plus","active_until":null}`, true},
		{`{"account_id":"workspace","plan_type":"plus","active_until":""}`, true},
	} {
		got, err := parseInsightSubscription([]byte(tc.body), insightTestCredentials(), false, time.Now())
		if err != nil || !got.IdentityVerified || got.ExpiresAt != nil || got.Reported != tc.reported {
			t.Fatalf("unknown expiry semantics lost: %+v %v", got, err)
		}
	}
	for _, body := range []string{`{}`, `{"active_until":123}`, `{"active_until":"not-a-date"}`, `{"active_until":"0001-01-01T00:00:00Z"}`, `{"plan_type":true}`} {
		if _, err := parseInsightSubscription([]byte(body), insightTestCredentials(), true, time.Now()); err == nil {
			t.Fatalf("malformed subscription accepted: %s", body)
		}
	}
	if _, err := parseInsightSubscription([]byte(`{"account_id":"other","plan_type":"plus"}`), insightTestCredentials(), true, time.Now()); !errors.Is(err, errInsightIdentity) {
		t.Fatal("previous accounts check overrode conflicting subscription identity")
	}
	got, err := parseInsightSubscription([]byte(`{"plan_type":"plus","active_until":"2030-01-01T00:00:00Z"}`), insightTestCredentials(), false, time.Now())
	if err != nil || got.IdentityVerified || got.Scope != "unknown" {
		t.Fatalf("request account_id alone became returned identity proof: %+v %v", got, err)
	}
}

func TestInsightSubscriptionFallbackMaintainsScopeAndPartialResults(t *testing.T) {
	for _, tc := range []struct {
		name            string
		accounts        string
		accountsStatus  int
		subscription    string
		subStatus       int
		wantSource      string
		wantScope       string
		wantVerified    bool
		wantExpiry      bool
		wantError       string
		wantSubRequests int
	}{
		{"verified-primary", insightAccountsFixture("2030-01-01T00:00:00Z"), 200, "", 200, "chatgpt_accounts", "account", true, true, "", 0},
		{"verified-fallback", insightAccountsFixture(nil), 200, `{"plan_type":"plus","active_until":"2030-01-01T00:00:00Z"}`, 200, "chatgpt_subscriptions", "account", true, true, "", 1},
		{"null-fallback-is-reported", insightAccountsFixture(nil), 200, `{"plan_type":"plus","active_until":null}`, 200, "chatgpt_subscriptions", "account", true, false, "", 1},
		{"missing-fallback-preserves-explicit-unknown", insightAccountsFixture(nil), 200, `{"plan_type":"plus"}`, 200, "chatgpt_accounts", "account", true, false, "", 1},
		{"failed-fallback-retains-primary", insightAccountsFixture(nil), 200, `{"error":"unavailable"}`, 503, "chatgpt_accounts", "account", true, false, "upstream_error", 1},
		{"no-identity-proof", "{}", 403, `{"plan_type":"plus","active_until":"2030-01-01T00:00:00Z"}`, 200, "", "", false, false, "identity_unverified", 1},
		{"fallback-self-binds", "{}", 403, `{"account_id":"workspace","plan_type":"plus","active_until":"2030-01-01T00:00:00Z"}`, 200, "chatgpt_subscriptions", "account", true, true, "permission_denied", 1},
		{"saved-plan-conflict", strings.ReplaceAll(insightAccountsFixture("2030-01-01T00:00:00Z"), "plus", "team"), 200, "", 200, "chatgpt_accounts", "mismatch", false, false, "subscription_scope_mismatch", 0},
		{"fallback-plan-conflict", insightAccountsFixture(nil), 200, `{"plan_type":"team","active_until":"2030-01-01T00:00:00Z"}`, 200, "chatgpt_subscriptions", "mismatch", false, false, "subscription_scope_mismatch", 1},
		{"fallback-missing-plan", insightAccountsFixture(nil), 200, `{"active_until":"2030-01-01T00:00:00Z"}`, 200, "chatgpt_subscriptions", "unknown", false, false, "subscription_scope_mismatch", 1},
		{"fallback-wrong-workspace", insightAccountsFixture(nil), 200, `{"account_id":"other","plan_type":"plus","active_until":"2030-01-01T00:00:00Z"}`, 200, "chatgpt_accounts", "account", true, false, "identity_mismatch", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			subRequests := 0
			client := &InsightsClient{Transport: insightFixtureDoer(func(_ context.Context, r transport.Request) (*transport.Response, error) {
				if r.URL == insightAccountsURL {
					return insightResponse(tc.accountsStatus, tc.accounts), nil
				}
				if r.URL != insightSubscriptionsURL+"?account_id=workspace" {
					t.Fatalf("unexpected metadata endpoint: %s", r.URL)
				}
				subRequests++
				return insightResponse(tc.subStatus, tc.subscription), nil
			})}
			got, failures := client.querySubscription(context.Background(), insightTestCredentials(), InsightsOptions{}, nil, nil)
			if subRequests != tc.wantSubRequests {
				t.Fatalf("subscription requests=%d want=%d", subRequests, tc.wantSubRequests)
			}
			if tc.wantSource == "" {
				if got != nil {
					t.Fatalf("unverified result published: %+v", got)
				}
			} else if got == nil || got.Source != tc.wantSource || got.Scope != tc.wantScope || got.IdentityVerified != tc.wantVerified || (got.ExpiresAt != nil) != tc.wantExpiry {
				t.Fatalf("wrong partial metadata result: %+v", got)
			}
			if tc.name == "missing-fallback-preserves-explicit-unknown" && !got.Reported {
				t.Fatal("fallback omitted date erased authoritative unknown")
			}
			if tc.wantError == "" && len(failures) != 0 {
				t.Fatalf("unexpected errors: %+v", failures)
			}
			if tc.wantError != "" {
				found := false
				for _, failure := range failures {
					found = found || failure.Component == "subscription" && failure.Code == tc.wantError
				}
				if !found {
					t.Fatalf("missing %s in %+v", tc.wantError, failures)
				}
			}
		})
	}
}

func TestInsightAccountsRejectsPlanAndDateConflicts(t *testing.T) {
	for _, body := range []string{
		`{"accounts":{"workspace":{"account":{"plan_type":"team"},"entitlement":{"subscription_plan":"plus","expires_at":"2030-01-01T00:00:00Z"}}}}`,
		`{"accounts":{"workspace":{"account":{"plan_type":"future"},"entitlement":{"expires_at":"2030-01-01T00:00:00Z"}}}}`,
		`{"accounts":{"workspace":{"account":{"plan_type":"plus"},"entitlement":{"expires_at":"bad"}}}}`,
	} {
		if _, err := parseInsightAccounts([]byte(body), insightTestCredentials(), time.Now()); err == nil {
			t.Fatalf("conflicting metadata accepted: %s", body)
		}
	}
	fallback := `{"accounts":{"workspace":{"entitlement":{"subscription_plan":"chatgpt_plus","expires_at":null}}}}`
	got, err := parseInsightAccounts([]byte(fallback), insightTestCredentials(), time.Now())
	if err != nil || got.PlanType != "plus" || !got.Reported || got.ExpiresAt != nil {
		t.Fatalf("explicit entitlement metadata lost: %+v %v", got, err)
	}
}
