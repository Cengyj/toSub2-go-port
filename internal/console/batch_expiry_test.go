package console

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestBatchImportExpiryKeepsCredentialFieldsAndLegacyFormats(t *testing.T) {
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name, line, password, totp, mail, body, want string
		post                                         bool
	}{
		{"short", "expiry@example.test----fixture-password----JBSWY3DPEHPK3PXP----16.30", "fixture-password", "JBSWY3DPEHPK3PXP", "", "", "2026-10-18T08:30:00Z", false},
		{"month-day", "expiry@example.test----fixture-password----JBSWY3DPEHPK3PXP----7-18 08:00", "fixture-password", "JBSWY3DPEHPK3PXP", "", "", "2026-07-18T00:00:00Z", false},
		{"month-day-single-hour", "expiry@example.test----fixture-password----JBSWY3DPEHPK3PXP----7-18 8:00", "fixture-password", "JBSWY3DPEHPK3PXP", "", "", "2026-07-18T00:00:00Z", false},
		{"full-date", "expiry@example.test----fixture-password----JBSWY3DPEHPK3PXP----2026-10-20 16:30", "fixture-password", "JBSWY3DPEHPK3PXP", "", "", "2026-10-20T08:30:00Z", false},
		{"no-password", "expiry@example.test--------JBSWY3DPEHPK3PXP----16:30", "", "JBSWY3DPEHPK3PXP", "", "", "2026-10-18T08:30:00Z", false},
		{"empty-totp", "expiry@example.test----fixture-password--------16.30", "fixture-password", "", "", "", "2026-10-18T08:30:00Z", false},
		{"empty-totp-month-day", "expiry@example.test----fixture-password--------7-18 8:00", "fixture-password", "", "", "", "2026-07-18T00:00:00Z", false},
		{"mail-url", "expiry@example.test----fixture-password----https://mail.example.test/messages----JBSWY3DPEHPK3PXP----16.30", "fixture-password", "JBSWY3DPEHPK3PXP", "https://mail.example.test/messages", "", "2026-10-18T08:30:00Z", false},
		{"legacy-totp-url", "expiry@example.test----fixture-password----JBSWY3DPEHPK3PXP----https://mail.example.test/messages", "fixture-password", "JBSWY3DPEHPK3PXP", "https://mail.example.test/messages", "", "", false},
		{"legacy-password", "expiry@example.test----16.30", "16.30", "", "", "", "", false},
		{"base32-password", "expiry@example.test----ABCDEFGHIJKLMNOP----JBSWY3DPEHPK3PXP----16.30", "ABCDEFGHIJKLMNOP", "JBSWY3DPEHPK3PXP", "", "", "2026-10-18T08:30:00Z", false},
		{"post-mail", "expiry@example.test----fixture-password----fixture-body----JBSWY3DPEHPK3PXP----16.30", "fixture-password", "JBSWY3DPEHPK3PXP", "https://mail.example.test/post", "fixture-body", "2026-10-18T08:30:00Z", true},
		{"post-mail-month-day", "expiry@example.test----fixture-password----fixture-body----JBSWY3DPEHPK3PXP----7-18 8:00", "fixture-password", "JBSWY3DPEHPK3PXP", "https://mail.example.test/post", "fixture-body", "2026-07-18T00:00:00Z", true},
		{"legacy-post-body", "expiry@example.test----fixture-password----JBSWY3DPEHPK3PXP----16.30", "fixture-password", "JBSWY3DPEHPK3PXP", "https://mail.example.test/post", "16.30", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := mailRequestConfig{}
			if tc.post {
				cfg.Method, cfg.URL = http.MethodPost, "https://mail.example.test/post"
			}
			entries, err := parseBatchEntriesAt(tc.line, "", nil, cfg, "Asia/Shanghai", now)
			if err != nil || len(entries) != 1 {
				t.Fatalf("parse failed: %v", err)
			}
			entry := entries[0]
			for key, want := range map[string]string{"password": tc.password, "totpSecret": tc.totp, "mailApiUrl": tc.mail, "mailRequestBody": tc.body, "manualExpiresAt": tc.want} {
				got, _ := entry.Input[key].(string)
				if got != want {
					t.Errorf("incorrect %s", key)
				}
			}
		})
	}
}

func TestBatchExpiryInvalidRowRejectsBeforeImport(t *testing.T) {
	s, m := newMonitorTestServer(t)
	for _, value := range []string{"25.30", "16.99", "2026-02-30 16:30", "2-30 8:00", "7-18 24:00", "7-18"} {
		w := frontendRequest(s, http.MethodPost, "/api/jobs/batch", map[string]any{"text": "valid@example.test\ninvalid@example.test----private-canary----JBSWY3DPEHPK3PXP----" + value, "timeZone": "Asia/Shanghai"}, "test")
		if w.Code != http.StatusBadRequest || len(m.List()) != 0 || strings.Contains(w.Body.String(), "private-canary") {
			t.Fatal("invalid expiration partially imported or exposed credentials", w.Code)
		}
	}
}

func TestBatchExpiryInvalidFactorCannotBecomePassword(t *testing.T) {
	s, m := newMonitorTestServer(t)
	for _, value := range []string{"16.30", "2030-12-31", "2030-12-31 16:30", "7-18 08:00", "7-18 8:00"} {
		w := frontendRequest(s, http.MethodPost, "/api/jobs/batch", map[string]any{"text": "valid@example.test\ninvalid@example.test----private-canary----invalid-key----" + value, "timeZone": "Asia/Shanghai"}, "test")
		if w.Code != http.StatusBadRequest || len(m.List()) != 0 || strings.Contains(w.Body.String(), "private-canary") {
			t.Fatal("invalid factor was combined into a password or batch partially imported", w.Code)
		}
	}
}
