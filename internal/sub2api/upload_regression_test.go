package sub2api

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// Mirrors the account payload assertions in legacy/test/console-smoke.mjs.
func TestLegacyUploadOptionsAndFingerprintDefaults(t *testing.T) {
	concurrency, load, priority := 10, 100, 1
	account := map[string]any{
		"name": "oauth---fixture@example.com", "platform": "openai", "type": "oauth",
		"credentials": map[string]any{"email": "fixture@example.com", "access_token": "synthetic-token"},
		"extra":       map[string]any{"existing": true},
	}
	options := UploadOptions{GroupIDs: []int{7, 8}, ProxyID: 3, Concurrency: &concurrency, LoadFactor: &load, Priority: &priority, ModelWhitelist: []string{"gpt-5", "gpt-5-mini"}, CodexFingerprintMode: "full"}
	payload, err := BuildUploadPayload([]map[string]any{account}, options)
	if err != nil {
		t.Fatal(err)
	}
	a := payload["accounts"].([]any)[0].(map[string]any)
	for key, want := range map[string]any{"group_ids": []int{7, 8}, "proxy_id": 3, "concurrency": 10, "load_factor": 100, "priority": 1, "status": "active", "schedulable": true} {
		if !reflect.DeepEqual(a[key], want) {
			t.Errorf("%s: got %v want %v", key, a[key], want)
		}
	}
	if !reflect.DeepEqual(a["credentials"].(map[string]any)["model_mapping"], map[string]any{"gpt-5": "gpt-5", "gpt-5-mini": "gpt-5-mini"}) || a["extra"].(map[string]any)["codex_fingerprint_mode"] != "full" {
		t.Fatal("model mapping or fingerprint mode changed")
	}
	if account["credentials"].(map[string]any)["model_mapping"] != nil || account["extra"].(map[string]any)["codex_fingerprint_mode"] != nil {
		t.Fatal("upload options mutated saved account")
	}
	for _, mode := range []string{"", "off", "device", "session", "full"} {
		payload, err := BuildUploadPayload([]map[string]any{account}, UploadOptions{CodexFingerprintMode: mode})
		if err != nil {
			t.Fatal(err)
		}
		want := mode
		if want == "" {
			want = "session"
		}
		if payload["accounts"].([]any)[0].(map[string]any)["extra"].(map[string]any)["codex_fingerprint_mode"] != want {
			t.Fatalf("mode %q not preserved", mode)
		}
	}
	if _, err := BuildUploadPayload([]map[string]any{account}, UploadOptions{CodexFingerprintMode: "invalid"}); err == nil {
		t.Fatal("invalid fingerprint mode accepted")
	}
}

func TestLegacyNumericUploadResultPreservedWithoutSecrets(t *testing.T) {
	var raw any
	if err := json.Unmarshal([]byte(`{"success":1,"failed":0,"results":[{"access_token":"synthetic-secret"}],"adminApiKey":"synthetic-key","data":{"success":2,"refresh_token":"synthetic-refresh"}}`), &raw); err != nil {
		t.Fatal(err)
	}
	got := PublicUploadResult(raw)
	if got["success"] != float64(1) || got["failed"] != float64(0) || got["data"].(map[string]any)["success"] != float64(2) {
		t.Fatal("numeric legacy result counters lost")
	}
	b, _ := json.Marshal(got)
	if strings.Contains(string(b), "synthetic") || strings.Contains(string(b), "token") || strings.Contains(string(b), "adminApiKey") {
		t.Fatal("secret leaked in public upload response")
	}
	if PublicUploadResult(map[string]any{"success": true})["success"] != true {
		t.Fatal("boolean success compatibility lost")
	}
}
