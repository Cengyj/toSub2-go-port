package sub2api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"tosub2-go-port/internal/protocol"
)

func TestProviderChangeUpdatesExistingAccountWithoutOldGrantOrConfigLoss(t *testing.T) {
	for _, method := range []string{"upload", "monitor"} {
		for _, tc := range []struct{ name, previous, target string }{
			{"chatgpt-to-bps", protocol.DefaultCodexClientID, protocol.DefaultBPSClientID},
			{"bps-to-chatgpt", protocol.DefaultBPSClientID, protocol.DefaultCodexClientID},
			{"bps-to-bps", protocol.DefaultBPSClientID, protocol.DefaultBPSClientID},
		} {
			t.Run(method+"-"+tc.name, func(t *testing.T) {
				previous, target := tc.previous, tc.target
				local := uploadFixtureAccount("owner@example.test", "fixture-new-access")
				incoming := local["credentials"].(map[string]any)
				incoming["client_id"], incoming["id_token"] = target, ""
				incoming["plan_type"], incoming["organization_id"] = "pro", "fixture-new-org"
				local["extra"].(map[string]any)["client_id"] = target
				local["extra"].(map[string]any)["openai_excel_bps_models"] = []string{"must-not-replace-remote-model"}
				local["extra"].(map[string]any)["privacy_mode"] = "training_set_failed"
				remote := map[string]any{
					"id": 12, "name": "custom name", "platform": "openai", "type": "oauth",
					"credentials": map[string]any{"email": "owner@example.test", "client_id": previous, "access_token": "fixture-old", "refresh_token": "fixture-old-refresh", "id_token": "fixture-old-id", "expires_at": 1234, "scope": "old-scope", "provider_setting": true, "chatgpt_account_id": "fixture-new-workspace", "organization_id": "fixture-old-org", "plan_type": "pro", "subscription_expires_at": "2030-01-01T00:00:00Z"},
					"extra":       map[string]any{"client_id": previous, "custom": true, "openai_excel_bps": previous == protocol.DefaultBPSClientID, "privacy_mode": "training_off", "codex_fingerprint_seed": "fixture-seed", "cost_multiplier": 1.7},
				}
				if previous == protocol.DefaultBPSClientID {
					remote["extra"].(map[string]any)["openai_excel_bps_models"] = []any{"remote-model-a", "remote-model-b"}
				}
				updates := 0
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch {
					case r.Method == http.MethodGet && r.URL.Path == "/api/v1/admin/accounts":
						writeUploadFixture(t, w, map[string]any{"data": map[string]any{"items": []any{remote}, "pages": 1, "total": 1}})
					case r.Method == http.MethodGet && r.URL.Path == "/api/v1/admin/accounts/12":
						writeUploadFixture(t, w, map[string]any{"data": remote})
					case r.Method == http.MethodPut && r.URL.Path == "/api/v1/admin/accounts/12":
						updates++
						var body map[string]any
						if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
							t.Error(err)
							return
						}
						creds := body["credentials"].(map[string]any)
						extra := body["extra"].(map[string]any)
						if len(body) != 2 || creds["client_id"] != target || extra["client_id"] != target || creds["provider_setting"] != true || extra["custom"] != true || creds["refresh_token"] != incoming["refresh_token"] {
							t.Error("client switch changed remote settings or lost the selected grant")
						}
						if extra["openai_excel_bps"] != (target == protocol.DefaultBPSClientID) || extra["privacy_mode"] != "training_off" || extra["codex_fingerprint_seed"] != "fixture-seed" || extra["cost_multiplier"] != 1.7 {
							t.Error("authorization routing or remote enrichment was lost")
						}
						if previous == protocol.DefaultBPSClientID && target == protocol.DefaultBPSClientID {
							if !reflect.DeepEqual(extra["openai_excel_bps_models"], remote["extra"].(map[string]any)["openai_excel_bps_models"]) {
								t.Error("same BPS grant replaced remote model catalog")
							}
						} else if target == protocol.DefaultBPSClientID {
							assertBPSDefaultConfiguration(t, extra)
						} else if _, exists := extra["openai_excel_bps_models"]; exists {
							t.Error("ChatGPT client retained a BPS model catalog")
						}
						if creds["subscription_expires_at"] != "2030-01-01T00:00:00Z" || creds["organization_id"] != "fixture-new-org" || creds["plan_type"] != "pro" {
							t.Error("same workspace lost subscription enrichment or new claim metadata")
						}
						for _, field := range []string{"id_token", "expires_at", "scope"} {
							if _, ok := creds[field]; ok {
								t.Errorf("new OAuth client retained old %s", field)
							}
						}
						writeUploadFixture(t, w, map[string]any{})
					case method == "monitor" && r.Method == http.MethodPost && (r.URL.Path == "/api/v1/admin/accounts/12/clear-error" || r.URL.Path == "/api/v1/admin/accounts/12/schedulable"):
						writeUploadFixture(t, w, map[string]any{})
					default:
						t.Errorf("unexpected account lifecycle request: %s %s", r.Method, r.URL.Path)
						w.WriteHeader(http.StatusBadRequest)
					}
				}))
				defer server.Close()
				client := New(Config{BaseURL: server.URL, APIKey: "fixture"})
				var err error
				if method == "upload" {
					_, err = client.Upload(context.Background(), uploadFixturePayload(local))
				} else {
					err = client.SyncAccountCredentials(context.Background(), 12, incoming, "")
				}
				if err != nil || updates != 1 {
					t.Fatalf("existing account updates=%d: %v", updates, err)
				}
			})
		}
	}
}

func TestUploadRequiresConsistentOAuthClient(t *testing.T) {
	local := uploadFixtureAccount("owner@example.test", "fixture-access")
	local["extra"].(map[string]any)["client_id"] = protocol.DefaultBPSClientID
	accounts, _, err := decodeOAuthUpload(uploadFixturePayload(local))
	if err != nil || accounts[0].account["credentials"].(map[string]any)["client_id"] != protocol.DefaultBPSClientID {
		t.Fatal("metadata OAuth client was not included in upload credentials")
	}
	local["credentials"].(map[string]any)["client_id"] = protocol.DefaultCodexClientID
	if _, _, err := decodeOAuthUpload(uploadFixturePayload(local)); err == nil {
		t.Fatal("conflicting OAuth clients accepted before upload")
	}
}
