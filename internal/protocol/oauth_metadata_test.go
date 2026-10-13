package protocol

import (
	"encoding/json"
	"testing"
)

func TestOAuthExportIncludesProviderCredentialMetadata(t *testing.T) {
	tokens := TokenSet{
		AccessToken:  base64JWT(`{"exp":1900000000,"auth":{"chatgpt_account_id":"current-account","chatgpt_user_id":"chatgpt-specific-user","user_id":"generic-access-user","chatgpt_plan_type":"self_serve_business_prolite"},"https://api.openai.com/profile":{"email":"current@example.test"}}`),
		IDToken:      base64JWT(`{"exp":1800000000,"email":"current@example.test","https://api.openai.com/auth":{"chatgpt_account_id":"current-account","user_id":"generic-id-user","chatgpt_plan_type":"plus","organizations":[{"id":"other-org"},{"id":"default-org","is_default":true}]}}`),
		RefreshToken: "synthetic-refresh",
	}
	export, err := BuildSub2APIOAuthExport(tokens, DefaultBPSClientID, "current@example.test", "", 1, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	a := export.Accounts[0]
	c := a.Credentials
	if c.ExpiresAt != 1900000000 || c.ChatGPTUserID != "chatgpt-specific-user" || c.OrganizationID != "default-org" || c.PlanType != "self_serve_business_prolite" {
		t.Fatal("export did not retain the current access-token metadata and default organization")
	}
	if a.Extra["openai_excel_bps"] != true || a.Extra["plan_type"] != "business_premium" || a.Extra["chatgpt_user_id"] != "chatgpt-specific-user" {
		t.Fatal("export did not include BPS routing and normalized display metadata")
	}
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(b, &fields); err != nil {
		t.Fatal(err)
	}
	if _, exists := fields["subscription_expires_at"]; exists {
		t.Fatal("export fabricated a subscription expiration from unrelated token expiry")
	}
	chatgpt, err := BuildSub2APIOAuthExport(tokens, DefaultCodexClientID, "current@example.test", "", 1, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := chatgpt.Accounts[0].Extra["openai_excel_bps"]; exists {
		t.Fatal("ChatGPT export was marked as BPS")
	}
}

func TestOAuthMetadataUsesCurrentAccessClaimsAndDedicatedUserID(t *testing.T) {
	for _, tc := range []struct {
		name, access, id, user string
	}{
		{"access dedicated", `{"auth":{"chatgpt_user_id":"current","user_id":"generic"}}`, `{"auth":{"chatgpt_user_id":"old"}}`, "current"},
		{"id dedicated before generic access", `{"auth":{"user_id":"generic"}}`, `{"auth":{"chatgpt_user_id":"dedicated"}}`, "dedicated"},
		{"short claim alongside namespace", `{"https://api.openai.com/auth":{"user_id":"generic"},"auth":{"chatgpt_user_id":"dedicated"}}`, `{}`, "dedicated"},
		{"generic before sub", `{"sub":"subject","auth":{"user_id":"generic"}}`, `{}`, "generic"},
		{"sub fallback", `{"sub":"subject"}`, `{}`, "subject"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			metadata := ExtractOAuthTokenMetadata(TokenSet{AccessToken: base64JWT(tc.access), IDToken: base64JWT(tc.id)})
			if metadata.ChatGPTUserID != tc.user {
				t.Fatal("unexpected ChatGPT user identity")
			}
		})
	}
	metadata := ExtractOAuthTokenMetadata(TokenSet{
		AccessToken: base64JWT(`{"auth":{"chatgpt_account_id":"new-account","chatgpt_plan_type":"self_serve_business_prolite","subscription_expires_at":"2030-01-01T00:00:00Z"},"email":"new@example.test"}`),
		IDToken:     base64JWT(`{"auth":{"chatgpt_account_id":"old-account","chatgpt_plan_type":"plus","subscription_expires_at":"2029-01-01T00:00:00Z"},"email":"old@example.test"}`),
	})
	if metadata.ChatGPTAccountID != "new-account" || metadata.Email != "new@example.test" || metadata.PlanType != "self_serve_business_prolite" || metadata.SubscriptionExpiresAt != "2030-01-01T00:00:00Z" {
		t.Fatal("older ID-token claims overrode current access-token metadata")
	}
}

func TestOAuthMetadataExpiryNeverUsesIDTokenOrRelativeTTL(t *testing.T) {
	for _, tc := range []struct {
		name, access string
		want         int64
	}{
		{"access numeric", `{"exp":1900000000}`, 1900000000},
		{"access numeric string", `{"exp":"1900000000"}`, 1900000000},
		{"opaque access", "opaque", 0},
		{"missing expiry", `{}`, 0},
		{"negative expiry", `{"exp":-1}`, 0},
		{"fractional expiry", `{"exp":1900000000.5}`, 0},
		{"overflow", `{"exp":9223372036854775808}`, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			access := tc.access
			if access != "opaque" {
				access = base64JWT(access)
			}
			tokens := TokenSet{AccessToken: access, IDToken: base64JWT(`{"exp":1800000000}`), ExpiresIn: 3600}
			metadata := ExtractOAuthTokenMetadata(tokens)
			if metadata.ExpiresAt != tc.want {
				t.Fatalf("expiry=%d, want %d", metadata.ExpiresAt, tc.want)
			}
		})
	}
}

