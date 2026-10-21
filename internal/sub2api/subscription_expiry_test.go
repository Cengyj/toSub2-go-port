package sub2api

import (
	"encoding/json"
	"testing"

	"tosub2-go-port/internal/protocol"
)

func TestUploadNormalizesSavedSubscriptionClaimWithoutMutatingSource(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value any
		want  string
	}{
		{"provider-active-until", "2031-01-01T08:00:00+08:00", "2031-01-01T00:00:00Z"},
		{"provider-explicit-unknown", nil, ""},
		{"provider-invalid-date", "synthetic-invalid", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			token := oauthMetadataFixtureJWT(t, map[string]any{"auth": map[string]any{"chatgpt_account_id": "workspace", "chatgpt_plan_type": "plus", "chatgpt_subscription_active_until": tc.value}})
			source := uploadFixtureAccount("expiry@example.test", token)
			c := source["credentials"].(map[string]any)
			c["client_id"] = protocol.DefaultCodexClientID
			c["chatgpt_account_id"] = "workspace"
			c["plan_type"] = "plus"
			before, _ := json.Marshal(source)
			payload, err := BuildUploadPayload([]map[string]any{source}, UploadOptions{})
			if err != nil {
				t.Fatal(err)
			}
			built := payload["accounts"].([]any)[0].(map[string]any)
			incoming := built["credentials"].(map[string]any)
			if got, reported := incoming["subscription_expires_at"]; !reported || got != tc.want {
				t.Fatalf("subscription expiry=%v reported=%v want=%q", got, reported, tc.want)
			}
			remote := map[string]any{"client_id": protocol.DefaultCodexClientID, "chatgpt_account_id": "workspace", "plan_type": "plus", "subscription_expires_at": "2030-01-01T00:00:00Z", "model_mapping": "synthetic-setting"}
			extra := map[string]any{"subscription_expires_at": "2030-01-01T00:00:00Z", "timezone": "America/Los_Angeles"}
			merged := mergeOAuthCredentials(remote, extra, incoming)
			mergedExtra := mergeOAuthExtra(remote, extra, incoming)
			if stringValue(merged["subscription_expires_at"]) != tc.want || stringValue(mergedExtra["subscription_expires_at"]) != tc.want {
				t.Fatal("update retained a stale subscription expiry")
			}
			if merged["model_mapping"] != remote["model_mapping"] || mergedExtra["timezone"] != extra["timezone"] {
				t.Fatal("expiry normalization changed provider settings")
			}
			after, _ := json.Marshal(source)
			if string(before) != string(after) {
				t.Fatal("upload normalization rewrote the saved grant")
			}
		})
	}
}

func TestUploadDoesNotBorrowExpiryFromMismatchedSavedClaims(t *testing.T) {
	for _, change := range []func(map[string]any){
		func(c map[string]any) { c["chatgpt_account_id"] = "different-workspace" },
		func(c map[string]any) { c["plan_type"] = "free" },
	} {
		c := map[string]any{"chatgpt_account_id": "workspace", "plan_type": "plus", "access_token": oauthMetadataFixtureJWT(t, map[string]any{"auth": map[string]any{"chatgpt_account_id": "workspace", "chatgpt_plan_type": "plus", "chatgpt_subscription_active_until": "2031-01-01T00:00:00Z"}})}
		change(c)
		if err := normalizeOAuthAuthorization(c, map[string]any{}); err != nil {
			t.Fatal(err)
		}
		if stringValue(c["subscription_expires_at"]) != "" {
			t.Fatal("normalization borrowed another workspace or tier's expiry")
		}
	}
}
