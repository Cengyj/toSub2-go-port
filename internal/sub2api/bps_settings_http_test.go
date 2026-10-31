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
// production helper. The latest template explicitly selects nine models.
func bpsDefaultConfigurationFixture() map[string]any {
	return map[string]any{
		"openai_excel_bps": true,
		"openai_excel_bps_models": []string{
			"gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna", "gpt-6-astra",
			"gpt-6.1-sol", "gpt-6.1-terra", "gpt-6-sol", "gpt-6-luna", "codex-auto-review",
		},
		"openai_excel_bps_config_mode":                   "initial",
		"openai_excel_bps_ignore_encrypted_content":      true,
		"openai_excel_bps_omit_unsupported_tools":        true,
		"openai_excel_bps_auto_disable_on_403":           true,
		"openai_excel_bps_auto_move_on_403":              true,
		"openai_excel_bps_403_target_group_id":           0,
		"openai_excel_bps_403_recovery_interval_minutes": 60,
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

func TestBPSCreationAppliesRequestedModelsAndPreservesAccountSettings(t *testing.T) {
	for _, fixture := range []struct {
		name    string
		present bool
		models  any
	}{
		{"missing", false, nil},
		{"configured-list", true, []string{"fixture-old-model"}},
		{"empty-list", true, []string{}},
		{"null-list", true, nil},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			source := uploadFixtureAccount("owner@example.test", "fixture-access")
			credentials := source["credentials"].(map[string]any)
			credentials["client_id"] = protocol.DefaultBPSClientID
			credentials["model_mapping_mode"] = "whitelist"
			credentials["model_mapping"] = map[string]any{"fixture-alias": "fixture-target"}
			wantExtra := bpsDefaultConfigurationFixture()
			wantExtra["openai_excel_bps_config_mode"] = "custom"
			wantExtra["openai_excel_bps_auto_disable_on_403"] = false
			wantExtra["openai_excel_bps_auto_move_on_403"] = false
			wantExtra["openai_excel_bps_403_target_group_id"] = 17
			wantExtra["openai_excel_bps_403_recovery_interval_minutes"] = 0
			wantExtra["custom_route"] = "fixture-route"
			wantExtra["cost_multiplier"] = 1.7
			extra := cloneMap(wantExtra)
			extra["client_id"] = protocol.DefaultBPSClientID
			delete(extra, "openai_excel_bps_models")
			if fixture.present {
				extra["openai_excel_bps_models"] = fixture.models
			}
			source["extra"] = extra
			source["group_ids"], source["proxy_id"], source["rate_multiplier"] = []int{23}, 71, 1.7
			source["priority"], source["concurrency"], source["load_factor"] = 6, 100, 17
			before, err := json.Marshal(source)
			if err != nil {
				t.Fatal(err)
			}
			payload, err := BuildUploadPayload([]map[string]any{source}, UploadOptions{})
			if err != nil {
				t.Fatal(err)
			}
			creations := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/api/v1/admin/accounts":
					writeUploadFixture(t, w, map[string]any{"items": []any{}, "pages": 0, "total": 0})
				case r.Method == http.MethodPost && r.URL.Path == "/api/v1/admin/accounts/batch":
					creations++
					var body struct{ Accounts []map[string]any }
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Accounts) != 1 {
						t.Error("invalid BPS creation request")
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					account := body.Accounts[0]
					gotCredentials, credentialsOK := account["credentials"].(map[string]any)
					gotExtra, extraOK := account["extra"].(map[string]any)
					if !credentialsOK || !extraOK {
						t.Error("BPS creation omitted authorization or extra settings")
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					assertBPSConfigurationFields(t, gotExtra, wantExtra)
					if gotCredentials["client_id"] != protocol.DefaultBPSClientID || gotExtra["client_id"] != protocol.DefaultBPSClientID || gotCredentials["model_mapping_mode"] != "whitelist" || !reflect.DeepEqual(gotCredentials["model_mapping"], credentials["model_mapping"]) {
						t.Error("BPS creation changed OAuth routing or credential model mapping")
					}
					for field, want := range map[string]any{
						"group_ids": []any{float64(23)}, "proxy_id": float64(71), "rate_multiplier": 1.7,
						"priority": float64(6), "concurrency": float64(100), "load_factor": float64(17),
					} {
						if !reflect.DeepEqual(account[field], want) {
							t.Errorf("BPS creation changed account setting %s", field)
						}
					}
					if gotExtra["openai_request_timezone"] != "America/Los_Angeles" {
						t.Error("BPS creation changed the default request timezone")
					}
					writeUploadFixture(t, w, map[string]any{"created": 1, "failed": 0, "account_ids": []any{12}})
				default:
					t.Errorf("unexpected BPS creation request: %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusBadRequest)
				}
			}))
			defer server.Close()
			_, err = New(Config{BaseURL: server.URL, APIKey: "fixture"}).Upload(context.Background(), payload)
			if err != nil || creations != 1 {
				t.Fatalf("BPS creation count=%d: %v", creations, err)
			}
			after, err := json.Marshal(source)
			if err != nil || string(after) != string(before) {
				t.Fatal("BPS normalization mutated the source authorization export")
			}
		})
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
	configuredCustom := cloneMap(custom)
	configuredCustom["openai_excel_bps_models"] = bpsDefaultConfigurationFixture()["openai_excel_bps_models"]
	configuredZeroGroup := cloneMap(emptyModels)
	configuredZeroGroup["openai_excel_bps_models"] = bpsDefaultConfigurationFixture()["openai_excel_bps_models"]
	missingModels := cloneMap(custom)
	delete(missingModels, "openai_excel_bps_models")
	nullModels := cloneMap(custom)
	nullModels["openai_excel_bps_models"] = nil
	legacyDefaults := bpsDefaultConfigurationFixture()
	legacyDefaults["openai_excel_bps_config_mode"] = "defaults"
	legacyDefaults["openai_excel_bps_models"] = []any{"legacy-default-model"}
	delete(legacyDefaults, "openai_excel_bps_auto_disable_on_403")
	legacyOverrides := cloneMap(custom)
	legacyOverrides["openai_excel_bps_config_mode"] = "defaults"
	legacyOverrides["openai_excel_bps_403_target_group_id"] = 0
	migratedOverrides := cloneMap(legacyOverrides)
	migratedOverrides["openai_excel_bps_config_mode"] = "initial"
	migratedOverrides["openai_excel_bps_models"] = bpsDefaultConfigurationFixture()["openai_excel_bps_models"]
	nilSettings := map[string]any{}
	for field := range bpsDefaultConfigurationFixture() {
		nilSettings[field] = nil
	}
	nilSettings["openai_excel_bps_models"] = nil
	cleanup := bpsDefaultConfigurationFixture()
	cleanup["openai_excel_bps_models"] = []any{"legacy-default-model"}
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
			{"enter-bps-replaces-stale-model-list", protocol.DefaultCodexClientID, protocol.DefaultBPSClientID, custom, "whitelist", configuredCustom},
			{"nil-settings", protocol.DefaultBPSClientID, protocol.DefaultBPSClientID, nilSettings, nil, bpsDefaultConfigurationFixture()},
			{"migrate-defaults-preset", protocol.DefaultBPSClientID, protocol.DefaultBPSClientID, legacyDefaults, nil, bpsDefaultConfigurationFixture()},
			{"migrate-defaults-preserves-overrides", protocol.DefaultBPSClientID, protocol.DefaultBPSClientID, legacyOverrides, "remote-mode", migratedOverrides},
			{"replace-model-list-preserve-custom", protocol.DefaultBPSClientID, protocol.DefaultBPSClientID, custom, "remote-mode", configuredCustom},
			{"replace-model-list-preserve-whitelist", protocol.DefaultBPSClientID, protocol.DefaultBPSClientID, custom, "whitelist", configuredCustom},
			{"replace-empty-model-list-preserve-zero-group", protocol.DefaultBPSClientID, protocol.DefaultBPSClientID, emptyModels, "remote-mode", configuredZeroGroup},
			{"replace-null-model-list", protocol.DefaultBPSClientID, protocol.DefaultBPSClientID, nullModels, "remote-mode", configuredCustom},
			{"missing-model-list-uses-template", protocol.DefaultBPSClientID, protocol.DefaultBPSClientID, missingModels, "remote-mode", configuredCustom},
			{"current-model-list-is-stable", protocol.DefaultBPSClientID, protocol.DefaultBPSClientID, configuredCustom, "remote-mode", configuredCustom},
			{"leave-bps", protocol.DefaultBPSClientID, protocol.DefaultCodexClientID, cleanup, "remote-mode", nil},
		} {
			t.Run(operation+"/"+fixture.name, func(t *testing.T) {
				local := uploadFixtureAccount("owner@example.test", "fixture-new-access")
				local["group_ids"], local["proxy_id"], local["rate_multiplier"] = []int{999}, 777, 9.9
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
				localBefore, _ := json.Marshal(local)
				remoteExtra := cloneMap(fixture.extra)
				remoteExtra["client_id"] = fixture.previous
				remoteExtra["provider_option"] = "keep-extra"
				remoteExtra["openai_excel_bps_future_setting"] = "keep-bps-extension"
				for field, value := range map[string]any{
					"privacy_mode": "training_off", "codex_fingerprint_seed": "fixture-seed",
					"cost_multiplier": 1.7, "custom_route": map[string]any{"fixture-route": "preserve"},
				} {
					remoteExtra[field] = value
				}
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
					"group_ids": []any{23}, "proxy_id": 71, "concurrency": 100,
					"priority": 6, "load_factor": 17, "rate_multiplier": 1.7, "notes": "preserve billing and route settings",
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
						for _, field := range []string{"privacy_mode", "codex_fingerprint_seed", "cost_multiplier", "custom_route"} {
							if !reflect.DeepEqual(extra[field], remoteExtra[field]) {
								t.Errorf("BPS model configuration changed unrelated remote extra field %s", field)
							}
						}
						if extra["openai_request_timezone"] != "America/Los_Angeles" {
							t.Error("BPS model configuration changed the default request timezone")
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
							if extra["openai_excel_bps_future_setting"] != "keep-bps-extension" {
								t.Error("BPS model configuration removed another BPS extension setting")
							}
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
				localAfter, _ := json.Marshal(local)
				if string(localAfter) != string(localBefore) {
					t.Fatal("BPS normalization mutated the incoming authorization export")
				}
			})
		}
	}
}
