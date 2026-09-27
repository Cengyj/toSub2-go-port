package sub2api

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestLegacyUploadPreservesAccountExtensionsAndExistingProxy(t *testing.T) {
	var source map[string]any
	if err := json.Unmarshal([]byte(`{"name":"fixture@example.com","platform":"openai","type":"oauth","credentials":{"access_token":"synthetic-token","email":"fixture@example.com"},"proxy_key":"local-proxy-reference","proxy_id":17,"notes":"synthetic imported account","expires_at":1790475630,"extra":{"custom_flag":true},"future_extension":{"compatible":true}}`), &source); err != nil {
		t.Fatal(err)
	}
	before, _ := json.Marshal(source)
	for _, proxyID := range []int{0, 23} {
		payload, err := BuildUploadPayload([]map[string]any{source}, UploadOptions{ProxyID: proxyID})
		if err != nil {
			t.Fatal(err)
		}
		account := payload["accounts"].([]any)[0].(map[string]any)
		for _, key := range []string{"notes", "expires_at", "future_extension"} {
			if !reflect.DeepEqual(account[key], source[key]) {
				t.Errorf("legacy imported account field %s was discarded: got %v, want %v", key, account[key], source[key])
			}
		}
		if proxyID == 0 && account["proxy_id"] != float64(17) {
			t.Error("existing remote proxy selection was discarded")
		}
		if proxyID != 0 && account["proxy_id"] != proxyID {
			t.Error("explicit upload proxy did not override saved selection")
		}
		if _, exists := account["proxy_key"]; exists {
			t.Error("local proxy reference must not enter the remote batch endpoint")
		}
		groups, err := json.Marshal(account["group_ids"])
		if err != nil || string(groups) != "[]" {
			t.Errorf("legacy upload must explicitly default group_ids to an empty list, got %s", groups)
		}
		if account["status"] != "active" || account["schedulable"] != true || account["extra"].(map[string]any)["custom_flag"] != true {
			t.Fatal("legacy activation defaults or existing extra fields changed")
		}
	}
	after, _ := json.Marshal(source)
	if string(after) != string(before) {
		t.Fatal("upload transforms modified the saved import account")
	}
}
