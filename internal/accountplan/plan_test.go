package accountplan

import (
	"encoding/base64"
	"strings"
	"testing"
)

func testJWT(claims string) string {
	return "e30." + base64.RawURLEncoding.EncodeToString([]byte(claims)) + ".synthetic"
}

func TestNormalizeDoesNotInferFree(t *testing.T) {
	for input, want := range map[string]string{
		" Free ": "free", "GO": "go", "chatgpt_plus": "plus", "pro": "pro",
		"team": "team", "Business": "team", "enterprise": "enterprise",
		"self_serve_business": "team", "self_serve_business_prolite": "business_premium",
		"prolite": "prolite", "Pro-Lite": "prolite", "pro_lite": "prolite", "business_premium": "business_premium",
		"education": "edu", "edu": "edu", "unknown": "unknown",
		"": "unknown", "unrecognized-provider-tier": "unknown", "token-secret": "unknown",
	} {
		if got := Normalize(input); got != want {
			t.Errorf("Normalize(%q)=%q, want %q", input, got, want)
		}
	}
}

func TestFromAccountUsesExplicitTierBeforeTokenClaims(t *testing.T) {
	account := map[string]any{
		"plan_type":   "business",
		"credentials": map[string]any{"chatgpt_plan_type": "plus", "id_token": testJWT(`{"https://api.openai.com/auth":{"chatgpt_plan_type":"free"}}`)},
		"extra":       map[string]any{"subscription_plan": "pro"},
	}
	if got := FromAccount(account); got != "team" {
		t.Fatalf("explicit remote account tier=%q", got)
	}
	delete(account, "plan_type")
	if got := FromAccount(account); got != "plus" {
		t.Fatalf("explicit credential tier=%q", got)
	}
	delete(account["credentials"].(map[string]any), "chatgpt_plan_type")
	if got := FromAccount(account); got != "pro" {
		t.Fatalf("explicit extra tier=%q", got)
	}
	delete(account, "extra")
	if got := FromAccount(account); got != "free" {
		t.Fatalf("token fallback tier=%q", got)
	}
}

func TestFromTokensMalformedOversizedAndAccessFallback(t *testing.T) {
	access := testJWT(`{"https://api.openai.com/auth":{"chatgpt_plan_type":"pro"}}`)
	id := testJWT(`{"https://api.openai.com/auth":{"chatgpt_plan_type":"plus"}}`)
	for _, tc := range []struct{ id, access, want string }{
		{id, access, "plus"}, {"opaque-id-token", access, "pro"},
		{testJWT(`{"email":"fixture@example.test"}`), access, "pro"},
		{testJWT(`{"chatgpt_plan_type":"enterprise"}`), "", "enterprise"},
		{testJWT(`{"https://api.openai.com/auth.chatgpt_plan_type":"team"}`), "", "team"},
		{testJWT(`{"https://api.openai.com/auth":{"chatgpt_plan_type":true}}`), "", "unknown"},
		{"e30.not_base64.signature", "", "unknown"},
		{testJWT(`{"chatgpt_plan_type":"plus","padding":"` + strings.Repeat("x", 128<<10) + `"}`), "", "unknown"},
		{"", "", "unknown"},
	} {
		if got := FromTokens(tc.id, tc.access); got != tc.want {
			t.Errorf("token extraction=%q, want %q", got, tc.want)
		}
	}
}
