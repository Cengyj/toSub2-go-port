package cpa

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"tosub2-go-port/internal/protocol"
)

func testAccount() map[string]any {
	return map[string]any{"platform": "openai", "type": "oauth", "name": "private name", "credentials": map[string]any{
		"client_id": protocol.DefaultCodexClientID, "access_token": "access-secret", "refresh_token": "refresh-secret", "id_token": "id-secret", "email": "Person@example.com", "chatgpt_account_id": "workspace-123", "expires_at": json.Number("1893456000"), "plan_type": "plus",
	}, "extra": map[string]any{"client_id": protocol.DefaultCodexClientID, "vendor_setting": "do-not-export"}}
}

func testJWT(claims map[string]any) string {
	data, _ := json.Marshal(claims)
	return "eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString(data) + ".signature"
}

func TestBuildAuthFileCPAContractAndStableIdentity(t *testing.T) {
	account := testAccount()
	before, _ := json.Marshal(account)
	file, err := BuildAuthFile(account)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(file.Data, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["type"] != "codex" || doc["email"] != "person@example.com" || doc["account_id"] != "workspace-123" || doc["expired"] != "2030-01-01T00:00:00Z" || doc["refresh_token"] != "refresh-secret" || doc["plan_type"] != "plus" {
		t.Fatal("converted auth file did not preserve Codex fields")
	}
	for _, name := range []string{"credentials", "extra", "name", "client_id", "last_refresh", "vendor_setting"} {
		if _, exists := doc[name]; exists {
			t.Errorf("unexpected field %s", name)
		}
	}
	if strings.Contains(file.Filename, "person") || strings.Contains(file.Filename, "workspace") || !strings.HasSuffix(file.Filename, ".json") {
		t.Fatal("filename exposes account identity")
	}
	after, _ := json.Marshal(account)
	if string(before) != string(after) {
		t.Fatal("source account was mutated")
	}
	creds := account["credentials"].(map[string]any)
	creds["email"], creds["plan_type"], creds["access_token"] = "PERSON@EXAMPLE.COM", "pro", "next-access"
	updated, err := BuildAuthFile(account)
	if err != nil || updated.Filename != file.Filename {
		t.Fatal("new grant or plan created a duplicate filename")
	}
	creds["chatgpt_account_id"] = "workspace-456"
	other, err := BuildAuthFile(account)
	if err != nil || other.Filename == file.Filename {
		t.Fatal("different workspace would overwrite first workspace")
	}
}

func TestBuildAuthFileRejectsIncompatibleAndConflictingGrants(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(map[string]any, map[string]any, map[string]any)
		want   error
	}{
		{"bps", func(a, c, e map[string]any) {
			c["client_id"], e["client_id"] = protocol.DefaultBPSClientID, protocol.DefaultBPSClientID
		}, ErrUnsupportedAuthorization},
		{"bps-marker", func(a, c, e map[string]any) { e["openai_excel_bps"] = true }, ErrUnsupportedAuthorization},
		{"unknown-client", func(a, c, e map[string]any) { c["client_id"] = "other-client" }, ErrUnsupportedAuthorization},
		{"unknown-identity", func(a, c, e map[string]any) { delete(c, "client_id"); delete(e, "client_id") }, ErrUnsupportedAuthorization},
		{"token-client-conflict", func(a, c, e map[string]any) {
			c["access_token"] = testJWT(map[string]any{"client_id": protocol.DefaultBPSClientID})
		}, ErrUnsupportedAuthorization},
		{"id-audience-conflict", func(a, c, e map[string]any) {
			c["id_token"] = testJWT(map[string]any{"aud": protocol.DefaultBPSClientID})
		}, ErrUnsupportedAuthorization},
		{"wrong-provider", func(a, c, e map[string]any) { a["platform"] = "anthropic" }, ErrInvalidAuthorization},
		{"no-refresh", func(a, c, e map[string]any) { delete(c, "refresh_token") }, ErrInvalidAuthorization},
		{"email-conflict", func(a, c, e map[string]any) { e["email"] = "other@example.com" }, ErrInvalidAuthorization},
		{"account-conflict", func(a, c, e map[string]any) { e["account_id"] = "other-workspace" }, ErrInvalidAuthorization},
		{"token-identity-conflict", func(a, c, e map[string]any) {
			c["access_token"] = testJWT(map[string]any{"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "other-workspace"}})
		}, ErrInvalidAuthorization},
		{"session-material", func(a, c, e map[string]any) { e["cookie"] = "private-cookie" }, ErrInvalidAuthorization},
		{"invalid-expiry", func(a, c, e map[string]any) { c["expires_at"] = 1.5 }, ErrInvalidAuthorization},
	} {
		t.Run(test.name, func(t *testing.T) {
			account := testAccount()
			test.change(account, account["credentials"].(map[string]any), account["extra"].(map[string]any))
			file, err := BuildAuthFile(account)
			if !errors.Is(err, test.want) || len(file.Data) != 0 {
				t.Fatalf("conversion = file %d bytes, error %v", len(file.Data), err)
			}
			if IsMutationOutcomeUnknown(err) || strings.Contains(SafeError(err), "secret") {
				t.Fatal("local rejection was uncertain or disclosed a credential")
			}
		})
	}
}

func TestBuildAuthFileLegacyClaimsAndAuthoritativeExpiry(t *testing.T) {
	account := testAccount()
	creds := account["credentials"].(map[string]any)
	delete(creds, "client_id")
	delete(account["extra"].(map[string]any), "client_id")
	creds["access_token"] = testJWT(map[string]any{"client_id": protocol.DefaultCodexClientID, "exp": 1924992000, "https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "workspace-123", "chatgpt_plan_type": "pro"}})
	creds["last_refresh"] = "2026-11-13T12:00:00+08:00"
	file, err := BuildAuthFile(account)
	if err != nil {
		t.Fatal(err)
	}
	var doc codexAuth
	_ = json.Unmarshal(file.Data, &doc)
	if doc.Expired != time.Unix(1924992000, 0).UTC().Format(time.RFC3339) || doc.PlanType != "pro" || doc.LastRefresh != "2026-11-13T04:00:00Z" {
		t.Fatal("current token metadata or actual refresh time was lost")
	}
}

func TestBuildAuthFileMultipleAudiencesRequireCodexAuthorizedParty(t *testing.T) {
	for _, scenario := range []string{"valid", "wrong-party", "no-party", "missing-client"} {
		t.Run(scenario, func(t *testing.T) {
			account := testAccount()
			creds := account["credentials"].(map[string]any)
			claims := map[string]any{"aud": []any{protocol.DefaultCodexClientID, "https://resource.example.test"}, "azp": protocol.DefaultCodexClientID}
			switch scenario {
			case "wrong-party":
				claims["azp"] = protocol.DefaultBPSClientID
			case "no-party":
				delete(claims, "azp")
			case "missing-client":
				claims["aud"] = []any{"https://resource.example.test", protocol.DefaultBPSClientID}
			}
			creds["id_token"] = testJWT(claims)
			_, err := BuildAuthFile(account)
			if scenario == "valid" && err != nil {
				t.Fatal(err)
			}
			if scenario != "valid" && !errors.Is(err, ErrUnsupportedAuthorization) {
				t.Fatal("ambiguous client accepted", err)
			}
		})
	}
}
