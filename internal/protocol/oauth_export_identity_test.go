package protocol

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"
)

func identityFixtureToken(t *testing.T, claims map[string]any) string {
	t.Helper()
	b, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	return "fixture." + base64.RawURLEncoding.EncodeToString(b) + ".signature"
}

func TestOAuthExportIdentityRetainedIDWorkspaceAndOpaqueCompatibility(t *testing.T) {
	current := map[string]any{"email": "owner@example.test", "client_id": DefaultCodexClientID, "chatgpt_account_id": "current"}
	current["access_token"] = identityFixtureToken(t, map[string]any{"client_id": DefaultCodexClientID, "aud": "https://api.openai.com/v1", "email": "OWNER@example.test", "auth": map[string]any{"chatgpt_account_id": "current"}})
	current["id_token"] = identityFixtureToken(t, map[string]any{"aud": []any{DefaultCodexClientID}, "email": "owner@example.test", "auth": map[string]any{"chatgpt_account_id": "previous"}})
	if err := ValidateOAuthExportIdentity(current, nil, "owner@example.test"); err != nil {
		t.Fatal("retained ID token changed current access identity", err)
	}
	current["id_token"] = identityFixtureToken(t, map[string]any{"aud": []any{DefaultCodexClientID, "https://resource.example.test"}, "azp": DefaultCodexClientID})
	if err := ValidateOAuthExportIdentity(current, nil, "owner@example.test"); err != nil {
		t.Fatal("valid authorized party with multiple recipients rejected", err)
	}
	if err := ValidateOAuthExportIdentity(map[string]any{"access_token": "opaque-fixture"}, nil, "owner@example.test"); err != nil {
		t.Fatal("opaque legacy grant rejected", err)
	}
}

func TestOAuthExportIdentityRejectsExplicitContradictions(t *testing.T) {
	for _, scenario := range []string{"email", "nested-email", "id-email", "client", "id-client", "id-missing-client", "id-missing-party", "account", "account-alias", "malformed", "duplicate", "typed-email"} {
		t.Run(scenario, func(t *testing.T) {
			claims := map[string]any{"client_id": DefaultCodexClientID, "email": "owner@example.test", "auth": map[string]any{"chatgpt_account_id": "current"}}
			c := map[string]any{"email": "owner@example.test", "client_id": DefaultCodexClientID, "chatgpt_account_id": "current"}
			switch scenario {
			case "email":
				claims["email"] = "other@example.test"
			case "nested-email":
				claims["https://api.openai.com/profile"] = map[string]any{"email": "other@example.test"}
			case "id-email":
				c["id_token"] = identityFixtureToken(t, map[string]any{"email": "other@example.test"})
			case "client":
				claims["azp"] = DefaultBPSClientID
			case "id-client":
				c["id_token"] = identityFixtureToken(t, map[string]any{"aud": DefaultBPSClientID})
			case "id-missing-client":
				c["id_token"] = identityFixtureToken(t, map[string]any{"aud": []any{DefaultBPSClientID, "resource"}, "azp": DefaultCodexClientID})
			case "id-missing-party":
				c["id_token"] = identityFixtureToken(t, map[string]any{"aud": []any{DefaultCodexClientID, "resource"}})
			case "account":
				c["chatgpt_account_id"] = "other-workspace"
			case "account-alias":
				claims["auth"] = map[string]any{"account_id": "other-workspace"}
			case "typed-email":
				c["email"] = 42
			}
			c["access_token"] = identityFixtureToken(t, claims)
			if scenario == "malformed" {
				c["access_token"] = "fixture.!.signature"
			}
			if scenario == "duplicate" {
				c["access_token"] = "fixture." + base64.RawURLEncoding.EncodeToString([]byte(`{"email":"other@example.test","email":"owner@example.test"}`)) + ".signature"
			}
			if err := ValidateOAuthExportIdentity(c, nil, "owner@example.test"); !errors.Is(err, ErrOAuthExportIdentity) {
				t.Fatal("contradiction accepted", err)
			}
		})
	}
}
