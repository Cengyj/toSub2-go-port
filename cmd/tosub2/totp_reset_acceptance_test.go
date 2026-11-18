package main

import (
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"tosub2-go-port/internal/console"
	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/protocol"
	"tosub2-go-port/internal/resetlog"
)

var publicPages = flag.String("reset-public-pages", "", "write sanitized console responses to a fresh temporary fixture directory")

func resetConsoleRequest(api *console.Server, token, method, action, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "http://127.0.0.1/api/jobs/"+sessionAuditJobID+"/"+action, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Console-Token", token)
	w := httptest.NewRecorder()
	api.Handler().ServeHTTP(w, r)
	return w
}

// Export only the authenticated, sanitized public log page. Do not export a
// manager/job snapshot, the provider ledger, or any recovery files.
func exportResetPage(t *testing.T, name string, body []byte) {
	t.Helper()
	if t.Failed() || *publicPages == "" {
		return
	}
	for _, mark := range []string{sessionAuditEmail, sessionAuditOld, sessionAuditNew, sessionAuditCookie, "synthetic-password", "private-provider-canary", "wrong-readback-canary", "otpauth://"} {
		if strings.Contains(string(body), mark) {
			t.Fatal("private marker in public fixture")
		}
	}
	f, e := os.OpenFile(filepath.Join(*publicPages, name+".json"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		t.Fatal(e)
	}
	_, e = f.Write(body)
	closeErr := f.Close()
	if e != nil || closeErr != nil {
		t.Fatal("fixture write failed")
	}
}

// Only the upstream decides what to show; the fixture never initiates an
// account request. Read retained events through the authenticated console API.
func TestResetConsoleRequestLedger(t *testing.T) {
	clearCLISensitiveEnvironment(t)
	for _, tc := range []struct {
		name, factor, binding, reply, terminal string
		reads, remove, enroll, activate        int
		ok                                     bool
	}{
		{"empty", "", "", "", "RESET_SAVED_BY_RECEIPT", 1, 0, 1, 1, true},
		{"empty-logout", "", "", "", "RESET_SAVED_BY_RECEIPT", 1, 0, 1, 1, true},
		{"empty-logout-unconfirmed", "", "", "", "RESET_COMPLETE_LOGOUT_UNCONFIRMED", 1, 0, 1, 1, false},
		{"empty-lost-then-matched", "", "", "lost", "RESET_COMPLETE", 2, 0, 1, 1, true},
		{"empty-lost-then-other", "", "", "lost-other", "EXPECTED_FACTOR_NOT_VISIBLE", 2, 0, 1, 1, false},
		{"old", "old-factor", "old-factor", "", "RESET_SAVED_BY_RECEIPT", 1, 1, 1, 1, true},
		{"import-seed-only", "old-factor", "", "", "RESET_SAVED_BY_RECEIPT", 1, 1, 1, 1, true},
		{"initial-conflict", "old-factor", "old-factor", "first", "RESET_SAVED_BY_RECEIPT", 2, 1, 2, 1, true},
		{"formal-conflict", "old-factor", "old-factor", "both", "FORMAL_CONFLICT", 2, 1, 2, 0, false},
		{"ambiguous", "old-factor", "old-factor", "ambiguous", "ENROLLMENT_UNCONFIRMED", 1, 0, 1, 0, false},
		{"bare-409", "", "", "bare", "ENROLLMENT_UNCONFIRMED", 1, 0, 1, 0, false},
		{"changed", "other-canary", "old-factor", "first", "TARGET_NOT_AUTHORIZED", 2, 0, 1, 0, false},
		{"unbound", "old-factor", "", "first", "TARGET_NOT_AUTHORIZED", 2, 0, 1, 0, false},
		{"initial-multiple", "old-factor", "old-factor", "multiple", "TARGET_UNRESOLVED", 1, 0, 0, 0, false},
		{"conflict-multiple", "old-factor", "old-factor", "first-multiple", "TARGET_UNRESOLVED", 2, 0, 1, 0, false},
		{"conflict-then-disabled", "", "", "first", "RESET_SAVED_BY_RECEIPT", 2, 0, 2, 1, true},
		{"conflict-then-disabled-twice", "", "", "both", "FORMAL_CONFLICT", 2, 0, 2, 0, false},
		{"removed-conflict-then-disabled", "old-factor", "old-factor", "after-remove-once", "RESET_SAVED_BY_RECEIPT", 2, 1, 2, 1, true},
		{"removed-conflict-then-changed", "old-factor", "old-factor", "after-remove-changed", "REMOVAL_BUDGET_USED", 2, 1, 1, 0, false},
		{"conflict-read-wrong-account", "", "", "conflict-account", "IDENTITY_UNVERIFIED", 2, 0, 1, 0, false},
		{"activation-echo-wrong-account", "", "", "activation-account", "IDENTITY_UNVERIFIED", 1, 0, 1, 1, false},
		{"activation-mixed-signals-logout", "", "", "activation-mixed-signals", "EXPECTED_FACTOR_NOT_VISIBLE", 2, 0, 1, 1, false},
		{"initial-mismatch", "other-canary", "old-factor", "", "TARGET_NOT_AUTHORIZED", 1, 0, 0, 0, false},
		{"new-and-old", "old-factor", "old-factor", "both-visible", "RESET_SAVED_OTHER_VISIBLE", 2, 1, 1, 1, true},
		{"new-and-old-logout", "old-factor", "old-factor", "both-visible", "RESET_SAVED_OTHER_VISIBLE", 2, 1, 1, 1, true},
		{"new-and-old-logout-unconfirmed", "old-factor", "old-factor", "both-visible", "RESET_SAVED_OTHER_VISIBLE", 2, 1, 1, 1, false},
		{"new-and-other", "old-factor", "old-factor", "also-other", "RESET_SAVED_OTHER_VISIBLE", 2, 1, 1, 1, true},
		{"multiple-logout", "old-factor", "old-factor", "multiple", "TARGET_UNRESOLVED", 1, 0, 0, 0, false},
		{"new-not-visible", "old-factor", "old-factor", "other-visible", "EXPECTED_FACTOR_NOT_VISIBLE", 2, 1, 1, 1, false},
		{"manual-proof", "old-factor", "", "manual", "RESET_SAVED_BY_RECEIPT", 2, 1, 2, 1, true},
		{"verified-old-logout", "old-factor", "", "verified-old", "RESET_SAVED_BY_RECEIPT", 1, 1, 1, 1, true},
		{"lost-then-matched", "old-factor", "old-factor", "lost", "RESET_COMPLETE", 2, 1, 1, 1, true},
		{"lost-then-other", "old-factor", "old-factor", "lost-other", "EXPECTED_FACTOR_NOT_VISIBLE", 2, 1, 1, 1, false},
		{"stale", "old-factor", "old-factor", "stale", "HTTP_REFUSED", 1, 1, 0, 0, false},
		{"partial", "old-factor", "old-factor", "partial", "FIELD_INVALID", 1, 1, 1, 0, false},
		{"wrong-account", "old-factor", "old-factor", "wrong-account", "IDENTITY_UNVERIFIED", 1, 0, 0, 0, false},
		{"logout-accepted", "old-factor", "old-factor", "logout", "RESET_SAVED_BY_RECEIPT", 1, 1, 1, 1, true},
		{"optional-state", "", "", "optional", "RESET_SAVED_BY_RECEIPT", 4, 0, 1, 1, true},
		{"formal-after-remove", "old-factor", "old-factor", "formal", "FORMAL_CONFLICT", 2, 1, 2, 0, false},
		{"session-200-protected-401", "old-factor", "old-factor", "revoked", "SESSION_REVOKED", 1, 0, 0, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wantLogout := strings.Contains(tc.name, "logout")
			logoutUnknown := strings.HasSuffix(tc.name, "-logout-unconfirmed")
			keySaved := tc.ok || logoutUnknown
			remote := &sessionAuditRemote{t: t, sessionValid: true, factor: tc.factor}
			lostReceipt := strings.HasPrefix(tc.reply, "lost") || strings.Contains(tc.reply, "visible") || tc.reply == "also-other"
			if lostReceipt {
				remote.failure = "activation-response-lost"
			} else if tc.reply == "activation-mixed-signals" {
				remote.failure = tc.reply
			}
			var mu sync.Mutex
			var m *jobs.Manager
			var ledger []string
			reads, enrolls := 0, 0
			firstConflict := tc.reply == "first" || tc.reply == "manual" || tc.reply == "first-multiple" || tc.reply == "conflict-account"
			loginChallenge := tc.reply == "manual" || tc.reply == "verified-old"
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				ledger = append(ledger, r.Method+" "+r.URL.Path)
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/api/accounts/password/verify":
					if loginChallenge {
						fmt.Fprintf(w, `{"continue_url":"http://%s/mfa-challenge/old-factor","page":{"type":"mfa_challenge"},"oai-client-auth-session":{"mfa_challenge_factors":[{"id":"old-factor","factor_type":"totp"}]}}`, r.Host)
						return
					}
				case "/api/accounts/mfa/issue_challenge", "/api/accounts/mfa/verify":
					if loginChallenge {
						var data map[string]any
						if json.NewDecoder(r.Body).Decode(&data) != nil || data["id"] != "old-factor" {
							t.Error("login proof used other target")
							w.WriteHeader(403)
							return
						}
						if r.URL.Path == "/api/accounts/mfa/verify" {
							if data["code"] != "825764" {
								t.Error("login proof was not the supplied fixture code")
								w.WriteHeader(403)
								return
							}
							fmt.Fprintf(w, `{"continue_url":"http://%s/api/auth/callback/openai?code=fixture-once"}`, r.Host)
						} else {
							fmt.Fprint(w, `{}`)
						}
						return
					}
				case "/backend-api/accounts/mfa_info":
					reads++
					if tc.reply == "optional" && enrolls == 0 {
						fmt.Fprint(w, `{}`)
						return
					}
					if tc.reply == "revoked" {
						w.WriteHeader(http.StatusUnauthorized)
						fmt.Fprint(w, `{"detail":{"code":"token_revoked"}}`)
						return
					}
					if tc.reply == "wrong-account" || tc.reply == "conflict-account" && enrolls > 0 {
						fmt.Fprint(w, `{"account":{"id":"wrong-account-canary"},"mfa_enabled_v2":false}`)
						return
					}
					if enrolls == 0 && (firstConflict || tc.reply == "both" || tc.reply == "ambiguous") {
						fmt.Fprint(w, `{"mfa_enabled_v2":false}`)
						return
					}
					if tc.reply == "multiple" || tc.reply == "first-multiple" {
						fmt.Fprint(w, `{"mfa_enabled_v2":true,"factors":{"totp":[{"id":"old-factor","factor_type":"totp","is_recovery":false},{"id":"other-canary","factor_type":"totp","is_recovery":false}]}}`)
						return
					}
					if reads > 1 && tc.reply == "both-visible" {
						fmt.Fprint(w, `{"mfa_enabled_v2":true,"factors":{"totp":[{"id":"old-factor","factor_type":"totp","is_recovery":false},{"id":"new-factor","factor_type":"totp","is_recovery":false}]}}`)
						return
					}
					if reads > 1 && tc.reply == "also-other" {
						fmt.Fprint(w, `{"mfa_enabled_v2":true,"factors":{"totp":[{"id":"other-canary","factor_type":"totp","is_recovery":false},{"id":"new-factor","factor_type":"totp","is_recovery":false}]}}`)
						return
					}
					if reads > 1 && (tc.reply == "other-visible" || tc.reply == "lost-other" || tc.reply == "after-remove-changed" || tc.reply == "activation-mixed-signals") {
						fmt.Fprint(w, `{"mfa_enabled_v2":true,"factors":{"totp":[{"id":"other-canary","factor_type":"totp","is_recovery":false}]}}`)
						return
					}
				case "/backend-api/accounts/mfa/enroll":
					enrolls++
					if tc.reply == "bare" {
						w.WriteHeader(409)
						fmt.Fprint(w, `{}`)
						return
					}
					if tc.reply == "partial" {
						fmt.Fprintf(w, `{"secret":%q,"factor":{"id":"new-factor","factor_type":"totp","is_recovery":false}}`, sessionAuditNew)
						return
					}
					if tc.reply == "ambiguous" {
						w.WriteHeader(409)
						fmt.Fprint(w, `{"message":"already handled fixture-canary"}`)
						return
					}
					if tc.reply == "both" || tc.reply == "formal" || (firstConflict || strings.HasPrefix(tc.reply, "after-remove-")) && enrolls == 1 {
						w.WriteHeader(409)
						fmt.Fprint(w, `{"request_id":"extension-canary","error":{"code":"totp_already_enrolled"}}`)
						return
					}
				case "/backend-api/accounts/mfa/user/disable_in_house":
					var body map[string]any
					if json.NewDecoder(r.Body).Decode(&body) != nil || body["factor_id"] != tc.factor {
						t.Error("removal did not address the fixed target")
					}
					if tc.reply == "stale" {
						w.WriteHeader(http.StatusBadRequest)
						fmt.Fprint(w, `{"message":"Invalid request"}`)
						return
					}
				case "/backend-api/accounts/mfa/user/activate_enrollment":
					if tc.reply == "activation-account" {
						fmt.Fprint(w, `{"success":true,"account_id":"wrong-account-canary"}`)
						return
					}
				case "/backend-api/accounts/logout_all":
					j, _ := m.Get(sessionAuditJobID)
					if !wantLogout || j == nil || !j.TOTPResetCommitted {
						t.Error("logout before final commit or without intent")
					}
					if logoutUnknown {
						w.WriteHeader(http.StatusServiceUnavailable)
					}
					fmt.Fprint(w, `null`)
					return
				}
				remote.serveHTTP(w, r)
			}))
			defer server.Close()
			cfg := sessionAuditConfig(t.TempDir(), server.URL)
			cfg.Port = 80
			cfg.StartupToken = "fixture-console-only"
			vault := &resetRunnerVault{MemoryStore: credentials.NewMemoryStore()}
			m, _, logger := sessionAuditManager(t, cfg, vault)
			api := console.NewServer(cfg, m, embed.FS{}, logger, vault)
			defer api.Close()
			if loginChallenge {
				j, e := m.Submit(jobs.Spec{ID: sessionAuditJobID, Email: sessionAuditEmail, ImportOnly: true, Input: map[string]any{"password": "synthetic-password"}})
				if e != nil {
					t.Fatal(e)
				}
				// Prior account metadata makes the explicit reset selectable in the UI;
				// no old ID or seed is provided by the fixture.
				if e = m.UpdateTOTPSetup(j.ID, "", "", "", true); e != nil {
					t.Fatal(e)
				}
				events, unsub := m.Subscribe(64)
				defer unsub()
				go func() {
					for e := range events {
						if e.Stage == "totp" && e.Status == jobs.StatusWaitingInput {
							m.Input(j.ID, map[string]any{"totp": "825764"})
							return
						}
					}
				}()
			} else {
				sessionAuditImport(t, cfg, m)
			}
			if tc.binding != "" {
				c, err := vault.Load(sessionAuditEmail)
				if err != nil {
					t.Fatal(err)
				}
				c.TOTPFactorID = tc.binding
				if err := vault.Save(sessionAuditEmail, c); err != nil {
					t.Fatal(err)
				}
			}
			intent := fmt.Sprintf(`{"resetTotp":true,"logoutAllDevicesAfterTotp":%t}`, wantLogout)
			if w := resetConsoleRequest(api, cfg.StartupToken, "POST", "setup-2fa", intent); w.Code != http.StatusOK {
				t.Fatalf("reset action: %d", w.Code)
			}
			err := sessionAuditExecute(t, cfg, m, logger, vault)
			w := resetConsoleRequest(api, cfg.StartupToken, "GET", "logs", "")
			if w.Code != 200 {
				t.Fatalf("logs API: %d", w.Code)
			}
			raw := w.Body.String()
			var page jobs.LogPage
			if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
				t.Fatal(err)
			}
			counts := map[string]int{}
			postActivationReads := 0
			mu.Lock()
			for _, path := range ledger {
				if path == "GET /backend-api/accounts/mfa_info" && counts["POST /backend-api/accounts/mfa/user/activate_enrollment"] > 0 {
					postActivationReads++
				}
				counts[path]++
			}
			mu.Unlock()
			got := [4]int{counts["GET /backend-api/accounts/mfa_info"], counts["POST /backend-api/accounts/mfa/user/disable_in_house"], counts["POST /backend-api/accounts/mfa/enroll"], counts["POST /backend-api/accounts/mfa/user/activate_enrollment"]}
			logoutCalls := counts["POST /backend-api/accounts/logout_all"]
			t.Logf("received status/remove/enroll/activate=%v, post-activation reads=%d, logout=%d; workflow completed=%v", got, postActivationReads, logoutCalls, err == nil)
			if got != [4]int{tc.reads, tc.remove, tc.enroll, tc.activate} || (err == nil) != tc.ok {
				t.Errorf("unexpected requests/outcome: %v", err)
			}
			if logoutCalls > 1 || (logoutCalls == 1) != (wantLogout && keySaved) || counts["POST /api/auth/signin/openai"] != 1 {
				t.Error("unexpected logout or second login", logoutCalls)
			}
			if tc.enroll == 2 {
				record, e := protocol.ReadTOTPResetIdentity(filepath.Join(cfg.OutputRoot, sessionAuditJobID, "totp-setup-result.json"), sessionAuditEmail)
				if e != nil {
					t.Fatal("two-attempt journal unreadable", e)
				}
				first, second := "initial_enroll", "enroll"
				if record["enrollment_source"] == "retry_enroll" {
					first, second = "enroll", "retry_enroll"
				}
				for i, step := range []string{first, second} {
					r, _ := record[step+"_receipt"].(map[string]any)
					status := float64(409)
					if i == 1 && keySaved {
						status = 200
					}
					if record[step+"_attempt_id"] == nil || r["attempt_id"] != record[step+"_attempt_id"] || r["operation_id"] != record["operation_id"] || r["step"] != step || r["body_complete"] != true || r["http_status"] != status {
						t.Fatal("missing or overwritten enrollment receipt", step)
					}
				}
				if record[first+"_attempt_id"] == record[second+"_attempt_id"] {
					t.Fatal("second enrollment reused the first attempt")
				}
			}
			c, loadErr := vault.Load(sessionAuditEmail)
			if loadErr != nil || keySaved && (c.TOTPSecret != sessionAuditNew || c.TOTPFactorID != "new-factor" || c.TOTPResetPending) {
				t.Error("active credential did not match completion")
			}
			// This is the explicit private view used by the account information dialog.
			// Never print or export its response as part of the diagnostic fixtures.
			if keySaved {
				view := resetConsoleRequest(api, cfg.StartupToken, "GET", "account-info", "")
				var info struct{ TOTPSecret, TOTPStatus string }
				if view.Code != http.StatusOK || view.Header().Get("Cache-Control") != "no-store" || json.Unmarshal(view.Body.Bytes(), &info) != nil || info.TOTPSecret != sessionAuditNew || info.TOTPStatus != "saved" {
					t.Error("saved new key unavailable through the private account view")
				}
			}
			var terminal *resetlog.Detail
			var terminalText, terminalLevel string
			for _, entry := range page.Entries {
				if d := entry.Diagnostic; d != nil && d.Terminal {
					if terminal != nil {
						t.Error("multiple terminal records")
					}
					terminal = d
					terminalText = entry.Message
					terminalLevel = entry.Level
				}
			}
			if terminal == nil || terminal.Reason != tc.terminal {
				t.Fatalf("wrong public terminal record: %+v", terminal)
			}
			receiptOnly := tc.terminal == "RESET_SAVED_BY_RECEIPT" || tc.name == "empty-logout-unconfirmed"
			if receiptOnly && (postActivationReads != 0 || terminal.Activation != "accepted" || terminal.Match != "skipped" || terminal.Visible != "" || terminal.VisibleCount != nil || terminal.Pending != "verified" || terminal.Commit != "committed" || !strings.Contains(terminalText, "网站已确认激活，新密钥已保存；未另查验证器列表") || strings.Contains(terminalText, "尚未确认。") || strings.Contains(raw, `"stage":"verify_factor"`)) {
				t.Error("receipt-only completion claimed a list read, lost the saved key or invented an unknown result")
			}
			switch terminal.Reason {
			case "TARGET_UNRESOLVED", "TARGET_NOT_AUTHORIZED":
				if terminal.Removal != "not_performed" || terminal.Next != "review_binding" || terminal.Commit == "committed" || err == nil {
					t.Fatal("ambiguous read was treated as removal or completion")
				}
			case "IDENTITY_UNVERIFIED":
				if terminal.Next != "review_identity" || terminal.Removal != "not_performed" {
					t.Error("wrong-account response lost the identity boundary")
				}
			case "RESET_SAVED_OTHER_VISIBLE":
				if terminalLevel != "warning" || terminal.Next != "review_binding" || terminal.Match != "matched" || terminal.Commit != "committed" || terminal.Removal != "accepted" || terminal.VisibleCount == nil || *terminal.VisibleCount != 2 || terminal.OldVisible != (tc.reply == "both-visible") || !strings.Contains(terminalText, "新密钥已核对并保存") {
					t.Error("public log conflated saved key, removal receipt and remaining authenticators")
				}
			case "RESET_COMPLETE_LOGOUT_UNCONFIRMED":
				if terminalLevel != "warning" || terminal.Next != "review_logout" || terminal.Commit != "committed" {
					t.Error("sole unconfirmed step is logout, not the saved key")
				}
			}
			if lostReceipt && (terminal.Writes.Activation != (resetlog.Count{Possible: 1}) || terminal.Activation != "unknown" || (!logoutUnknown && terminal.WriteIssue != "NO_RESPONSE") || terminal.WriteAttempt == "" || !strings.Contains(raw, `"reason_code":"NO_RESPONSE"`)) {
				t.Error("a later GET replaced evidence of the uncertain POST")
			}
			if tc.reply == "activation-mixed-signals" && (terminal.Writes.Activation != (resetlog.Count{Received: 1}) || terminal.Activation != "unknown" || terminal.Pending != "verified" || logoutCalls != 0 || terminal.WriteIssue != "CONTRADICTORY_RECEIPT" || terminal.WriteAttempt == "") {
				t.Error("a complete but contradictory receipt was treated as lost or accepted")
			}
			if tc.reply == "partial" && (terminal.Field != "session_id" || terminal.ActualType != "absent" || terminal.Pending == "verified" || terminal.Material != "partial") {
				t.Error("missing enrollment field not distinguished")
			}
			if tc.reply == "optional" && (!strings.Contains(raw, `"reason_code":"OPTIONAL_STATE_MISSING"`) || strings.Contains(raw, `"reason_code":"NO_ORDINARY_TOTP"`)) {
				t.Error("missing state was reported as explicitly disabled")
			}
			if wantLogout && tc.ok && (terminal.Logout != "accepted" || terminal.Commit != "committed" || !strings.Contains(terminalText, "退出请求已接受") || !strings.Contains(terminalText, "未核实")) {
				t.Error("accepted logout overwrote reset or was not recorded")
			}
			if logoutUnknown && (terminal.Logout != "unknown" || terminal.Commit != "committed" || !strings.Contains(terminalText, "未确认退出结果") || terminal.WriteIssue != "SERVER_ERROR") {
				t.Error("uncertain logout overwrote or hid the committed key or factor warning")
			}
			if logoutUnknown && (err == nil || !strings.Contains(err.Error(), "退出所有设备结果待核实") || strings.Contains(raw, "退出所有设备未完成")) {
				t.Error("workflow's final error claimed a definite logout result despite the unknown receipt")
			}
			if tc.reply == "stale" && terminal.Removal != "refused" {
				t.Error("generic 400 reported as removal")
			}
			if tc.reply == "lost-other" || tc.reply == "other-visible" || tc.reply == "activation-mixed-signals" {
				if !c.TOTPResetPending || c.PendingTOTPSecret != sessionAuditNew || c.PendingTOTPFactorID != "new-factor" {
					t.Error("unmatched operation material not retained in its pending binding")
				}
				view := resetConsoleRequest(api, cfg.StartupToken, "GET", "account-info", "")
				if view.Code != http.StatusConflict || strings.Contains(view.Body.String(), sessionAuditNew) || strings.Contains(view.Body.String(), sessionAuditOld) {
					t.Error("unmatched seed exposed as an active credential")
				}
			}
			if err != nil && strings.Contains(err.Error(), "canary") {
				t.Error("marker in public error chain")
			}
			for _, s := range []string{"fixture-canary", "extension-canary", "other-canary", "wrong-account-canary", sessionAuditEmail, sessionAuditNew, sessionAuditOld, sessionAuditCookie, "synthetic-password", `825764`, "synthetic-enrollment-session", "otpauth://", "fixture-once"} {
				if strings.Contains(raw, s) {
					t.Fatal("private marker in logs API")
				}
			}
			exportResetPage(t, tc.name, w.Body.Bytes())
		})
	}
}
