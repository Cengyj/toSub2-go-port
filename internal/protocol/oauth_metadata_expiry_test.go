package protocol

import (
	"encoding/json"
	"testing"
)

func TestOAuthMetadataSubscriptionExpiryReported(t *testing.T) {
	const current = "2031-01-01T00:00:00Z"
	const previous = `{"auth":{"chatgpt_account_id":"workspace","chatgpt_plan_type":"plus","subscription_expires_at":"2030-01-01T00:00:00Z"}}`
	for _, test := range []struct {
		name, access, id, want string
		reported               bool
	}{
		{"provider ID alias", `{"auth":{"chatgpt_account_id":"workspace","chatgpt_plan_type":"plus"}}`, `{"https://api.openai.com/auth":{"chatgpt_account_id":"workspace","chatgpt_plan_type":"plus","chatgpt_subscription_active_until":"2031-01-01T08:00:00+08:00"}}`, current, true},
		{"provider access alias", `{"auth":{"chatgpt_subscription_active_until":"2031-01-01T00:00:00Z"}}`, previous, current, true},
		{"top level alias", `{"chatgpt_subscription_active_until":"2031-01-01T00:00:00Z"}`, `{}`, current, true},
		{"canonical wins within source", `{"auth":{"subscription_expires_at":"2031-01-01T00:00:00Z","chatgpt_subscription_active_until":"2032-01-01T00:00:00Z"}}`, previous, current, true},
		{"canonical null blocks alias", `{"auth":{"subscription_expires_at":null,"chatgpt_subscription_active_until":"2031-01-01T00:00:00Z"}}`, previous, "", true},
		{"null blocks old ID", `{"auth":{"chatgpt_subscription_active_until":null}}`, previous, "", true},
		{"empty blocks old ID", `{"auth":{"subscription_expires_at":""}}`, previous, "", true},
		{"invalid blocks old ID", `{"auth":{"subscription_expires_at":"not-a-date"}}`, previous, "", true},
		{"numeric is not a provider date", `{"auth":{"chatgpt_subscription_active_until":2000000000}}`, previous, "", true},
		{"relative date rejected", `{"auth":{"chatgpt_subscription_active_until":"tomorrow"}}`, previous, "", true},
		{"date without zone rejected", `{"auth":{"chatgpt_subscription_active_until":"2031-01-01T00:00:00"}}`, previous, "", true},
		{"zero time rejected", `{"auth":{"chatgpt_subscription_active_until":"0001-01-01T00:00:00Z"}}`, previous, "", true},
		{"UTC underflow rejected", `{"auth":{"chatgpt_subscription_active_until":"0001-01-01T00:00:00+01:00"}}`, previous, "", true},
		{"fraction preserved", `{"auth":{"chatgpt_subscription_active_until":" 2031-01-01T08:00:00.123+08:00 "}}`, `{}`, "2031-01-01T00:00:00.123Z", true},
		{"absent remains unreported", `{}`, `{}`, "", false},
		{"other dates are not subscription expiry", `{"exp":2000000000,"auth":{"chatgpt_subscription_active_start":"2031-01-01T00:00:00Z","chatgpt_subscription_last_checked":"2031-02-01T00:00:00Z"}}`, `{}`, "", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			metadata := ExtractOAuthTokenMetadata(TokenSet{AccessToken: base64JWT(test.access), IDToken: base64JWT(test.id)})
			if metadata.SubscriptionExpiresAt != test.want || metadata.SubscriptionExpiryReported != test.reported {
				t.Fatalf("subscription expiry=%q reported=%t, want %q reported=%t", metadata.SubscriptionExpiresAt, metadata.SubscriptionExpiryReported, test.want, test.reported)
			}
		})
	}
}

func TestOAuthMetadataSubscriptionExpiryRejectsStalePlan(t *testing.T) {
	for _, test := range []struct {
		name, accessAccount, idAccount, accessPlan, idPlan string
		want                                               string
	}{
		{"different workspace", "new", "old", "plus", "plus", ""},
		{"same workspace different plan", "workspace", "workspace", "pro", "plus", ""},
		{"unknown explicit plan change", "workspace", "workspace", "future-plan", "plus", ""},
		{"same plan", "workspace", "workspace", "plus", "plus", "2031-01-01T00:00:00Z"},
		{"equivalent plan alias", "workspace", "workspace", "self_serve_business_prolite", "business_premium", "2031-01-01T00:00:00Z"},
	} {
		t.Run(test.name, func(t *testing.T) {
			access, err := json.Marshal(map[string]any{"auth": map[string]any{"chatgpt_account_id": test.accessAccount, "chatgpt_plan_type": test.accessPlan}})
			if err != nil {
				t.Fatal(err)
			}
			id, err := json.Marshal(map[string]any{"auth": map[string]any{"chatgpt_account_id": test.idAccount, "chatgpt_plan_type": test.idPlan, "chatgpt_subscription_active_until": "2031-01-01T00:00:00Z"}})
			if err != nil {
				t.Fatal(err)
			}
			metadata := ExtractOAuthTokenMetadata(TokenSet{AccessToken: base64JWT(string(access)), IDToken: base64JWT(string(id))})
			if metadata.SubscriptionExpiresAt != test.want || metadata.SubscriptionExpiryReported != (test.want != "") {
				t.Fatal("subscription expiry crossed a workspace or plan boundary")
			}
		})
	}
}

func TestOAuthExportNormalizesProviderSubscriptionAliasWithoutRejectingGrant(t *testing.T) {
	for _, value := range []any{"2031-01-01T08:00:00+08:00", nil, "not-a-date"} {
		auth := map[string]any{"chatgpt_account_id": "workspace", "chatgpt_subscription_active_until": value}
		claims, err := json.Marshal(map[string]any{"email": "owner@example.test", "https://api.openai.com/auth": auth})
		if err != nil {
			t.Fatal(err)
		}
		tokens := TokenSet{AccessToken: base64JWT(`{"auth":{"chatgpt_account_id":"workspace"}}`), IDToken: base64JWT(string(claims)), RefreshToken: "synthetic-refresh"}
		for _, client := range []string{DefaultBPSClientID, DefaultCodexClientID} {
			export, err := BuildSub2APIOAuthExport(tokens, client, "owner@example.test", "", 1, 1, 1)
			if err != nil {
				t.Fatal("optional subscription metadata prevented accepted grant export:", err)
			}
			credentials := export.Accounts[0].Credentials
			want := ""
			if value == "2031-01-01T08:00:00+08:00" {
				want = "2031-01-01T00:00:00Z"
			}
			if credentials.SubscriptionExpiresAt != want || credentials.RefreshToken != tokens.RefreshToken {
				t.Fatal("export lost the accepted grant or subscription date normalization")
			}
		}
	}
}
