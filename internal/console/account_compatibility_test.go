package console

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"tosub2-go-port/internal/jobs"
)

func TestPublicOperationTypesMatchExistingReactLabels(t *testing.T) {
	for operation, want := range map[string]string{
		"": "initial_authorization", "full": "initial_authorization",
		"regenerate": "reauthorize", "refresh": "reauthorize", "reauthorize": "reauthorize",
		"setup-2fa": "setup_2fa", "totp_setup": "setup_2fa", "setup-totp": "setup_2fa",
		"add-password": "add_password", "password_add": "add_password", "relogin": "relogin", "resume": "resume",
	} {
		if got := publicOperationType(&jobs.Job{Operation: operation}); got != want {
			t.Errorf("%s displayed as %s, want %s", operation, got, want)
		}
	}
	if got := publicOperationType(&jobs.Job{Operation: "relogin", LastOperationType: "automatic_relogin"}); got != "automatic_relogin" {
		t.Fatal("completed repair lost its operation history", got)
	}
	if got := publicOperationType(&jobs.Job{Operation: "full", LastOperationType: "reauthorize", Attempts: 1}); got != "reauthorize" {
		t.Fatal("queued retry displayed as first authorization", got)
	}
}

func TestAccountCreationPreservesPostBodyAndExplicitCredentialClears(t *testing.T) {
	s, m := newMonitorTestServer(t)
	w := frontendRequest(s, "POST", "/api/mail-request-config", map[string]any{"config": map[string]any{"method": "POST", "url": "https://mail.example/api"}}, "test")
	if w.Code != http.StatusOK {
		t.Fatal(w.Code)
	}
	w = frontendRequest(s, "POST", "/api/jobs", map[string]any{
		"email": "mail@example.com", "password": "Fixture-password9!", "totpSecret": "jbsw y3dp ehpk 3pxp=", "proxyUrl": "socks5h://user:fixture@proxy.example:1080", "mailRequestBody": "{\"mailbox\":\"synthetic-token\"}",
	}, "test")
	if w.Code != http.StatusCreated {
		t.Fatal(w.Code, w.Body.String())
	}
	j := m.List()[0]
	input, _ := m.InputCopy(j.ID)
	if input["mailRequestBody"] != "{\"mailbox\":\"synthetic-token\"}" || input["mailApiUrl"] != "https://mail.example/api" || input["totpSecret"] != "JBSWY3DPEHPK3PXP" {
		t.Fatal("top-level POST mail body or normalized TOTP was discarded")
	}
	for _, secret := range []string{"Fixture-password9!", "synthetic-token", "JBSWY3DPEHPK3PXP", "user:fixture"} {
		if strings.Contains(w.Body.String(), secret) {
			t.Fatal("private account input leaked")
		}
	}
	w = frontendRequest(s, "POST", "/api/jobs", map[string]any{"email": j.Email, "password": "", "totpSecret": "", "proxyUrl": "", "mailApiUrl": "", "mailRequestBody": ""}, "test")
	got, _ := m.Get(j.ID)
	if w.Code != http.StatusOK || got.HasPassword || got.HasTOTPKey || got.ProxyConfigured || got.MailAPIURL != "" || got.LoginMode != "email_otp" {
		t.Fatal("explicit clears did not reset stored input", w.Code)
	}
	input, _ = m.InputCopy(j.ID)
	for _, key := range []string{"password", "totpSecret", "proxyUrl", "mailApiUrl", "mailRequestBody"} {
		if input[key] != "" {
			t.Fatalf("%s was not cleared", key)
		}
	}
}

func TestCreateValidationRejectsEntireBatchBeforeMutation(t *testing.T) {
	cases := []struct {
		name, path string
		body       any
	}{
		{"email", "/api/jobs/batch", map[string]any{"jobs": []any{map[string]any{"email": "valid@example.com"}, map[string]any{"email": "invalid"}}}},
		{"proxy", "/api/jobs/batch", map[string]any{"jobs": []any{map[string]any{"email": "valid@example.com"}, map[string]any{"email": "other@example.com", "proxyUrl": "file:///private/secrets"}}}},
		{"totp", "/api/jobs", map[string]any{"email": "valid@example.com", "totpSecret": "not-a-base32-secret"}},
		{"mail", "/api/jobs", map[string]any{"email": "valid@example.com", "mailApiUrl": "file:///private/secrets"}},
		{"nested_type", "/api/jobs", map[string]any{"email": "valid@example.com", "input": map[string]any{"password": 123}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, m := newMonitorTestServer(t)
			w := frontendRequest(s, "POST", tc.path, tc.body, "test")
			if w.Code != http.StatusBadRequest || len(m.List()) != 0 {
				t.Fatal("invalid request partially created jobs", w.Code)
			}
		})
	}
}

