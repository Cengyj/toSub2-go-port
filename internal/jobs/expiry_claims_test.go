package jobs

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"tosub2-go-port/internal/store"
)

func expiryClaimsToken(t *testing.T, auth map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"exp": 2000000000, "https://api.openai.com/auth": auth})
	if err != nil {
		t.Fatal(err)
	}
	return "synthetic." + base64.RawURLEncoding.EncodeToString(raw) + ".fixture"
}

func TestOutputSubscriptionExpiryRecoversSavedClaimsSafely(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(map[string]any, map[string]any, map[string]any)
		want   string
	}{
		{"saved-id-subscription-claim", nil, "2031-01-01T00:00:00Z"},
		{"saved-access-subscription-claim", func(c, e, auth map[string]any) { c["access_token"] = expiryClaimsToken(t, auth); delete(c, "id_token") }, "2031-01-01T00:00:00Z"},
		{"new-access-tier-with-retained-id", func(c, e, auth map[string]any) {
			delete(c, "plan_type")
			auth["chatgpt_plan_type"] = "pro"
			auth["chatgpt_subscription_active_until"] = "2032-01-01T00:00:00Z"
			c["access_token"] = expiryClaimsToken(t, auth)
		}, "2032-01-01T00:00:00Z"},
		{"explicit-date-wins", func(c, e, auth map[string]any) { c["subscription_expires_at"] = "2032-01-01T00:00:00Z" }, "2032-01-01T00:00:00Z"},
		{"extra-date-wins", func(c, e, auth map[string]any) { e["subscription_expires_at"] = "2032-01-01T00:00:00Z" }, "2032-01-01T00:00:00Z"},
		{"explicit-null-stays-unknown", func(c, e, auth map[string]any) { c["subscription_expires_at"] = nil }, ""},
		{"explicit-invalid-stays-unknown", func(c, e, auth map[string]any) { c["subscription_expires_at"] = "synthetic-invalid" }, ""},
		{"different-export-workspace", func(c, e, auth map[string]any) { c["chatgpt_account_id"] = "different-workspace" }, ""},
		{"different-export-plan", func(c, e, auth map[string]any) { c["plan_type"] = "free" }, ""},
		{"unbound-token-workspace", func(c, e, auth map[string]any) {
			delete(auth, "chatgpt_account_id")
			c["id_token"] = expiryClaimsToken(t, auth)
		}, ""},
		{"other-timestamps-are-not-expiry", func(c, e, auth map[string]any) {
			delete(auth, "chatgpt_subscription_active_until")
			auth["chatgpt_subscription_active_start"] = "2030-01-01T00:00:00Z"
			auth["chatgpt_subscription_last_checked"] = "2030-02-01T00:00:00Z"
			c["id_token"] = expiryClaimsToken(t, auth)
		}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			private, err := store.New(filepath.Join(root, "saved"))
			if err != nil {
				t.Fatal(err)
			}
			auth := map[string]any{"chatgpt_account_id": "workspace", "chatgpt_plan_type": "plus", "chatgpt_subscription_active_until": "2031-01-01T08:00:00+08:00"}
			c := map[string]any{"email": "expiry@example.test", "chatgpt_account_id": "workspace", "plan_type": "plus", "id_token": expiryClaimsToken(t, auth), "access_token": "synthetic-access"}
			e := map[string]any{}
			if tc.change != nil {
				tc.change(c, e, auth)
			}
			if err := private.WriteJSON(t.Context(), "sub2api-import-oauth.json", map[string]any{"type": "sub2api-data", "accounts": []any{map[string]any{"credentials": c, "extra": e}}}); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(filepath.Join(private.Root, "sub2api-import-oauth.json"))
			if err != nil {
				t.Fatal(err)
			}
			got, found := outputSubscriptionExpiry(root, "saved", "expiry@example.test")
			if !found || got != tc.want {
				t.Fatalf("expiry=%q found=%v want=%q", got, found, tc.want)
			}
			if value, matched := outputSubscriptionExpiry(root, "saved", "other@example.test"); matched || value != "" {
				t.Fatal("recovered another account's subscription")
			}
			after, err := os.ReadFile(filepath.Join(private.Root, "sub2api-import-oauth.json"))
			if err != nil || string(before) != string(after) {
				t.Fatal("metadata inspection rewrote the saved grant")
			}
		})
	}
}

func TestRestoreBackfillsSubscriptionWithoutChangingGrantOrManualExpiry(t *testing.T) {
	for _, manual := range []string{"", "2035-01-01T00:00:00Z"} {
		st, err := store.New(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		m := NewManager(t.Context(), 1, st, nil)
		job, err := m.Submit(Spec{ID: "saved-expiry", Email: "expiry@example.test", ImportOnly: true, Input: map[string]any{"manualExpiresAt": manual}})
		if err != nil {
			t.Fatal(err)
		}
		private, err := store.New(filepath.Join(st.Root, job.ID))
		if err != nil {
			t.Fatal(err)
		}
		token := expiryClaimsToken(t, map[string]any{"chatgpt_account_id": "workspace", "chatgpt_plan_type": "plus", "chatgpt_subscription_active_until": "2031-01-01T00:00:00Z"})
		document := map[string]any{"type": "sub2api-data", "accounts": []any{map[string]any{"credentials": map[string]any{"email": job.Email, "chatgpt_account_id": "workspace", "plan_type": "plus", "access_token": "synthetic-access", "id_token": token}}}}
		if err := private.WriteJSON(t.Context(), "sub2api-import-oauth.json", document); err != nil {
			t.Fatal(err)
		}
		before, err := os.ReadFile(filepath.Join(private.Root, "sub2api-import-oauth.json"))
		if err != nil {
			t.Fatal(err)
		}
		m.cancel()
		for range 2 {
			restored := NewManager(t.Context(), 1, st, nil)
			if err := restored.Restore(t.Context()); err != nil {
				t.Fatal(err)
			}
			current, ok := restored.Get(job.ID)
			if !ok || current.SubscriptionExpiresAt != "2031-01-01T00:00:00Z" || current.ManualExpiresAt != manual || current.Attempts != job.Attempts || len(restored.pending) != 0 {
				t.Fatal("restore lost subscription/manual metadata or admitted authentication")
			}
			want, source := current.SubscriptionExpiresAt, "subscription"
			if manual != "" {
				want, source = manual, "manual"
			}
			if got, gotSource := EffectiveExpiry(current); got != want || gotSource != source {
				t.Fatal("restore changed expiry precedence")
			}
			restored.cancel()
		}
		after, err := os.ReadFile(filepath.Join(private.Root, "sub2api-import-oauth.json"))
		if err != nil || string(before) != string(after) {
			t.Fatal("subscription backfill rewrote the saved grant")
		}
	}
}
