package jobs

import (
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"testing"

	"tosub2-go-port/internal/logging"
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

func TestPublicJobRedactsCredentialURLComponentsAndRepeatedQueryValues(t *testing.T) {
	const proxyPassword = "proxy-component-p@ss/word"
	const mailTokenA, mailTokenB = "mail-component-first+value", "mail-component-second/value"
	proxy := "https://" + url.UserPassword("proxy-user", proxyPassword).String() + "@proxy.example.test:8443"
	_, encodedProxyPassword, _ := strings.Cut(url.UserPassword("proxy-user", proxyPassword).String(), ":")
	rawEncodedProxyPassword := strings.ReplaceAll(encodedProxyPassword, "%2F", "%2f")
	mail := "https://mail.example.test/inbox?access%5Ftoken=" + url.QueryEscape(mailTokenA) + "&access%5Ftoken=" + url.QueryEscape(mailTokenB) + "&label=visible-label"
	const ordinaryPassword = "ordinary-field-keeps-its-meaning"
	for _, fixture := range []struct {
		name        string
		job         *Job
		rawPassword string
	}{
		{"input-url-fields", &Job{Input: map[string]any{"proxyUrl": proxy, "mailApiUrl": mail, "note": "https://user:" + ordinaryPassword + "@reference.example.test"}}, ""},
		{"restored-mail-url", &Job{MailAPIURL: mail, Input: map[string]any{"proxyUrl": proxy}}, ""},
		{"nested-url-fields", &Job{Input: map[string]any{"configuration": map[string]any{"proxyUrl": proxy, "mailApiUrl": mail}}}, ""},
		{"raw-userinfo-encoding", &Job{Input: map[string]any{"proxyUrl": strings.ReplaceAll(proxy, encodedProxyPassword, rawEncodedProxyPassword), "mailApiUrl": mail}}, rawEncodedProxyPassword},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			private := []string{proxyPassword, encodedProxyPassword, mailTokenA, mailTokenB, url.QueryEscape(mailTokenA), url.QueryEscape(mailTokenB), proxy, mail}
			if fixture.rawPassword != "" {
				private = append(private, fixture.rawPassword)
			}
			fixture.job.Prompt = strings.Join(private, " ") + " visible-label " + ordinaryPassword
			fixture.job.PhoneError = fixture.job.Prompt
			view := SanitizeJobForPublic(fixture.job)
			for _, value := range private {
				if strings.Contains(view.Prompt, value) || strings.Contains(view.PhoneError, value) {
					t.Errorf("public diagnostic leaked an independently rendered private URL component %q", value)
				}
			}
			if !strings.Contains(view.Prompt, "visible-label") || !strings.Contains(view.Prompt, ordinaryPassword) {
				t.Error("secret extraction reinterpreted an unrelated field or non-sensitive query value")
			}
		})
	}
}

func TestPublicJobProjectionRetainsEveryNonemptyDiagnosticProtection(t *testing.T) {
	const secret = "unlabelled-public-canary"
	job := &Job{Email: "owner@example.test", Input: map[string]any{"headers": map[string]string{"X-Fixture": secret}}}
	text := secret + " owner@example.test Bearer upstream-token https://private.example.test/path verification code ABCD"
	fields := []*string{&job.Error, &job.LastError, &job.OperationError, &job.Prompt, &job.MailAPIError, &job.PhoneError, &job.TOTPSetupError, &job.TOTPResetError, &job.LogoutAllDevicesError, &job.TOTPWorkflowError, &job.ReloginUploadError, &job.PasswordAddError, &job.SMSError, &job.AutoRepairBlockedReason, &job.AutoRepairLastError, &job.LastAuthAutomationReason, &job.AutoRepairSkippedReason, &job.SMSServiceLabel}
	for _, field := range fields {
		*field = text
	}
	got := SanitizeJobForPublic(job)
	want := logging.RedactKnown(text, secret, job.Email)
	for _, field := range []string{got.Error, got.LastError, got.OperationError, got.Prompt, got.MailAPIError, got.PhoneError, got.TOTPSetupError, got.TOTPResetError, got.LogoutAllDevicesError, got.TOTPWorkflowError, got.ReloginUploadError, got.PasswordAddError, got.SMSError, got.AutoRepairBlockedReason, got.AutoRepairLastError, got.LastAuthAutomationReason, got.AutoRepairSkippedReason, got.SMSServiceLabel} {
		if field != want || strings.Contains(field, "canary") || strings.Contains(field, "upstream-token") || strings.Contains(field, "ABCD") {
			t.Fatal("public projection skipped a nonempty diagnostic protection")
		}
	}
	if job.Prompt != text || job.Input["headers"].(map[string]string)["X-Fixture"] != secret {
		t.Fatal("public sanitization changed its input")
	}
	empty := SanitizeJobForPublic(&Job{})
	if empty.Prompt != "" || empty.Error != "" || empty.LastError != "" || empty.SMSServiceLabel != "" {
		t.Fatal("empty diagnostics gained synthetic content")
	}
}