func TestOAuthMetadataDoesNotInheritAnotherWorkspaceSubscription(t *testing.T) {
	tokens := TokenSet{
		AccessToken:  base64JWT(`{"auth":{"chatgpt_account_id":"new-workspace"},"email":"owner@example.test"}`),
		IDToken:      base64JWT(`{"auth":{"chatgpt_account_id":"old-workspace","chatgpt_plan_type":"self_serve_business_prolite","subscription_expires_at":"2030-01-01T00:00:00Z","organizations":[{"id":"old-org"}]}}`),
		RefreshToken: "synthetic-refresh",
	}
	metadata := ExtractOAuthTokenMetadata(tokens)
	if metadata.PlanType != "" || metadata.SubscriptionExpiresAt != "" || metadata.OrganizationID != "" {
		t.Fatal("metadata inherited account-scoped claims from another workspace")
	}
	export, err := BuildSub2APIOAuthExport(tokens, DefaultBPSClientID, "owner@example.test", "", 1, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	c := export.Accounts[0].Credentials
	if c.ChatGPTAccountID != "new-workspace" || c.PlanType != "" || c.SubscriptionExpiresAt != "" || c.OrganizationID != "" {
		t.Fatal("export inherited metadata from another workspace")
	}
	if _, exists := export.Accounts[0].Extra["plan_type"]; exists {
		t.Fatal("export inherited a display tier from another workspace")
	}
}

func TestOAuthMetadataOrganizationSelection(t *testing.T) {
	for _, tc := range []struct {
		name, organizations, want string
	}{
		{"default", `[{"id":"one"},{"id":"two","is_default":true}]`, "two"},
		{"unique", `[{"id":"one"}]`, "one"},
		{"duplicate same organization", `[{"id":"one"},{"id":"one"}]`, "one"},
		{"ambiguous", `[{"id":"one"},{"id":"two"}]`, ""},
		{"multiple defaults", `[{"id":"one","is_default":true},{"id":"two","is_default":true}]`, ""},
		{"empty", `[]`, ""},
		{"malformed", `"unexpected"`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tokens := TokenSet{
				AccessToken: base64JWT(`{"auth":{"organizations":` + tc.organizations + `}}`),
				IDToken:     base64JWT(`{"auth":{"organizations":[{"id":"old-id-token-org","is_default":true}]}}`),
			}
			if got := ExtractOAuthTokenMetadata(tokens).OrganizationID; got != tc.want {
				t.Fatalf("organization=%q, want %q", got, tc.want)
			}
		})
	}
	metadata := ExtractOAuthTokenMetadata(TokenSet{AccessToken: base64JWT(`{"auth":{"organization_id":"explicit"}}`)})
	if metadata.OrganizationID != "explicit" {
		t.Fatal("explicit organization identity was lost")
	}
	metadata = ExtractOAuthTokenMetadata(TokenSet{
		AccessToken: base64JWT(`{"auth":{"chatgpt_account_id":"new-account"}}`),
		IDToken:     base64JWT(`{"auth":{"chatgpt_account_id":"old-account","organizations":[{"id":"old-org"}]}}`),
	})
	if metadata.OrganizationID != "" {
		t.Fatal("an ID token for a different account supplied the new grant's organization")
	}
}
