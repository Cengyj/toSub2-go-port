package jobs

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestSMSLabelPreservesServiceIdentifierAndRemovesKnownSecrets(t *testing.T) {
	if got := SanitizeSMSServiceLabel("121949"); got != "121949" {
		t.Fatal("numeric supplier ID was treated as a verification code")
	}
	if got := SanitizeSMSServiceLabel("Fixture Country / fixture-provider-secret", "fixture-provider-secret"); strings.Contains(got, "fixture-provider-secret") || !strings.Contains(got, "Fixture Country") {
		t.Fatal("provider credential was not safely removed from its display label")
	}
	m := NewManager(context.Background(), 1, nil, nil)
	j, err := m.Submit(Spec{Email: "label-security@example.test", Input: map[string]any{"password": "Synthetic-password9!"}})
	if err != nil {
		t.Fatal(err)
	}
	m.UpdateSMS(j.ID, "smsbower", "SMSBower", "+12025550123", "waiting_sms", "", "Fixture / Synthetic-password9!")
	j, _ = m.Get(j.ID)
	data, _ := json.Marshal(SanitizeJobForPublic(j))
	if strings.Contains(string(data), "Synthetic-password9!") {
		t.Fatal("service label leaked an account credential")
	}
	m.UpdateSMS(j.ID, "luban", "LubanSMS", "", "requesting", "")
	j, _ = m.Get(j.ID)
	if j.SMSServiceLabel != "" {
		t.Fatal("a different provider inherited the previous service label")
	}
}
