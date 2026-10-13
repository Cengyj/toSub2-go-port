package sub2api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"tosub2-go-port/internal/protocol"
)

func oauthMetadataFixtureJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	return "synthetic." + base64.RawURLEncoding.EncodeToString(raw) + ".fixture"
}

func TestBuildUploadPayloadRoutesKnownClientsAndAppliesBPSDefaults(t *testing.T) {
	for _, tc := range []struct {
		name, client string
		marker       bool
		keepModels   bool
	}{
		{"bps", protocol.DefaultBPSClientID, true, true},
		{"chatgpt", protocol.DefaultCodexClientID, false, false},
		{"custom-client", "fixture-other-client", false, false},
		{"unspecified", "", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := uploadFixtureAccount("owner@example.test", "fixture-access")
			sourceExtra := source["extra"].(map[string]any)
			sourceExtra["openai_excel_bps"] = true
			sourceExtra["openai_excel_bps_models"] = []string{"configured-model"}
			if tc.client != "" {
				sourceExtra["client_id"] = tc.client // Older exports stored this only in extra.
			}
			before, _ := json.Marshal(source)
			payload, err := BuildUploadPayload([]map[string]any{source}, UploadOptions{})
			if err != nil {
				t.Fatal(err)
			}
			built := payload["accounts"].([]any)[0].(map[string]any)
			for name, account := range map[string]map[string]any{"build": built, "decode": nil} {
				if name == "decode" {
					decoded, _, decodeErr := decodeOAuthUpload(uploadFixturePayload(source))
					if decodeErr != nil {
						t.Fatal(decodeErr)
					}
					account = decoded[0].account
				}
				extra := account["extra"].(map[string]any)
				if extra["openai_excel_bps"] != tc.marker {
					t.Errorf("%s routed the selected OAuth client incorrectly", name)
				}
				_, hasModels := extra["openai_excel_bps_models"]
				if hasModels != tc.keepModels {
					t.Errorf("%s retained a model catalog for the wrong client", name)
				}
			}
			after, _ := json.Marshal(source)
			if string(before) != string(after) {
				t.Fatal("payload normalization mutated the source export")
			}
		})
	}
	plain := uploadFixtureAccount("owner@example.test", "fixture-access")
	plain["credentials"].(map[string]any)["client_id"] = protocol.DefaultBPSClientID
	payload, err := BuildUploadPayload([]map[string]any{plain}, UploadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	built := payload["accounts"].([]any)[0].(map[string]any)
	assertBPSDefaultConfiguration(t, built["extra"].(map[string]any))
	if built["credentials"].(map[string]any)["model_mapping_mode"] != "whitelist" {
		t.Fatal("BPS upload omitted the configured model mapping default")
	}
}

