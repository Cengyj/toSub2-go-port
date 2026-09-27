package console

import (
	"net/http"
	"strings"
	"testing"

	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/jobs"
)

func TestLegacyConsoleSmokeAccountFormats(t *testing.T) {
	mail := "http://127.0.0.1:4399/api/bootstrap"
	lines := []string{
		"password-mail@example.com---test-password----" + mail,
		"password-mail-totp@example.com----test-password-2----" + mail + "----JBSWY3DPEHPK3PXP",
		mail + "|JBSWY3DPEHPK3PXP|unordered-password|smart.user-name@sub.example.co.uk",
		"pipe-password@example.dev|part-one|part-two|" + mail + "|NB2W45DFOIZAQWER",
		"MFRGGZDFMZTWQ2LK::colon-password::name+tag@sub-domain.example.cloud",
		mail + ",reverse.api-order@example.xyz",
		"manual-totp@example.net--------ONSWG4TFOQXXXXXX======",
		"user---tag@example.com----hyphen-email-password",
		"password-empty-field@example.org----plain-password--------GEZDGNBVGY3TQOJQ",
		"space-group@example.org  grouped-password  JBSW  Y3DP  EHPK  3PXP",
		"base32-password@example.org----ABCDEFGHIJKLMNOP----" + mail,
		"biers.ellipse.case@icloud.com----aBCD2345eFGH6723----JBSWY3DPEHPK3PXPNB2W45DFOIZAQWER",
		"dot-password@example.com----r7.UjRUWVVS----JBSWY3DPEHPK3PXP",
	}
	entries, err := parseBatchEntries(strings.Join(lines, "\n"), "", nil, mailRequestConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 13 {
		t.Fatal(len(entries))
	}
	byEmail := map[string]createRequest{}
	for _, e := range entries {
		byEmail[e.Email] = e
	}
	for _, email := range []string{"password-mail-totp@example.com", "smart.user-name@sub.example.co.uk", "pipe-password@example.dev", "name+tag@sub-domain.example.cloud", "manual-totp@example.net", "password-empty-field@example.org", "space-group@example.org", "biers.ellipse.case@icloud.com", "dot-password@example.com"} {
		if secret, _ := byEmail[email].Input["totpSecret"].(string); secret == "" {
			t.Error("missing TOTP", email)
		}
	}
	if byEmail["base32-password@example.org"].Input["totpSecret"] != "" || byEmail["base32-password@example.org"].Input["password"] != "ABCDEFGHIJKLMNOP" {
		t.Fatal("base32 password misclassified")
	}
	if byEmail["pipe-password@example.dev"].Input["password"] != "part-one|part-two" {
		t.Fatal("password delimiter destroyed")
	}
	if byEmail["space-group@example.org"].Input["totpSecret"] != "JBSWY3DPEHPK3PXP" {
		t.Fatal("grouped secret not normalized")
	}
	if byEmail["manual-totp@example.net"].Input["password"] != "" {
		t.Fatal("manual TOTP inferred password")
	}
	for _, line := range []string{"two-emails@example.com----second@example.net----" + mail, "comma-url@example.com," + mail + "?a=1,b=2", "odd-hyphen@example.com-----JBSWY3DPEHPK3PXP", "mixed-delimiter@example.com----password|" + mail + "|JBSWY3DPEHPK3PXP", "broken input"} {
		if _, err := parseBatchEntries(line, "", nil, mailRequestConfig{}); err == nil {
			t.Error("ambiguous line accepted", line)
		}
	}
}
func TestLegacyPOSTMailboxBatchFormats(t *testing.T) {
	text := "post-body@example.com----eyJtYWlsYm94X2lkIjoiaWQtYSJ9\npost-password@example.com----test-password----opaque-account-body\n" + `{"mailbox_id":"id-c"}----post-unordered@example.com----test-password-c`
	entries, err := parseBatchEntries(text, "", nil, mailRequestConfig{Method: http.MethodPost, URL: "https://example.com/mail"})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatal(len(entries))
	}
	if entries[0].Input["password"] != "" || entries[0].Input["mailRequestBody"] != "eyJtYWlsYm94X2lkIjoiaWQtYSJ9" {
		t.Fatal("encoded mail body changed")
	}
	if entries[1].Input["password"] != "test-password" || entries[1].Input["mailRequestBody"] != "opaque-account-body" {
		t.Fatal("POST password omitted")
	}
	if entries[2].Input["password"] != "test-password-c" || entries[2].Input["mailRequestBody"] != `{"mailbox_id":"id-c"}` {
		t.Fatal("unordered POST body wrong")
	}
}
func TestBatchDuplicateUsesLastCredentialsAndRejectsBeforeCreation(t *testing.T) {
	entries, err := parseBatchEntries("a@example.com----old\nA@example.com----new", "", nil, mailRequestConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Input["password"] != "new" {
		t.Fatal(entries)
	}
	if _, err := parseBatchEntries("a@example.com----password\ninvalid", "", nil, mailRequestConfig{}); err == nil {
		t.Fatal("invalid line silently dropped")
	}
}

func TestBatchStructuredReimportReplacesCredentials(t *testing.T) {
	const email = "batch-reimport@example.test"
	const newProxy = "socks5h://new-proxy.example:1080"
	for _, tc := range []struct {
		name, line, password, totp, mailURL, mailBody string
		postURL                                       string
	}{
		{name: "email_api_without_password", line: email + "----https://new-mail.example/messages", mailURL: "https://new-mail.example/messages"},
		{name: "manual_totp_without_password", line: email + "--------GEZDGNBVGY3TQOJQ", totp: "GEZDGNBVGY3TQOJQ"},
		{name: "password_without_mail_or_totp", line: email + "----New-fixture-password9!", password: "New-fixture-password9!"},
		{name: "post_body_without_password", line: email + "----mailbox=synthetic-new-body", mailURL: "https://post-mail.example/messages", mailBody: "mailbox=synthetic-new-body", postURL: "https://post-mail.example/messages"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, m := newMonitorTestServer(t)
			vault := credentials.NewMemoryStore()
			m.SetCredentialStore(vault)
			j, err := m.Submit(jobs.Spec{Email: email, Input: map[string]any{
				"password": "Old-fixture-password9!", "totpSecret": "JBSWY3DPEHPK3PXP",
				"mailApiUrl": "https://old-mail.example/messages", "mailRequestBody": "old-synthetic-mail-body",
				"proxyUrl": "http://old-proxy.example:8080",
			}})
			if err != nil {
				t.Fatal(err)
			}
			if tc.postURL != "" {
				w := frontendRequest(s, http.MethodPost, "/api/mail-request-config", map[string]any{"config": map[string]any{"method": http.MethodPost, "url": tc.postURL}}, "test")
				if w.Code != http.StatusOK {
					t.Fatal("POST mail configuration failed", w.Code)
				}
			}
			w := frontendRequest(s, http.MethodPost, "/api/jobs/batch", map[string]any{"text": tc.line, "proxyUrl": newProxy}, "test")
			if w.Code != http.StatusCreated || len(m.List()) != 1 {
				t.Fatal("reimport did not update the existing account", w.Code)
			}
			got, ok := m.Get(j.ID)
			wantMode := "email_otp"
			if tc.password != "" {
				wantMode = "password"
			}
			if !ok || got.HasPassword != (tc.password != "") || got.LoginMode != wantMode || got.HasTOTPKey != (tc.totp != "") || got.MailAPIURL != tc.mailURL {
				t.Fatal("structured reimport retained an old credential flag or login mode")
			}
			input, _ := m.InputCopy(j.ID)
			for key, want := range map[string]string{"password": tc.password, "totpSecret": tc.totp, "mailApiUrl": tc.mailURL, "mailRequestBody": tc.mailBody, "proxyUrl": newProxy} {
				if input[key] != want {
					t.Errorf("structured reimport did not replace %s", key)
				}
			}
			stored, err := vault.Load(email)
			if err != nil || stored.Password != tc.password || stored.TOTPSecret != tc.totp || stored.MailAPIURL != tc.mailURL || stored.MailBody != tc.mailBody || stored.ProxyURL != newProxy {
				t.Fatal("protected credentials do not match the reimported account", err)
			}
			for _, secret := range []string{"Old-fixture-password9!", "JBSWY3DPEHPK3PXP", "old-synthetic-mail-body", tc.password, tc.totp, tc.mailBody} {
				if secret != "" && strings.Contains(w.Body.String(), secret) {
					t.Fatal("reimport response exposed private account input")
				}
			}
		})
	}
}

