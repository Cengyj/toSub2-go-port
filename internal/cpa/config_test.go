package cpa

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSettingsNormalizeBaseAndExcludeSecretFromJSON(t *testing.T) {
	for _, raw := range []string{"https://cpa.example/proxy", "https://cpa.example/proxy/", "https://cpa.example/proxy/v0/management/"} {
		cfg, err := ParseUploadSettings(map[string]any{"baseUrl": raw, "adminApiKey": "private-management-key"})
		if err != nil || cfg.BaseURL != "https://cpa.example/proxy" || cfg.APIKey != "private-management-key" {
			t.Fatalf("settings: %v", err)
		}
		data, _ := json.Marshal(cfg)
		if strings.Contains(string(data), "private-management-key") {
			t.Fatal("serialized config contains secret")
		}
	}
	for _, raw := range []string{"", "ftp://cpa.example", "https://user:secret@cpa.example", "https://cpa.example?key=secret", "https://cpa.example?", "https://cpa.example/#fragment", "http://", "https://cpa.example/a/../b", "https://cpa.example/v0/management/auth-files"} {
		if _, err := ValidateBaseURL(raw); err == nil {
			t.Errorf("accepted invalid base %q", raw)
		}
	}
	for _, value := range []any{nil, map[string]any{"baseUrl": "https://cpa.example"}, map[string]any{"baseUrl": "https://cpa.example", "adminApiKey": "secret\r\nHeader: bad"}} {
		if _, err := ParseUploadSettings(value); err == nil {
			t.Error("accepted invalid settings")
		}
	}
}
