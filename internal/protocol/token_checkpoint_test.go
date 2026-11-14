package protocol

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testIssuedGrant() SavedOAuthGrant {
	claims, _ := json.Marshal(map[string]any{"email": "issued@example.test", "sid": "account-fixture"})
	return SavedOAuthGrant{Version: 1, Email: "issued@example.test", AuthorizationMethod: "chatgpt", ClientID: DefaultCodexClientID, RedirectURI: DefaultCodexRedirectURI, ChatGPTBase: "https://chatgpt.com", AuthBase: "https://auth.openai.com", Tokens: TokenSet{AccessToken: "issued-access", RefreshToken: "issued-refresh", IDToken: "header." + base64.RawURLEncoding.EncodeToString(claims) + ".signature"}}
}
func TestIssuedGrantFinalizationIsLocalAndContainsNoLoginProgress(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "oauth-grant.json")
	grant := testIssuedGrant()
	if err := WriteSavedOAuthGrant(path, grant); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	for _, key := range []string{"cookies", "callback", "codeVerifier", "state", "web", "session"} {
		if strings.Contains(string(data), `"`+key+`"`) {
			t.Fatal("persisted authentication progress", key)
		}
	}
	saved, err := ReadSavedOAuthGrant(path)
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "oauth.json")
	result, err := FinalizeSavedOAuthGrant(context.Background(), saved, FlowOptions{Email: grant.Email, GrantPath: path, OutputPath: out})
	if err != nil || len(result.Export.Accounts) != 1 {
		t.Fatal("local finalization failed", err)
	}
	if _, err = os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("issued receipt not retired", err)
	}
	if result.Export.Accounts[0].Credentials.RefreshToken != grant.Tokens.RefreshToken {
		t.Fatal("issued grant changed")
	}
}
func TestIssuedGrantRejectsOldProgressAndAccountMismatch(t *testing.T) {
	grant := testIssuedGrant()
	if _, err := FinalizeSavedOAuthGrant(t.Context(), Checkpoint{Stage: "callback_ready"}, FlowOptions{Email: grant.Email}); err == nil {
		t.Fatal("old login progress accepted")
	}
	if _, err := FinalizeSavedOAuthGrant(t.Context(), grant, FlowOptions{Email: "another@example.test"}); err == nil {
		t.Fatal("account mismatch accepted")
	}
	if _, err := FinalizeSavedOAuthGrant(t.Context(), grant, FlowOptions{Email: grant.Email, AuthorizationMethod: "bps"}); err == nil {
		t.Fatal("client mismatch accepted")
	}
	path := filepath.Join(t.TempDir(), "grant.json")
	data, _ := json.Marshal(grant)
	var raw map[string]any
	json.Unmarshal(data, &raw)
	raw["cookies"] = []any{}
	if err := WritePrivateJSON(path, raw); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadSavedOAuthGrant(path); err == nil {
		t.Fatal("session-bearing grant accepted")
	}
}
