package protocol

import "strings"

const DefaultBPSModelMappingMode = "whitelist"

// ApplyBPSSettings adds the user's Sub2 BPS configuration template (2026-10-13).
// These model names are configured defaults, not a discovered entitlement list.
// Explicit remote settings, including false, zero and empty lists, take priority.
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
		"openai_excel_bps_models":                        []string{"gpt-6-astra", "gpt-5.6-sol", "gpt-5.6-terra"},
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
