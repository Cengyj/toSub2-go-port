package protocol

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"time"

	"tosub2-go-port/internal/accountplan"
)

func (c *InsightsClient) querySubscription(ctx context.Context, creds InsightCredentials, options InsightsOptions, failures []InsightError, attempted func(string)) (*InsightSubscription, []InsightError) {
	var account *InsightSubscription
	if body, failure := c.getWithAttempt(ctx, "subscription", insightAccountsURL, creds, options, 5*time.Second, attempted); failure != nil {
		failures = append(failures, *failure)
	} else if parsed, err := parseInsightAccounts(body, creds, time.Now().UTC()); err != nil {
		failures = append(failures, insightParseFailure("subscription", err))
	} else {
		account = parsed
	}
	if account != nil && insightSubscriptionPlanConflict(creds.PlanType, account.PlanType) {
		account.ExpiresAt = nil
		account.IdentityVerified = false
		account.Scope = "mismatch"
		failures = append(failures, insightFailure("subscription", "subscription_scope_mismatch"))
		return account, failures
	}
	if account != nil && account.ExpiresAt != nil {
		return account, failures
	}
	endpoint := insightSubscriptionsURL + "?" + url.Values{"account_id": {creds.AccountID}}.Encode()
	if body, failure := c.getWithAttempt(ctx, "subscription", endpoint, creds, options, 5*time.Second, attempted); failure != nil {
		return account, append(failures, *failure)
	} else {
		parsed, err := parseInsightSubscription(body, creds, account != nil && account.IdentityVerified, time.Now().UTC())
		if err != nil {
			return account, append(failures, insightParseFailure("subscription", err))
		}
		if !parsed.IdentityVerified {
			return account, append(failures, insightFailure("subscription", "identity_unverified"))
		}
		if insightSubscriptionPlanConflict(creds.PlanType, parsed.PlanType) || account != nil && insightSubscriptionPlanConflict(account.PlanType, parsed.PlanType) {
			parsed.ExpiresAt = nil
			parsed.IdentityVerified = false
			parsed.Scope = "mismatch"
			return parsed, append(failures, insightFailure("subscription", "subscription_scope_mismatch"))
		}
		// A date without a reported plan cannot inherit a potentially unrelated
		// personal/workspace tier from another endpoint or the saved grant.
		if parsed.ExpiresAt != nil && (parsed.PlanType == "" || parsed.PlanType == "unknown") {
			parsed.ExpiresAt = nil
			parsed.IdentityVerified = false
			parsed.Scope = "unknown"
			return parsed, append(failures, insightFailure("subscription", "subscription_scope_mismatch"))
		}
		// A fallback that omits its date must not erase an authoritative
		// unknown from the accounts endpoint and revive an older saved date.
		if account != nil && account.Reported && !parsed.Reported {
			return account, failures
		}
		return parsed, failures
	}
}

func parseInsightAccounts(body []byte, creds InsightCredentials, now time.Time) (*InsightSubscription, error) {
	f, err := insightObject(body)
	if err != nil {
		return nil, err
	}
	if err := insightIdentity(f, creds); err != nil {
		return nil, err
	}
	accounts, err := insightNested(f, "accounts")
	if err != nil {
		return nil, err
	}
	if accounts == nil {
		return nil, errInsightProtocol
	}
	if len(accounts) > 256 {
		return nil, errInsightProtocol
	}
	var selected map[string]json.RawMessage
	for key, raw := range accounts {
		entry, err := insightObject(raw)
		if err != nil {
			return nil, err
		}
		account, err := insightNested(entry, "account")
		if err != nil {
			return nil, err
		}
		id, err := insightField[string](account, "account_id")
		if err != nil {
			return nil, err
		}
		matches := key == creds.AccountID || id != nil && *id == creds.AccountID
		if !matches {
			continue
		}
		if id != nil && *id != "" && *id != creds.AccountID {
			return nil, errInsightIdentity
		}
		if selected != nil {
			return nil, errInsightProtocol
		}
		if err := insightIdentity(entry, creds); err != nil {
			return nil, err
		}
		if err := insightIdentity(account, creds); err != nil {
			return nil, err
		}
		selected = entry
	}
	if selected == nil {
		return nil, errInsightIdentity
	}
	account, _ := insightNested(selected, "account")
	entitlement, err := insightNested(selected, "entitlement")
	if err != nil {
		return nil, err
	}
	plan, err := insightField[string](account, "plan_type")
	if err != nil {
		return nil, err
	}
	entitlementPlan, err := insightField[string](entitlement, "subscription_plan")
	if err != nil {
		return nil, err
	}
	if plan == nil || strings.TrimSpace(*plan) == "" {
		plan = entitlementPlan
	} else if entitlementPlan != nil && insightSubscriptionPlanConflict(*plan, *entitlementPlan) {
		return nil, errInsightProtocol
	}
	result := &InsightSubscription{Source: "chatgpt_accounts", ObservedAt: now, AccountID: creds.AccountID, Scope: "account", IdentityVerified: true}
	if plan != nil {
		result.PlanType = accountplan.Normalize(*plan)
	}
	result.ExpiresAt, result.Reported, err = insightExpiry(entitlement, "expires_at")
	if err != nil {
		return nil, err
	}
	if result.ExpiresAt != nil && (result.PlanType == "" || result.PlanType == "unknown") {
		return nil, errInsightProtocol
	}
	return result, nil
}

func parseInsightSubscription(body []byte, creds InsightCredentials, accountVerified bool, now time.Time) (*InsightSubscription, error) {
	f, err := insightObject(body)
	if err != nil {
		return nil, err
	}
	if err := insightIdentity(f, creds); err != nil {
		return nil, err
	}
	_, hasPlan := f["plan_type"]
	_, hasExpiry := f["active_until"]
	if !hasPlan && !hasExpiry {
		return nil, errInsightProtocol
	}
	for _, key := range []string{"account_id", "chatgpt_account_id", "accountId"} {
		id, err := insightField[string](f, key)
		if err != nil {
			return nil, err
		}
		if id != nil && *id == creds.AccountID {
			accountVerified = true
		}
	}
	result := &InsightSubscription{Source: "chatgpt_subscriptions", ObservedAt: now, AccountID: creds.AccountID, Scope: "account", IdentityVerified: accountVerified}
	if !accountVerified {
		result.Scope = "unknown"
	}
	plan, err := insightField[string](f, "plan_type")
	if err != nil {
		return nil, err
	}
	if plan != nil {
		result.PlanType = accountplan.Normalize(*plan)
	}
	result.ExpiresAt, result.Reported, err = insightExpiry(f, "active_until")
	if err != nil {
		return nil, err
	}
	return result, nil
}

func insightSubscriptionPlanConflict(a, b string) bool {
	a, b = accountplan.Normalize(a), accountplan.Normalize(b)
	return a != "unknown" && b != "unknown" && a != b
}

func insightExpiry(f map[string]json.RawMessage, key string) (*string, bool, error) {
	_, reported := f[key]
	value, err := insightField[string](f, key)
	if err != nil {
		return nil, reported, err
	}
	if value == nil || strings.TrimSpace(*value) == "" {
		return nil, reported, nil
	}
	parsed, err := time.Parse(time.RFC3339, *value)
	if err != nil || parsed.Year() < 1970 || parsed.Year() > 9999 {
		return nil, reported, errInsightProtocol
	}
	utc := parsed.UTC().Format(time.RFC3339)
	return &utc, reported, nil
}
