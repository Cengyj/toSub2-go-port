package console

import (
	"net/http"
	"testing"
	"time"

	"tosub2-go-port/internal/credentials"
)

func TestBatchPasswordsRetainExactWhitespaceAcrossParsersAndVault(t *testing.T) {
	const email = "whitespace@example.test"
	for _, tc := range []struct {
		name, line, password, body string
		post                       bool
	}{
		{name: "last-field", line: email + "----  Synthetic-password9!  ", password: "  Synthetic-password9!  "},
		{name: "middle-field", line: email + "----  Synthetic-password9!  ----JBSWY3DPEHPK3PXP", password: "  Synthetic-password9!  "},
		{name: "expiry", line: email + "----  Synthetic-password9!  ----JBSWY3DPEHPK3PXP----2026-10-01 12:00", password: "  Synthetic-password9!  "},
		{name: "whitespace-only", line: email + "----  ", password: "  "},
		{name: "post-body-last", line: email + "----  Synthetic-password9!  ----  {\"mailbox\":\"synthetic\"}  ", password: "  Synthetic-password9!  ", body: "  {\"mailbox\":\"synthetic\"}  ", post: true},
		{name: "post-password-last", line: email + "----mailbox=synthetic----  Synthetic-password9!  ", password: "  Synthetic-password9!  ", body: "mailbox=synthetic", post: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := mailRequestConfig{}
			if tc.post {
				cfg.Method, cfg.URL = http.MethodPost, "https://mail.synthetic.invalid/messages"
			}
			entries, err := parseBatchEntriesAt(tc.line, "", nil, cfg, "Asia/Shanghai", time.Date(2026, 11, 23, 0, 0, 0, 0, time.UTC))
			if err != nil || len(entries) != 1 || entries[0].Input["password"] != tc.password {
				t.Fatal("batch parser changed password whitespace", err)
			}
			s, manager := newMonitorTestServer(t)
			vault := credentials.NewMemoryStore()
			manager.SetCredentialStore(vault)
			s.mail = cfg
			response := frontendRequest(s, http.MethodPost, "/api/jobs/batch", map[string]any{"text": tc.line, "timeZone": "Asia/Shanghai"}, "test")
			if response.Code != http.StatusCreated {
				t.Fatal(response.Code, response.Body.String())
			}
			saved, err := vault.Load(email)
			if err != nil || saved.Password != tc.password || tc.post && saved.MailBody != tc.body {
				t.Fatal("import changed opaque credentials before secure save", err)
			}
			if snapshot := manager.ActivitySnapshot(); snapshot.Queued != 0 || snapshot.Running != 0 {
				t.Fatal("credential-only import started authorization")
			}
		})
	}
}

func TestBatchBareEmailWhitespaceStillDeduplicatesAndPreservesCredentials(t *testing.T) {
	entries, err := parseBatchEntries("  Same@example.test  \nSAME@example.test\n  ", "", nil, mailRequestConfig{})
	if err != nil || len(entries) != 1 || !entries[0].preserveExistingCredentials || len(entries[0].Input) != 0 {
		t.Fatal("email normalization changed bare-email credential semantics", err)
	}
}