func TestUploadOlderBPSExportDerivesMissingMetadataBeforeCreation(t *testing.T) {
	access := oauthMetadataFixtureJWT(t, map[string]any{
		"exp":                            int64(2000000000),
		"https://api.openai.com/auth":    map[string]any{"chatgpt_account_id": "fixture-workspace", "chatgpt_user_id": "fixture-user", "chatgpt_plan_type": "self_serve_business_prolite"},
		"https://api.openai.com/profile": map[string]any{"email": "owner@example.test"},
	})
	id := oauthMetadataFixtureJWT(t, map[string]any{
		"exp": int64(1999999900), "email": "owner@example.test",
		"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "fixture-workspace", "organizations": []any{map[string]any{"id": "fixture-org", "is_default": true}}},
	})
	source := map[string]any{
		"name": "fixture-account", "platform": "openai", "type": "oauth",
		"credentials": map[string]any{"access_token": access, "refresh_token": "fixture-refresh", "id_token": id},
		"extra":       map[string]any{"client_id": protocol.DefaultBPSClientID},
	}
	before, _ := json.Marshal(source)
	creations := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/admin/accounts":
			writeUploadFixture(t, w, map[string]any{"items": []any{}, "pages": 0, "total": 0})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/admin/accounts/batch":
			creations++
			var body struct{ Accounts []map[string]any }
			if json.NewDecoder(r.Body).Decode(&body) != nil || len(body.Accounts) != 1 {
				t.Error("invalid creation payload")
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			account := body.Accounts[0]
			credentials := account["credentials"].(map[string]any)
			for key, want := range map[string]any{"client_id": protocol.DefaultBPSClientID, "chatgpt_account_id": "fixture-workspace", "chatgpt_user_id": "fixture-user", "organization_id": "fixture-org", "plan_type": "self_serve_business_prolite", "email": "owner@example.test", "expires_at": float64(2000000000)} {
				if credentials[key] != want {
					t.Errorf("legacy export did not derive %s from its grant", key)
				}
			}
			if _, exists := credentials["subscription_expires_at"]; exists {
				t.Error("token expiry was mistaken for subscription expiry")
			}
			extra := account["extra"].(map[string]any)
			if extra["openai_excel_bps"] != true {
				t.Error("BPS routing flag was not sent on creation")
			}
			assertBPSDefaultConfiguration(t, extra)
			if credentials["model_mapping_mode"] != "whitelist" {
				t.Error("legacy BPS creation omitted the configured model mapping default")
			}
			writeUploadFixture(t, w, map[string]any{"created": 1, "failed": 0, "account_ids": []any{12}})
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer server.Close()
	_, err := New(Config{BaseURL: server.URL, APIKey: "fixture"}).Upload(context.Background(), uploadFixturePayload(source))
	if err != nil || creations != 1 {
		t.Fatalf("legacy export creation count=%d: %v", creations, err)
	}
	after, _ := json.Marshal(source)
	if string(before) != string(after) {
		t.Fatal("legacy upload modified the source export")
	}
}

func TestOAuthMetadataPreservesEnrichmentAndExpiresItOnIdentityOrPlanChange(t *testing.T) {
	remote := map[string]any{
		"client_id": protocol.DefaultBPSClientID, "chatgpt_account_id": "fixture-workspace",
		"organization_id": "fixture-org", "plan_type": "pro", "subscription_expires_at": "2030-01-01T00:00:00Z",
	}
	extra := cloneMap(remote)
	extra["openai_excel_bps"], extra["openai_excel_bps_models"] = true, []string{"remote-model"}
	for _, tc := range []struct {
		name                      string
		incoming                  map[string]any
		wantPlan, wantOrg, expiry string
	}{
		{"same-workspace", map[string]any{"client_id": protocol.DefaultCodexClientID, "chatgpt_account_id": "fixture-workspace"}, "pro", "fixture-org", "2030-01-01T00:00:00Z"},
		{"different-workspace", map[string]any{"client_id": protocol.DefaultBPSClientID, "chatgpt_account_id": "fixture-other-workspace"}, "", "", ""},
		{"different-workspace-with-new-claims", map[string]any{"client_id": protocol.DefaultBPSClientID, "chatgpt_account_id": "fixture-other-workspace", "plan_type": "free", "organization_id": "fixture-other-org"}, "free", "fixture-other-org", ""},
		{"different-plan", map[string]any{"client_id": protocol.DefaultBPSClientID, "chatgpt_account_id": "fixture-workspace", "plan_type": "plus"}, "plus", "fixture-org", ""},
		{"different-plan-with-renewal", map[string]any{"client_id": protocol.DefaultBPSClientID, "chatgpt_account_id": "fixture-workspace", "plan_type": "plus", "subscription_expires_at": "2031-01-01T00:00:00Z"}, "plus", "fixture-org", "2031-01-01T00:00:00Z"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for location, merged := range map[string]map[string]any{"credentials": mergeOAuthCredentials(remote, extra, tc.incoming), "extra": mergeOAuthExtra(remote, extra, tc.incoming)} {
				for field, want := range map[string]string{"plan_type": tc.wantPlan, "organization_id": tc.wantOrg, "subscription_expires_at": tc.expiry} {
					if stringValue(merged[field]) != want {
						t.Errorf("%s retained stale or lost valid %s", location, field)
					}
				}
			}
		})
	}
	unknown := mergeOAuthExtra(remote, extra, map[string]any{"access_token": "fixture-access"})
	if !reflect.DeepEqual(unknown, extra) {
		t.Fatal("grant without a client changed BPS routing or remote enrichment")
	}
	claims := oauthMetadataFixtureJWT(t, map[string]any{"exp": int64(2000000000), "https://api.openai.com/auth": map[string]any{"chatgpt_plan_type": "free", "organization_id": "fixture-other-org"}})
	enriched := cloneMap(remote)
	enriched["access_token"], enriched["expires_at"] = claims, int64(2000001234)
	if err := normalizeOAuthAuthorization(enriched, map[string]any{}); err != nil {
		t.Fatal(err)
	}
	for field, want := range map[string]any{"plan_type": "pro", "organization_id": "fixture-org", "subscription_expires_at": "2030-01-01T00:00:00Z", "expires_at": int64(2000001234)} {
		if enriched[field] != want {
			t.Errorf("normalization overwrote existing Sub2 enrichment %s", field)
		}
	}
}
