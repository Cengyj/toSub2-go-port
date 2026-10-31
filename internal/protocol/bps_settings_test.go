package protocol

import (
	"reflect"
	"strings"
	"testing"
)

// This is the non-secret model selection from the user's latest Sub2 template.
// Keep the expectation independent from the production configuration.
func requestedBPSModels() []string {
	return []string{
		"gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna", "gpt-6-astra",
		"gpt-6.1-sol", "gpt-6.1-terra", "gpt-6-sol", "gpt-6-luna", "codex-auto-review",
	}
}

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
		if !reflect.DeepEqual(account.Extra["openai_excel_bps_models"], requestedBPSModels()) {
			t.Fatal("BPS export must include the exact requested model selection")
		}
	}
}

func TestBPSSettingsPreserveOtherOverridesAndApplyRequestedModels(t *testing.T) {
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
			continue // The user's latest model template supersedes only this field.
		}
		if !reflect.DeepEqual(extra[key], value) {
			t.Errorf("overwrote explicit %s", key)
		}
	}
	if !reflect.DeepEqual(extra["openai_excel_bps_models"], requestedBPSModels()) {
		t.Fatal("BPS settings did not apply the requested model selection")
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

func TestBPSRequestedModelsReplacePreviousSelections(t *testing.T) {
	for _, tc := range []struct {
		name   string
		models any
	}{
		{"missing", nil},
		{"null", nil},
		{"empty", []string{}},
		{"restricted", []string{"synthetic-model"}},
		{"decoded-json", []any{"synthetic-model"}},
		{"current-template", requestedBPSModels()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			extra := map[string]any{"openai_excel_bps_models": tc.models, "provider_option": "keep"}
			if tc.name == "missing" {
				delete(extra, "openai_excel_bps_models")
			}
			ApplyBPSSettings(extra, DefaultBPSClientID)
			if !reflect.DeepEqual(extra["openai_excel_bps_models"], requestedBPSModels()) {
				t.Fatal("BPS model selection differs from the requested template")
			}
			if extra["openai_excel_bps"] != true || extra["provider_option"] != "keep" {
				t.Fatal("BPS model selection changed unrelated configuration")
			}
		})
	}
}

func TestBPSRequestedModelsAreIndependentAndIdempotent(t *testing.T) {
	first, second := map[string]any{}, map[string]any{}
	ApplyBPSSettings(first, DefaultBPSClientID)
	ApplyBPSSettings(second, DefaultBPSClientID)
	models, ok := first["openai_excel_bps_models"].([]string)
	if !ok || len(models) == 0 {
		t.Fatal("missing model template")
	}
	models[0] = "synthetic-mutated-model"
	if !reflect.DeepEqual(second["openai_excel_bps_models"], requestedBPSModels()) {
		t.Fatal("accounts share mutable model configuration")
	}
	ApplyBPSSettings(first, DefaultBPSClientID)
	ApplyBPSSettings(first, DefaultBPSClientID)
	if !reflect.DeepEqual(first, second) {
		t.Fatal("applying the template twice changed the effective configuration")
	}
}
