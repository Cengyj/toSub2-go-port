package jobs

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestStructuredMailEventsAllowOnlyPublicTypedValues(t *testing.T) {
	m := NewManager(context.Background(), 1, nil, nil)
	j, err := m.Submit(Spec{Email: "mail@example.test"})
	if err != nil {
		t.Fatal(err)
	}
	events, unsubscribe := m.Subscribe(10)
	defer unsubscribe()
	for _, status := range []string{"ready", "polling", "found", "error", "timeout"} {
		input := map[string]any{"mailStatus": status, "mailApiError": "api_key=fixture-mail-key code 123456 https://mail.example/private", "providerBody": "unlabelled-private-value", "Cookie": "fixture-cookie"}
		m.Emit(JobEvent{JobID: j.ID, Stage: "email_otp", Status: StatusWaitingInput, Data: input})
		current, _ := m.Get(j.ID)
		if current.MailStatus != status || current.MailAPIError == "" || current.Status != "email_otp" {
			t.Fatal("mail event did not update waiting job")
		}
		event := <-events
		b, _ := json.Marshal(event)
		for _, secret := range []string{"fixture-mail-key", "123456", "mail.example", "unlabelled-private-value", "fixture-cookie"} {
			if strings.Contains(string(b), secret) || strings.Contains(current.MailAPIError, secret) {
				t.Fatal("mail event leaked provider material")
			}
		}
		if len(event.Data) != 2 || input["Cookie"] != "fixture-cookie" {
			t.Fatal("event allowlist failed or mutated producer metadata")
		}
	}
	for _, data := range []map[string]any{
		{"mailStatus": "fixture-token", "mailApiError": map[string]any{"error": "private"}},
		{"mailStatus": 123456, "mailApiError": 123456},
		{"mailStatus": nil, "mailApiError": nil},
	} {
		m.Emit(JobEvent{JobID: j.ID, Data: data})
		current, _ := m.Get(j.ID)
		if current.MailStatus != "timeout" || current.MailAPIError == "" || len((<-events).Data) != 0 {
			t.Fatal("unrecognized mailbox state or non-string error was accepted")
		}
	}
	m.Emit(JobEvent{JobID: j.ID, Data: map[string]any{"mailStatus": "ready", "mailApiError": ""}})
	current, _ := m.Get(j.ID)
	if current.MailStatus != "ready" || current.MailAPIError != "" {
		t.Fatal("explicit empty mailbox error did not clear old failure")
	}
	long := SanitizeJobEvent(JobEvent{Data: map[string]any{"mailApiError": "password=fixture-password " + strings.Repeat("界", 600)}})
	message, ok := long.Data["mailApiError"].(string)
	if !ok || len([]rune(message)) != 512 || strings.Contains(message, "fixture-password") {
		t.Fatal("mailbox error was not redacted before safe character truncation")
	}
}
