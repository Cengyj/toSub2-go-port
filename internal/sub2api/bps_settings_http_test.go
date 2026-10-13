package sub2api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"tosub2-go-port/internal/protocol"
)

// These expectations describe the user's import template independently of the
// production default helper. Model names are configured defaults, not claims.
func bpsDefaultConfigurationFixture() map[string]any {
	return map[string]any{
		"openai_excel_bps":                               true,
		"openai_excel_bps_config_mode":                   "initial",
		"openai_excel_bps_ignore_encrypted_content":      true,
		"openai_excel_bps_omit_unsupported_tools":        true,
		"openai_excel_bps_auto_disable_on_403":           true,
		"openai_excel_bps_auto_move_on_403":              true,
		"openai_excel_bps_403_target_group_id":           0,
		"openai_excel_bps_403_recovery_interval_minutes": 60,
		"openai_excel_bps_models":                        []string{"gpt-6-astra", "gpt-5.6-sol", "gpt-5.6-terra"},
	}
}

func assertBPSDefaultConfiguration(t *testing.T, extra map[string]any) {
	t.Helper()
	assertBPSConfigurationFields(t, extra, bpsDefaultConfigurationFixture())
}

func assertBPSConfigurationFields(t *testing.T, extra, want map[string]any) {
	t.Helper()
	for field, expected := range want {
		got, present := extra[field]
		actualJSON, err := json.Marshal(got)
		expectedJSON, expectedErr := json.Marshal(expected)
		if !present || err != nil || expectedErr != nil || string(actualJSON) != string(expectedJSON) {
			t.Errorf("BPS configuration field %s was missing or changed", field)
		}
	}
}

