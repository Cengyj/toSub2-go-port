package protocol

import "strings"

const DefaultBPSModelMappingMode = "whitelist"

// ApplyBPSSettings applies the user's Sub2 BPS configuration template.
// The latest 2026-10-31 template selects nine models, superseding the earlier
// all-model compatibility setting. This routing list does not grant access.
// Other explicit remote settings, including false and zero, take priority.
// The superseded "defaults" preset is migrated to the corrected "initial" preset.
func ApplyBPSSettings(extra map[string]any, clientID string) {
	clientID = strings.TrimSpace(clientID)
	if clientID != DefaultBPSClientID {
		if _, exists := extra["openai_excel_bps"]; exists {
			extra["openai_excel_bps"] = false
		}
		for key := range extra {
			if strings.HasPrefix(key, "openai_excel_bps_") {
				delete(extra, key)
			}
		}
		return
	}
	extra["openai_excel_bps"] = true
	// Allocate a fresh list per account so consumers cannot mutate another
	// account's configuration or the template used by subsequent uploads.
	extra["openai_excel_bps_models"] = []string{
		"gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna", "gpt-6-astra",
		"gpt-6.1-sol", "gpt-6.1-terra", "gpt-6-sol", "gpt-6-luna", "codex-auto-review",
	}
	if extra["openai_excel_bps_config_mode"] == "defaults" {
		extra["openai_excel_bps_config_mode"] = "initial"
	}
	for key, value := range map[string]any{
		"openai_excel_bps_config_mode":                   "initial",
		"openai_excel_bps_ignore_encrypted_content":      true,
		"openai_excel_bps_omit_unsupported_tools":        true,
		"openai_excel_bps_auto_disable_on_403":           true,
		"openai_excel_bps_auto_move_on_403":              true,
		"openai_excel_bps_403_target_group_id":           0,
		"openai_excel_bps_403_recovery_interval_minutes": 60,
	} {
		if current, exists := extra[key]; !exists || current == nil {
			extra[key] = value
		}
	}
}

// ApplyBPSCredentialSettings fills the provider setting separately from grant
// metadata so reauthorization never overwrites a remote model-mapping policy.
func ApplyBPSCredentialSettings(credentials map[string]any, clientID string) {
	if clientID == DefaultBPSClientID {
		if value, exists := credentials["model_mapping_mode"]; !exists || value == nil {
			credentials["model_mapping_mode"] = DefaultBPSModelMappingMode
		}
	}
}
