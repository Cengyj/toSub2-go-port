package main

import (
	"encoding/base64"
	"reflect"
	"testing"

	"tosub2-go-port/internal/protocol"
)

func metadataFixtureToken(claims string) string {
	return "e30." + base64.RawURLEncoding.EncodeToString([]byte(claims)) + ".fixture"
}

func TestBPSRefreshUsesNewAccessMetadataAndPreservesSub2Settings(t *testing.T) {
	oldID := metadataFixtureToken(`{"exp":1900000000,"email":"owner@example.test","https://api.openai.com/auth":{"chatgpt_account_id":"workspace","chatgpt_plan_type":"plus","organizations":[{"id":"old-org","is_default":true}]}}`)
	creds := map[string]any{
		"email": "owner@example.test", "chatgpt_account_id": "workspace", "client_id": protocol.DefaultBPSClientID,
		"access_token": "old-access", "refresh_token": "old-refresh", "id_token": oldID,
		"expires_at": 1890000000, "expires_in": 900, "organization_id": "existing-org",
		"plan_type": "self_serve_business_prolite", "subscription_expires_at": "2030-01-01T00:00:00Z",
	}
	extra := map[string]any{"codex_fingerprint_seed": "synthetic-seed", "cost_multiplier": 0.1, "privacy_mode": "training_off", "openai_excel_bps_models": []string{"fixture-model"}, "openai_excel_bps": false}
	extra["openai_excel_bps_ignore_encrypted_content"] = false
	extra["openai_excel_bps_config_mode"] = "defaults"
	extra["openai_excel_bps_403_target_group_id"] = 17
	creds["model_mapping_mode"] = "passthrough"
	account := map[string]any{"name": "Keep custom name", "credentials": creds, "extra": extra, "concurrency": 100, "group_rate_multiplier": 0.5}
	access := metadataFixtureToken(`{"exp":2000000000,"https://api.openai.com/auth":{"chatgpt_account_id":"workspace","chatgpt_user_id":"new-user","chatgpt_plan_type":"self_serve_business_prolite"},"https://api.openai.com/profile":{"email":"owner@example.test"}}`)
	updateRefreshedDocument(map[string]any{"accounts": []any{account}}, protocol.TokenSet{AccessToken: access, RefreshToken: "new-refresh"}, protocol.DefaultBPSClientID)
	for field, want := range map[string]any{"expires_at": int64(2000000000), "refresh_token": "new-refresh", "id_token": oldID, "chatgpt_user_id": "new-user", "organization_id": "existing-org", "plan_type": "self_serve_business_prolite", "subscription_expires_at": "2030-01-01T00:00:00Z"} {
		if creds[field] != want {
			t.Errorf("wrong refreshed %s", field)
		}
	}
	if _, exists := creds["expires_in"]; exists {
		t.Fatal("refresh retained the old relative TTL")
	}
	if extra["openai_excel_bps_config_mode"] != "initial" || extra["openai_excel_bps_auto_disable_on_403"] != true || extra["openai_excel_bps_omit_unsupported_tools"] != true || extra["openai_excel_bps_ignore_encrypted_content"] != false || extra["openai_excel_bps_403_target_group_id"] != 17 || creds["model_mapping_mode"] != "passthrough" {
		t.Fatal("refresh lost BPS defaults or overwrote explicit configuration")
	}
	if extra["openai_excel_bps"] != true || extra["plan_type"] != "business_premium" || !reflect.DeepEqual(extra["openai_excel_bps_models"], []string{"fixture-model"}) || extra["codex_fingerprint_seed"] != "synthetic-seed" || extra["cost_multiplier"] != 0.1 || extra["privacy_mode"] != "training_off" || account["name"] != "Keep custom name" || account["concurrency"] != 100 || account["group_rate_multiplier"] != 0.5 {
		t.Fatal("refresh changed Sub2 configuration or BPS routing")
	}
}

func TestRefreshDoesNotCarryOldExpiryOrBPSRoutingIntoChatGPT(t *testing.T) {
	creds := map[string]any{"access_token": "old", "refresh_token": "keep-refresh", "id_token": metadataFixtureToken(`{"exp":2100000000}`), "expires_at": 1900000000, "expires_in": 300}
	extra := map[string]any{"openai_excel_bps": true, "openai_excel_bps_models": []string{"fixture-model"}, "custom": true}
	protocol.ApplyBPSSettings(extra, protocol.DefaultBPSClientID)
	account := map[string]any{"credentials": creds, "extra": extra}
	updateRefreshedDocument(map[string]any{"accounts": []any{account}}, protocol.TokenSet{AccessToken: "opaque-new-access"}, protocol.DefaultCodexClientID)
	for _, field := range []string{"expires_at", "expires_in"} {
		if _, exists := creds[field]; exists {
			t.Errorf("unknown new expiry retained old %s", field)
		}
	}
	if _, exists := extra["openai_excel_bps_models"]; exists || extra["openai_excel_bps"] != false || extra["custom"] != true || creds["refresh_token"] != "keep-refresh" {
		t.Fatal("ChatGPT retained BPS routing or lost saved data")
	}
	if _, exists := extra["openai_excel_bps_auto_move_on_403"]; exists {
		t.Fatal("ChatGPT retained BPS recovery behavior")
	}
}

func TestRefreshClearsAccountMetadataOnlyWhenWorkspaceOrPlanChanges(t *testing.T) {
	for _, fixture := range []struct {
		name, claims string
		clearOrg     bool
	}{
		{"workspace", `{"auth":{"chatgpt_account_id":"new-workspace"}}`, true},
		{"plan", `{"auth":{"chatgpt_account_id":"old-workspace","chatgpt_plan_type":"pro"}}`, false},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			creds := map[string]any{"email": "owner@example.test", "chatgpt_account_id": "old-workspace", "plan_type": "plus", "organization_id": "old-org", "subscription_expires_at": "2030-01-01T00:00:00Z", "refresh_token": "old-refresh"}
			extra := map[string]any{"plan_type": "plus", "organization_id": "old-org", "subscription_expires_at": "2030-01-01T00:00:00Z"}
			account := map[string]any{"planType": "plus", "credentials": creds, "extra": extra}
			updateRefreshedDocument(map[string]any{"accounts": []any{account}}, protocol.TokenSet{AccessToken: metadataFixtureToken(fixture.claims), RefreshToken: "new-refresh"}, protocol.DefaultBPSClientID)
			for _, fields := range []map[string]any{creds, extra} {
				if _, exists := fields["subscription_expires_at"]; exists {
					t.Fatal("old subscription expiry followed a different workspace/plan")
				}
				if _, exists := fields["organization_id"]; exists == fixture.clearOrg {
					t.Fatal("organization was cleared or retained for the wrong condition")
				}
			}
			if fixture.clearOrg {
				for _, fields := range []map[string]any{account, creds, extra} {
					for _, key := range []string{"plan_type", "planType"} {
						if _, exists := fields[key]; exists {
							t.Fatal("unknown new workspace inherited the old plan")
						}
					}
				}
			}
		})
	}
}