func TestEmailArrayBatchPreservesCommonInputs(t *testing.T) {
	s, m := newMonitorTestServer(t)
	w := frontendRequest(s, "POST", "/api/jobs/batch", map[string]any{"emails": []string{"first@example.com", "second@example.com"}, "password": "Fixture9!", "proxyUrl": "http://proxy.example:8080"}, "test")
	if w.Code != http.StatusCreated || len(m.List()) != 2 {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, job := range m.List() {
		input, _ := m.InputCopy(job.ID)
		if input["password"] != "Fixture9!" || input["proxyUrl"] != "http://proxy.example:8080" {
			t.Fatal("common batch inputs were ignored")
		}
	}
}

func TestGetMailboxRequestPreservesConfiguredHeaders(t *testing.T) {
	s, m := newMonitorTestServer(t)
	w := frontendRequest(s, "POST", "/api/mail-request-config", map[string]any{"config": map[string]any{"method": "GET", "headers": map[string]string{"Authorization": "Bearer fixture-mail-header"}}}, "test")
	if w.Code != http.StatusOK {
		t.Fatal(w.Code)
	}
	w = frontendRequest(s, "POST", "/api/jobs", map[string]any{"email": "get-mail@example.com", "mailApiUrl": "https://mail.example/messages"}, "test")
	if w.Code != http.StatusCreated || len(m.List()) != 1 {
		t.Fatal(w.Code)
	}
	input, _ := m.InputCopy(m.List()[0].ID)
	headers, ok := input["mailRequestHeaders"].(map[string]string)
	if !ok || headers["Authorization"] != "Bearer fixture-mail-header" || input["mailRequestMethod"] != "GET" {
		t.Fatal("GET mailbox auth headers were discarded without a shared POST URL")
	}
	if strings.Contains(w.Body.String(), "fixture-mail-header") {
		t.Fatal("mail API headers leaked to public job response")
	}
}

func TestConsoleNewestUniqueEmailOrderingAndFIFOPosition(t *testing.T) {
	s, m := newMonitorTestServer(t)
	_, _ = m.Submit(jobs.Spec{ID: "old", Email: "same@example.com"})
	time.Sleep(time.Millisecond)
	_, _ = m.Submit(jobs.Spec{ID: "middle", Email: "middle@example.com"})
	time.Sleep(time.Millisecond)
	_, _ = m.Submit(jobs.Spec{ID: "new", Email: "SAME@example.com"})
	w := frontendRequest(s, "GET", "/api/jobs?pageSize=1", nil, "test")
	var result struct {
		Jobs      []map[string]any `json:"jobs"`
		Selection []map[string]any `json:"selection"`
		Total     int              `json:"total"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || w.Code != http.StatusOK {
		t.Fatal("invalid response", err)
	}
	if result.Total != 2 || len(result.Selection) != 2 || len(result.Jobs) != 1 || result.Jobs[0]["id"] != "new" || result.Jobs[0]["queuePosition"] != float64(3) {
		t.Fatal("console ordering or FIFO position incompatible")
	}
	if !strings.Contains(result.Jobs[0]["prompt"].(string), "2") {
		t.Fatal("queue prompt lost waiting count")
	}
	if current, ok := m.FindByEmail("same@example.com"); !ok || current.ID != "new" {
		t.Fatal("monitor selected a hidden old duplicate")
	}
	if updated, created, err := m.SubmitUniqueEmail(jobs.Spec{Email: "same@example.com", Input: map[string]any{"password": "Fixture9!"}}); err != nil || created || updated.ID != "new" {
		t.Fatal("account update selected a hidden old duplicate")
	}
	w = frontendRequest(s, "POST", "/api/jobs/delete-batch", map[string]any{"ids": []string{"new"}}, "test")
	if w.Code != http.StatusOK || len(m.List()) != 1 || m.List()[0].ID != "middle" {
		t.Fatal("email-scoped legacy deletion failed", w.Code)
	}
}
