// Package accountplan extracts non-secret subscription metadata. It never
// treats decoded JWT claims as proof of identity or authorization.
package accountplan

import (
	"encoding/base64"
	"encoding/json"
	"strings"
)

// Normalize returns a known subscription tier, or unknown when the
// provider did not report a recognized tier. Unknown is never inferred as free.
func Normalize(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.TrimPrefix(value, "chatgpt_")
	switch value {
	case "free", "go", "plus", "pro", "prolite", "business_premium", "team", "enterprise", "edu":
		return value
	case "business", "self_serve_business":
		return "team"
	case "pro-lite", "pro_lite":
		return "prolite"
	case "self_serve_business_prolite":
		return "business_premium"
	case "education":
		return "edu"
	default:
		return "unknown"
	}
}

func fromFields(fields map[string]any) string {
	for _, key := range []string{"chatgpt_plan_type", "plan_type", "planType", "subscription_plan", "https://api.openai.com/auth.chatgpt_plan_type"} {
		if value, ok := fields[key].(string); ok {
			if plan := Normalize(value); plan != "unknown" {
				return plan
			}
		}
	}
	return "unknown"
}

// FromAccount checks explicit account metadata before the tokens carried by
// the account. The API's explicit tier can be newer than its saved JWT claims.
func FromAccount(account map[string]any) string {
	credentials, _ := account["credentials"].(map[string]any)
	extra, _ := account["extra"].(map[string]any)
	for _, fields := range []map[string]any{account, credentials, extra} {
		if plan := fromFields(fields); plan != "unknown" {
			return plan
		}
	}
	idToken, _ := credentials["id_token"].(string)
	accessToken, _ := credentials["access_token"].(string)
	return FromTokens(idToken, accessToken)
}

// FromTokens reads the plan claim from ID and access tokens without retaining
// the tokens or claims. Malformed and oversized tokens yield unknown metadata.
func FromTokens(idToken, accessToken string) string {
	for _, token := range []string{idToken, accessToken} {
		if len(token) > 128<<10 {
			continue
		}
		parts := strings.Split(token, ".")
		if len(parts) != 3 {
			continue
		}
		payload, err := base64.RawURLEncoding.DecodeString(parts[1])
		if err != nil {
			continue
		}
		var claims map[string]any
		if json.Unmarshal(payload, &claims) != nil {
			continue
		}
		auth, _ := claims["https://api.openai.com/auth"].(map[string]any)
		for _, fields := range []map[string]any{auth, claims} {
			if plan := fromFields(fields); plan != "unknown" {
				return plan
			}
		}
	}
	return "unknown"
}
