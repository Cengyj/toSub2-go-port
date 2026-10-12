package main

import (
	"encoding/base64"
	"testing"

	"tosub2-go-port/internal/accountplan"
	"tosub2-go-port/internal/protocol"
)

func planToken(plan string) string {
	return "e30." + base64.RawURLEncoding.EncodeToString([]byte(`{"email":"fixture@example.test","https://api.openai.com/auth":{"chatgpt_plan_type":"`+plan+`"}}`)) + ".synthetic"
}

func TestRefreshUsesFreshPlanAndPreservesKnownPlanWithoutClaims(t *testing.T) {
	for _, tc := range []struct{ id, access, want string }{
		{planToken("pro"), "new-access", "pro"},
		{"", planToken("business"), "team"},
		{"opaque-new-id", planToken("free"), "free"},
		{"", "opaque-new-access", "plus"},
	} {
		extra := map[string]any{"plan_type": "plus", "custom": "preserve"}
		account := map[string]any{
			"planType":    "plus",
			"extra":       extra,
			"credentials": map[string]any{"email": "fixture@example.test", "id_token": planToken("plus"), "refresh_token": "old-refresh", "chatgpt_plan_type": "plus"},
		}
		updated := updateRefreshedDocument(map[string]any{"accounts": []any{account}}, protocol.TokenSet{IDToken: tc.id, AccessToken: tc.access, RefreshToken: "new-refresh"}, "fixture-client")
		got := updated["accounts"].([]any)[0].(map[string]any)["extra"].(map[string]any)
		if got["plan_type"] != tc.want || got["custom"] != "preserve" {
			t.Fatalf("refreshed plan=%v, want %q; preserved custom=%v", got["plan_type"], tc.want, got["custom"])
		}
		if recognized := accountplan.FromAccount(account); recognized != tc.want {
			t.Fatalf("existing tier aliases took precedence over refreshed tier: %q", recognized)
		}
	}
}

func TestRefreshDoesNotReplaceExplicitTierWithOldTokenClaim(t *testing.T) {
	extra := map[string]any{"plan_type": "pro"}
	account := map[string]any{
		"extra":       extra,
		"credentials": map[string]any{"email": "fixture@example.test", "id_token": planToken("plus"), "refresh_token": "old-refresh"},
	}
	updateRefreshedDocument(map[string]any{"accounts": []any{account}}, protocol.TokenSet{AccessToken: "opaque-new-access", RefreshToken: "new-refresh"}, "fixture-client")
	if extra["plan_type"] != "pro" {
		t.Fatal("an old ID token erased the newer explicit plan")
	}
}

func TestRefreshUpdatesExistingFlattenedPlanClaims(t *testing.T) {
	for _, location := range []string{"account", "credentials", "extra"} {
		t.Run(location, func(t *testing.T) {
			account := map[string]any{
				"credentials": map[string]any{"email": "fixture@example.test", "id_token": planToken("plus"), "refresh_token": "old-refresh"},
				"extra":       map[string]any{},
			}
			fields := account
			if location != "account" {
				fields = account[location].(map[string]any)
			}
			fields["https://api.openai.com/auth.chatgpt_plan_type"] = "plus"
			updateRefreshedDocument(map[string]any{"accounts": []any{account}}, protocol.TokenSet{AccessToken: planToken("pro"), RefreshToken: "new-refresh"}, "fixture-client")
			if got := accountplan.FromAccount(account); got != "pro" {
				t.Fatalf("stale flattened %s claim shadowed refreshed plan: %q", location, got)
			}
			if got := fields["https://api.openai.com/auth.chatgpt_plan_type"]; got != "pro" {
				t.Fatalf("flattened %s claim was not updated: %v", location, got)
			}
		})
	}
}
