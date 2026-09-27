package jobs

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestSanitizeEventAndPublicJob(t *testing.T) {
	e := JobEvent{Stage: "email_otp", Status: "running", Prompt: "received code 123456", Data: map[string]any{"nested": map[string]any{"REFRESH_TOKEN": "refresh-secret"}, "Cookie": "cookie-secret"}}
	safe := SanitizeJobEvent(e)
	b, _ := json.Marshal(safe)
	for _, secret := range []string{"123456", "refresh-secret", "cookie-secret"} {
		if strings.Contains(string(b), secret) {
			t.Fatal("event leaked secret")
		}
	}
	if e.Data["Cookie"] != "cookie-secret" {
		t.Fatal("source mutated")
	}
	j := &Job{Input: map[string]any{"password": "unlabelled-pass"}, Prompt: "failed unlabelled-pass", Logs: []string{"Authorization: Bearer log-secret"}, TOTPSetupSecret: "setup-secret"}
	out := SanitizeJobForPublic(j)
	b, _ = json.Marshal(out)
	for _, secret := range []string{"unlabelled-pass", "log-secret", "setup-secret"} {
		if strings.Contains(string(b), secret) {
			t.Fatal("job leaked secret")
		}
	}
	if j.Input["password"] != "unlabelled-pass" {
		t.Fatal("private job changed")
	}
	if got := SafeJobError(errors.New(`{"apiKey":"error-secret"}`)); strings.Contains(got, "error-secret") {
		t.Fatal("error leak")
	}
}