func TestBatchBareEmailReimportPreservesCredentials(t *testing.T) {
	s, m := newMonitorTestServer(t)
	vault := credentials.NewMemoryStore()
	m.SetCredentialStore(vault)
	values := map[string]any{
		"password": "Saved-fixture-password9!", "totpSecret": "JBSWY3DPEHPK3PXP",
		"mailApiUrl": "https://saved-mail.example/messages", "mailRequestBody": "saved-synthetic-body",
	}
	j, err := m.Submit(jobs.Spec{Email: "bare-email@example.test", Input: values})
	if err != nil {
		t.Fatal(err)
	}
	w := frontendRequest(s, http.MethodPost, "/api/jobs/batch", map[string]any{"text": strings.ToUpper(j.Email), "proxyUrl": ""}, "test")
	if w.Code != http.StatusCreated || len(m.List()) != 1 {
		t.Fatal("bare-email import did not retain the existing task", w.Code)
	}
	input, _ := m.InputCopy(j.ID)
	for key, want := range values {
		if input[key] != want {
			t.Errorf("bare-email import replaced %s", key)
		}
	}
	got, _ := m.Get(j.ID)
	stored, err := vault.Load(j.Email)
	if err != nil || !got.HasPassword || !got.HasTOTPKey || got.LoginMode != "password" || stored.Password != values["password"] || stored.TOTPSecret != values["totpSecret"] || stored.MailAPIURL != values["mailApiUrl"] || stored.MailBody != values["mailRequestBody"] {
		t.Fatal("bare-email import failed to preserve protected credentials", err)
	}
}
