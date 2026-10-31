package protocol

import (
	"reflect"
	"strings"
	"testing"
)

func TestBPSExportUsesRequestedSub2Configuration(t *testing.T) {
	tokens := TokenSet{
		AccessToken:  base64JWT(`{"email":"fixture@example.test","auth":{"chatgpt_account_id":"fixture-workspace"}}`),
		IDToken:      base64JWT(`{"email":"fixture@example.test","auth":{"chatgpt_account_id":"fixture-workspace"}}`),
		RefreshToken: "fixture-refresh",
	}
	for _, client := range []string{DefaultBPSClientID, DefaultCodexClientID} {
		exported, err := BuildSub2APIOAuthExport(tokens, client, "fixture@example.test", "", 10, 1, 1)
		if err != nil {
			t.Fatal(err)
		}
		account := exported.Accounts[0]
		if client == DefaultCodexClientID {
			for key := range account.Extra {
				if strings.HasPrefix(key, "openai_excel_bps") {
					t.Fatalf("ChatGPT export gained BPS setting %s", key)
				}
			}
			if account.Credentials.ModelMappingMode != "" {
				t.Fatal("BPS template changed ChatGPT model configuration")
			}
			continue
		}
		for key, want := range map[string]any{
			"openai_excel_bps":                               true,
			"openai_excel_bps_config_mode":                   "initial",
			"openai_excel_bps_ignore_encrypted_content":      true,
			"openai_excel_bps_omit_unsupported_tools":        true,
			"openai_excel_bps_auto_disable_on_403":           true,
			"openai_excel_bps_auto_move_on_403":              true,
			"openai_excel_bps_403_target_group_id":           0,
			"openai_excel_bps_403_recovery_interval_minutes": 60,
		} {
			if !reflect.DeepEqual(account.Extra[key], want) {
				t.Errorf("BPS export configuration %s does not match template", key)
			}
		}
		if account.Credentials.ModelMappingMode != "whitelist" {
			t.Fatal("BPS export is missing its model mapping mode")
		}
		if _, present := account.Extra["openai_excel_bps_models"]; present {
			t.Fatal("BPS export must enable all-model compatibility by omitting the model selection")
		}
	}
}

func TestBPSSettingsPreserveOtherOverridesAndEnableAllModels(t *testing.T) {
	extra := map[string]any{
		"openai_excel_bps_config_mode":                   "custom",
		"openai_excel_bps_ignore_encrypted_content":      false,
		"openai_excel_bps_omit_unsupported_tools":        false,
		"openai_excel_bps_auto_disable_on_403":           false,
		"openai_excel_bps_auto_move_on_403":              false,
		"openai_excel_bps_403_target_group_id":           17,
		"openai_excel_bps_403_recovery_interval_minutes": 0,
		"openai_excel_bps_models":                        []string{},
	}
	before := map[string]any{}
	for key, value := range extra {
		before[key] = value
	}
	ApplyBPSSettings(extra, DefaultBPSClientID)
	for key, value := range before {
		if key == "openai_excel_bps_models" {
			continue // The user's all-model selection supersedes only this field.
		}
		if !reflect.DeepEqual(extra[key], value) {
			t.Errorf("overwrote explicit %s", key)
		}
	}
	if _, present := extra["openai_excel_bps_models"]; present {
		t.Fatal("BPS settings retained an explicit model selection instead of enabling all models")
	}
	extra["codex_fingerprint_seed"], extra["privacy_mode"] = "fixture-seed", "training_off"
	ApplyBPSSettings(extra, DefaultCodexClientID)
	for key := range extra {
		if strings.HasPrefix(key, "openai_excel_bps_") {
			t.Errorf("provider switch retained %s", key)
		}
	}
	if extra["codex_fingerprint_seed"] != "fixture-seed" || extra["privacy_mode"] != "training_off" {
		t.Fatal("provider switch changed shared settings")
	}
}

func TestBPSAllModelCompatibilityRemovesPreviousSelections(t *testing.T) {
	for _, tc := range []struct {
		name   string
		models any
	}{
		{"null", nil},
		{"empty", []string{}},
		{"restricted", []string{"synthetic-model"}},
		{"decoded-json", []any{"synthetic-model"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			extra := map[string]any{"openai_excel_bps_models": tc.models, "provider_option": "keep"}
			ApplyBPSSettings(extra, DefaultBPSClientID)
			if _, present := extra["openai_excel_bps_models"]; present {
				t.Fatal("all-model compatibility requires an absent field, not an empty or null selection")
			}
			if extra["openai_excel_bps"] != true || extra["provider_option"] != "keep" {
				t.Fatal("BPS all-model selection changed unrelated configuration")
			}
		})
	}
}