func TestUploadAndMonitorPreserveBPSConfigurationAcrossAuthorizationUpdates(t *testing.T) {
	custom := map[string]any{
		"openai_excel_bps":                               true,
		"openai_excel_bps_config_mode":                   "custom",
		"openai_excel_bps_ignore_encrypted_content":      false,
		"openai_excel_bps_omit_unsupported_tools":        false,
		"openai_excel_bps_auto_disable_on_403":           false,
		"openai_excel_bps_auto_move_on_403":              false,
		"openai_excel_bps_403_target_group_id":           17,
		"openai_excel_bps_403_recovery_interval_minutes": 0,
		"openai_excel_bps_models":                        []any{"remote-model"},
	}
	emptyModels := cloneMap(custom)
	emptyModels["openai_excel_bps_models"] = []any{}
	emptyModels["openai_excel_bps_403_target_group_id"] = 0
	legacyDefaults := bpsDefaultConfigurationFixture()
	legacyDefaults["openai_excel_bps_config_mode"] = "defaults"
	delete(legacyDefaults, "openai_excel_bps_auto_disable_on_403")
	legacyOverrides := cloneMap(custom)
	legacyOverrides["openai_excel_bps_config_mode"] = "defaults"
	legacyOverrides["openai_excel_bps_403_target_group_id"] = 0
	migratedOverrides := cloneMap(legacyOverrides)
	migratedOverrides["openai_excel_bps_config_mode"] = "initial"
	nilSettings := map[string]any{}
	for field := range bpsDefaultConfigurationFixture() {
		nilSettings[field] = nil
	}
	cleanup := bpsDefaultConfigurationFixture()
	cleanup["openai_excel_bps_future_setting"] = "remote-extension"
	for _, operation := range []string{"upload", "monitor"} {
		for _, fixture := range []struct {
			name, previous, target string
			extra                  map[string]any
			mapping                any
			want                   map[string]any
		}{
			{"missing-settings", protocol.DefaultBPSClientID, protocol.DefaultBPSClientID, nil, nil, bpsDefaultConfigurationFixture()},
			{"enter-bps", protocol.DefaultCodexClientID, protocol.DefaultBPSClientID, nil, nil, bpsDefaultConfigurationFixture()},
			{"nil-settings", protocol.DefaultBPSClientID, protocol.DefaultBPSClientID, nilSettings, nil, bpsDefaultConfigurationFixture()},
			{"migrate-defaults-preset", protocol.DefaultBPSClientID, protocol.DefaultBPSClientID, legacyDefaults, nil, bpsDefaultConfigurationFixture()},
			{"migrate-defaults-preserves-overrides", protocol.DefaultBPSClientID, protocol.DefaultBPSClientID, legacyOverrides, "remote-mode", migratedOverrides},
			{"preserve-custom", protocol.DefaultBPSClientID, protocol.DefaultBPSClientID, custom, "remote-mode", custom},
			{"preserve-empty-models-and-zero-group", protocol.DefaultBPSClientID, protocol.DefaultBPSClientID, emptyModels, "remote-mode", emptyModels},
			{"leave-bps", protocol.DefaultBPSClientID, protocol.DefaultCodexClientID, cleanup, "remote-mode", nil},
		} {
			t.Run(operation+"/"+fixture.name, func(t *testing.T) {
				local := uploadFixtureAccount("owner@example.test", "fixture-new-access")
				incoming := local["credentials"].(map[string]any)
				incoming["client_id"] = fixture.target
				incoming["model_mapping_mode"] = "incoming-mode-must-not-replace-remote"
				local["extra"] = map[string]any{
					"client_id":                            fixture.target,
					"openai_excel_bps_config_mode":         "incoming-mode",
					"openai_excel_bps_models":              []any{"incoming-model-must-not-replace-remote"},
					"openai_excel_bps_auto_disable_on_403": true,
					"openai_excel_bps_auto_move_on_403":    true,
				}
				remoteExtra := cloneMap(fixture.extra)
				remoteExtra["client_id"] = fixture.previous
				remoteExtra["provider_option"] = "keep-extra"
				remoteCredentials := map[string]any{
					"email": "owner@example.test", "client_id": fixture.previous,
					"access_token": "fixture-old-access", "refresh_token": "fixture-old-refresh",
					"provider_option": "keep-credentials", "model_mapping": map[string]any{"remote-model": "remote-target"},
				}
				if fixture.mapping != nil {
					remoteCredentials["model_mapping_mode"] = fixture.mapping
				} else if fixture.name == "nil-settings" {
					remoteCredentials["model_mapping_mode"] = nil
				}
				remote := map[string]any{
					"id": 12, "name": "Keep remote name", "platform": "openai", "type": "oauth",
					"credentials": remoteCredentials, "extra": remoteExtra,
					"group_ids": []any{23}, "concurrency": 100,
				}
				before, _ := json.Marshal(remote)
				updates, lifecycle := 0, 0
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch {
					case r.Method == http.MethodGet && r.URL.Path == "/api/v1/admin/accounts":
						writeUploadFixture(t, w, map[string]any{"items": []any{remote}, "pages": 1, "total": 1})
					case r.Method == http.MethodGet && r.URL.Path == "/api/v1/admin/accounts/12":
						writeUploadFixture(t, w, map[string]any{"data": remote})
					case r.Method == http.MethodPut && r.URL.Path == "/api/v1/admin/accounts/12":
						updates++
						var body map[string]any
						if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
							t.Error(err)
							w.WriteHeader(http.StatusBadRequest)
							return
						}
						creds, credentialsOK := body["credentials"].(map[string]any)
						extra, extraOK := body["extra"].(map[string]any)
						if len(body) != 2 || !credentialsOK || !extraOK {
							t.Error("authorization update changed top-level remote configuration")
							w.WriteHeader(http.StatusBadRequest)
							return
						}
						if creds["client_id"] != fixture.target || extra["client_id"] != fixture.target || creds["access_token"] != "fixture-new-access" {
							t.Error("BPS configuration update lost the new authorization")
						}
						if extra["provider_option"] != "keep-extra" || creds["provider_option"] != "keep-credentials" || !reflect.DeepEqual(creds["model_mapping"], remoteCredentials["model_mapping"]) {
							t.Error("authorization update replaced unrelated remote provider settings")
						}
						wantMapping := fixture.mapping
						if wantMapping == nil && fixture.target == protocol.DefaultBPSClientID {
							wantMapping = "whitelist"
						}
						if creds["model_mapping_mode"] != wantMapping {
							t.Error("model mapping mode was not preserved or defaulted correctly")
						}
						if fixture.target == protocol.DefaultBPSClientID {
							assertBPSConfigurationFields(t, extra, fixture.want)
						} else {
							if extra["openai_excel_bps"] != false {
								t.Error("ChatGPT authorization kept the BPS routing marker")
							}
							for field := range extra {
								if strings.HasPrefix(field, "openai_excel_bps_") {
									t.Errorf("ChatGPT authorization kept BPS field %s", field)
								}
							}
						}
						writeUploadFixture(t, w, map[string]any{})
					case operation == "monitor" && r.Method == http.MethodPost && (r.URL.Path == "/api/v1/admin/accounts/12/clear-error" || r.URL.Path == "/api/v1/admin/accounts/12/schedulable"):
						lifecycle++
						writeUploadFixture(t, w, map[string]any{})
					default:
						t.Errorf("unexpected configuration lifecycle request: %s %s", r.Method, r.URL.Path)
						w.WriteHeader(http.StatusBadRequest)
					}
				}))
				defer server.Close()
				client := New(Config{BaseURL: server.URL, APIKey: "fixture"})
				var err error
				if operation == "upload" {
					_, err = client.Upload(context.Background(), uploadFixturePayload(local))
				} else {
					err = client.SyncAccountCredentials(context.Background(), 12, incoming, "")
				}
				if err != nil || updates != 1 {
					t.Fatalf("existing account updates=%d: %v", updates, err)
				}
				if (operation == "monitor" && lifecycle != 2) || (operation == "upload" && lifecycle != 0) {
					t.Fatal("configuration update changed the remote account lifecycle")
				}
				after, _ := json.Marshal(remote)
				if string(after) != string(before) {
					t.Fatal("configuration merge mutated its remote source")
				}
			})
		}
	}
}
