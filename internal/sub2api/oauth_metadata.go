package sub2api

import (
	"strings"

	"tosub2-go-port/internal/protocol"
)

// normalizeOAuthAuthorization upgrades older exports from their own token
// claims. Sub2API may already have enriched these fields, so explicit values
// take precedence. The tokens themselves are never logged or returned.
func normalizeOAuthAuthorization(credentials, extra map[string]any) error {
	client, err := oauthClientID(credentials, extra)
	if err != nil {
		return err
	}
	metadata := protocol.ExtractOAuthTokenMetadata(protocol.TokenSet{
		AccessToken: stringValue(credentials["access_token"]),
		IDToken:     stringValue(credentials["id_token"]),
	})
	for field, value := range map[string]string{
		"chatgpt_account_id":      metadata.ChatGPTAccountID,
		"chatgpt_user_id":         metadata.ChatGPTUserID,
		"email":                   metadata.Email,
		"organization_id":         metadata.OrganizationID,
		"plan_type":               metadata.PlanType,
		"subscription_expires_at": metadata.SubscriptionExpiresAt,
	} {
		if value != "" && (credentials[field] == nil || credentials[field] == "") {
			credentials[field] = value
		}
	}
	if metadata.ExpiresAt > 0 && (credentials["expires_at"] == nil || credentials["expires_at"] == "") {
		credentials["expires_at"] = metadata.ExpiresAt
	}
	if client != "" {
		credentials["client_id"] = client
		protocol.ApplyBPSCredentialSettings(credentials, client)
		applyOAuthClientMetadata(extra, client)
	}
	return nil
}

func applyOAuthClientMetadata(extra map[string]any, client string) {
	client = strings.TrimSpace(client)
	if client == "" {
		return
	}
	extra["client_id"] = client
	extra["openai_excel_bps"] = client == protocol.DefaultBPSClientID
	protocol.ApplyBPSSettings(extra, client)
}

func oauthAccountID(credentials, extra map[string]any) string {
	for _, source := range []map[string]any{credentials, extra} {
		for _, field := range []string{"chatgpt_account_id", "account_id"} {
			if value := strings.TrimSpace(stringValue(source[field])); value != "" {
				return value
			}
		}
	}
	return ""
}

func oauthAccountMetadataChanged(remote, extra, incoming map[string]any) bool {
	previousAccount, nextAccount := oauthAccountID(remote, extra), oauthAccountID(incoming, nil)
	if previousAccount != "" && nextAccount != "" && previousAccount != nextAccount {
		return true
	}
	client := strings.TrimSpace(stringValue(incoming["client_id"]))
	previousClient, err := oauthClientID(remote, extra)
	return client != "" && (err != nil || client != previousClient) && (nextAccount == "" || nextAccount != previousAccount)
}

func oauthPlanChanged(remote, extra, incoming map[string]any) bool {
	previous := strings.TrimSpace(stringValue(remote["plan_type"]))
	if previous == "" {
		previous = strings.TrimSpace(stringValue(extra["plan_type"]))
	}
	next := strings.TrimSpace(stringValue(incoming["plan_type"]))
	return previous != "" && next != "" && !strings.EqualFold(previous, next)
}

// mergeOAuthExtra keeps remote routing settings while updating the identity
// that owns the new grant. Existing BPS settings win over template defaults.
func mergeOAuthExtra(remote, extra, incoming map[string]any) map[string]any {
	merged := cloneMap(extra)
	if oauthAccountMetadataChanged(remote, extra, incoming) {
		for _, field := range []string{"account_id", "chatgpt_account_id", "chatgpt_user_id", "user_id", "organization_id", "plan_type", "subscription_expires_at"} {
			delete(merged, field)
		}
	}
	if oauthPlanChanged(remote, extra, incoming) && stringValue(incoming["subscription_expires_at"]) == "" {
		delete(merged, "subscription_expires_at")
	}
	// Only keep existing extra aliases in sync. Credentials are the canonical
	// location and new accounts need no duplicate metadata keys.
	for _, field := range []string{"chatgpt_account_id", "chatgpt_user_id", "email", "organization_id", "plan_type", "subscription_expires_at"} {
		if _, exists := extra[field]; exists {
			if value := stringValue(incoming[field]); value != "" {
				merged[field] = value
			}
		}
	}
	for alias, field := range map[string]string{"account_id": "chatgpt_account_id", "user_id": "chatgpt_user_id"} {
		if _, exists := extra[alias]; exists {
			if value := stringValue(incoming[field]); value != "" {
				merged[alias] = value
			}
		}
	}
	applyOAuthClientMetadata(merged, stringValue(incoming["client_id"]))
	return merged
}
