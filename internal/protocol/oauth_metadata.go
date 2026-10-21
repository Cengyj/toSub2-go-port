package protocol

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"time"

	"tosub2-go-port/internal/accountplan"
)

// OAuthTokenMetadata contains provider-reported export metadata. Decoding the
// claims does not verify their signature or grant authorization.
type OAuthTokenMetadata struct {
	ChatGPTAccountID      string
	ChatGPTUserID         string
	Email                 string
	ExpiresAt             int64
	OrganizationID        string
	PlanType              string
	SubscriptionExpiresAt string
	// Reported distinguishes an omitted field from an explicit unknown
	// (null, empty or invalid). An explicit unknown must clear stale dates.
	SubscriptionExpiryReported bool
}

// ExtractOAuthTokenMetadata prefers access-token claims because a refresh may
// retain an older ID token. ExpiresAt is exclusively the access token's exp;
// absent expiry is left unknown rather than extending a saved grant by its TTL.
func ExtractOAuthTokenMetadata(tokens TokenSet) OAuthTokenMetadata {
	access, _ := DecodeJWTClaims(tokens.AccessToken)
	id, _ := DecodeJWTClaims(tokens.IDToken)
	claims := []map[string]any{access, id}
	m := OAuthTokenMetadata{ExpiresAt: claimUnixSeconds(access["exp"])}
	for _, c := range claims {
		if m.ChatGPTAccountID == "" {
			m.ChatGPTAccountID = claimAccountID(c)
		}
		if m.Email == "" {
			m.Email = claimsEmail(c)
		}
	}
	// OpenAI's generic user_id and OIDC sub can differ from chatgpt_user_id.
	// Prefer the dedicated ChatGPT identity even when it is only in the ID token.
	for _, key := range []string{"chatgpt_user_id", "user_id"} {
		for _, c := range claims {
			if m.ChatGPTUserID == "" {
				m.ChatGPTUserID = metadataString(c, key)
			}
		}
	}
	if m.ChatGPTUserID == "" {
		for _, c := range claims {
			if m.ChatGPTUserID == "" {
				m.ChatGPTUserID, _ = c["sub"].(string)
			}
		}
	}
	accountScopedClaims := claims
	if accessAccount, idAccount := claimAccountID(access), claimAccountID(id); accessAccount != "" && idAccount != "" && accessAccount != idAccount {
		// A retained ID token can describe a workspace that was not selected
		// for the new grant. Its subscription and organization belong there.
		accountScopedClaims = []map[string]any{access}
	}
	for _, c := range accountScopedClaims {
		if m.PlanType == "" {
			m.PlanType = metadataString(c, "chatgpt_plan_type", "plan_type")
		}
	}
	subscriptionClaims := accountScopedClaims
	if accessPlan, idPlan := metadataPlan(access), metadataPlan(id); accessPlan != "" && idPlan != "" && accessPlan != idPlan {
		// A retained ID token can describe an older subscription even when
		// both tokens belong to the same workspace.
		subscriptionClaims = []map[string]any{access}
	}
	for _, c := range subscriptionClaims {
		if value, reported := metadataSubscriptionExpiry(c); reported {
			m.SubscriptionExpiresAt, m.SubscriptionExpiryReported = value, true
			break
		}
	}
	for _, c := range accountScopedClaims {
		if org, reported := metadataOrganization(c); reported {
			m.OrganizationID = org
			break
		}
	}
	return m
}

func metadataFields(claims map[string]any) []map[string]any {
	auth, _ := claims["https://api.openai.com/auth"].(map[string]any)
	shortAuth, _ := claims["auth"].(map[string]any)
	return []map[string]any{auth, shortAuth, claims}
}

func metadataString(claims map[string]any, keys ...string) string {
	for _, fields := range metadataFields(claims) {
		for _, key := range keys {
			if value, _ := fields[key].(string); strings.TrimSpace(value) != "" {
				return strings.TrimSpace(value)
			}
		}
	}
	return ""
}

func metadataPlan(claims map[string]any) string {
	value := metadataString(claims, "chatgpt_plan_type", "plan_type")
	if normalized := accountplan.Normalize(value); normalized != "unknown" {
		return normalized
	}
	return strings.ToLower(value)
}

func metadataSubscriptionExpiry(claims map[string]any) (string, bool) {
	for _, fields := range metadataFields(claims) {
		for _, key := range []string{"subscription_expires_at", "chatgpt_subscription_active_until"} {
			value, reported := fields[key]
			if !reported {
				continue
			}
			text, _ := value.(string)
			expires, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(text))
			if err != nil || expires.IsZero() || expires.UTC().Year() < 1 || expires.UTC().Year() > 9999 {
				// Optional metadata must never prevent an accepted grant from
				// being saved, or fall back to a stale value in an older token.
				return "", true
			}
			return expires.UTC().Format(time.RFC3339Nano), true
		}
	}
	return "", false
}

func metadataOrganization(claims map[string]any) (string, bool) {
	if id := metadataString(claims, "organization_id"); id != "" {
		return id, true
	}
	for _, fields := range metadataFields(claims) {
		organizations, reported := fields["organizations"]
		if !reported {
			continue
		}
		list, ok := organizations.([]any)
		if !ok {
			return "", true
		}
		ids, defaults := map[string]bool{}, map[string]bool{}
		for _, value := range list {
			organization, _ := value.(map[string]any)
			id, _ := organization["id"].(string)
			id = strings.TrimSpace(id)
			if id == "" {
				continue
			}
			ids[id] = true
			if isDefault, _ := organization["is_default"].(bool); isDefault {
				defaults[id] = true
			}
		}
		if len(defaults) == 1 {
			for id := range defaults {
				return id, true
			}
		}
		if len(ids) == 1 {
			for id := range ids {
				return id, true
			}
		}
		// Do not guess among multiple organizations or fall back to stale ID
		// token organizations when the newer access token is ambiguous.
		return "", true
	}
	return "", false
}

func claimUnixSeconds(value any) int64 {
	var result int64
	switch v := value.(type) {
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) || v <= 0 || v >= math.MaxInt64 || math.Trunc(v) != v {
			return 0
		}
		result = int64(v)
	case json.Number:
		result, _ = v.Int64()
	case string:
		result, _ = strconv.ParseInt(v, 10, 64)
	}
	if result <= 0 {
		return 0
	}
	return result
}
