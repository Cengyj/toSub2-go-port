package console

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tosub2-go-port/internal/jobs"
)

func TestConsoleMailboxStatusEventsRemainPublicAndSanitized(t *testing.T) {
	s, manager := newMonitorTestServer(t)
	job, err := manager.Submit(jobs.Spec{Email: "mail-events@example.test"})
	if err != nil {
		t.Fatal(err)
	}
	for _, status := range []string{"ready", "polling", "found", "error", "timeout"} {
		message := ""
		if status == "error" || status == "timeout" {
			message = "mailbox unavailable api_key=fixture-mail-key code 123456"
		}
		manager.Emit(jobs.JobEvent{JobID: job.ID, Stage: "email_otp", Status: jobs.StatusWaitingInput, Data: map[string]any{"mailStatus": status, "mailApiError": message, "providerResponse": "unlabelled-provider-token"}})
		for _, method := range []string{"GET", "POST"} {
			path := "/api/jobs"
			var body any
			if method == "POST" {
				path += "/query"
				body = map[string]any{"emails": []string{job.Email}}
			}
			w := frontendRequest(s, method, path, body, "test")
			var result struct {
				Jobs []map[string]any `json:"jobs"`
			}
			if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &result) != nil || len(result.Jobs) != 1 {
				t.Fatal("mail status response unavailable", w.Code)
			}
			current := result.Jobs[0]
			if current["mailStatus"] != status || current["status"] != "email_otp" || (current["mailApiError"] == "") != (message == "") {
				t.Fatal("public mailbox state did not match structured event")
			}
			for _, secret := range []string{"fixture-mail-key", "123456", "unlabelled-provider-token"} {
				if strings.Contains(w.Body.String(), secret) {
					t.Fatal("mailbox status API leaked provider response")
				}
			}
		}
	}
	manager.Emit(jobs.JobEvent{JobID: job.ID, Data: map[string]any{"mailStatus": "found", "mailApiError": ""}})
	current, _ := manager.Get(job.ID)
	if current.MailAPIError != "" {
		t.Fatal("successful mail poll retained old error")
	}
	data, err := os.ReadFile(filepath.Join(s.cfg.OutputRoot, "job-meta.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"fixture-mail-key", "123456", "unlabelled-provider-token"} {
		if strings.Contains(string(data), secret) {
			t.Fatal("mail event persisted private material")
		}
	}
}
