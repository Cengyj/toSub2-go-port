package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"tosub2-go-port/internal/credentials"
)

// Exercise the same runner, job event sink and paged public records as a
// console reset. Count at the server, not from events or journal flags.
func TestResetSupportLogExplainsLostReceiptAndInvisibleFactor(t *testing.T) {
	clearCLISensitiveEnvironment(t)
	remote := &sessionAuditRemote{t: t, sessionValid: true, factor: "old-factor", failure: "activation-response-lost"}
	var reads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/backend-api/accounts/mfa_info" && reads.Add(1) > 1 {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"mfa_enabled_v2":true,"factors":{"totp":[{"id":"other-factor-canary","factor_type":"totp","is_recovery":false}]},"request_id":"extension-canary"}`)
			return
		}
		remote.serveHTTP(w, r)
	}))
	defer server.Close()
	cfg := sessionAuditConfig(t.TempDir(), server.URL)
	vault := &resetRunnerVault{MemoryStore: credentials.NewMemoryStore()}
	m, _, logger := sessionAuditManager(t, cfg, vault)
	sessionAuditPrepare(t, cfg, m)
	if err := sessionAuditExecute(t, cfg, m, logger, vault); err == nil {
		t.Fatal("mismatch completed")
	}
	s := remote.snapshot()
	if s.disableCalls != 1 || s.enrollCalls != 1 || s.activateCalls != 1 || reads.Load() != 2 {
		t.Fatal("unexpected requests", s.disableCalls, s.enrollCalls, s.activateCalls, reads.Load())
	}
	j, _ := m.Get(sessionAuditJobID)
	page, err := m.ReadLogPage(t.Context(), j, "")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(page.Entries)
	for _, mark := range []string{"other-factor-canary", "extension-canary", sessionAuditEmail, sessionAuditNew, sessionAuditCookie} {
		if strings.Contains(string(raw), mark) {
			t.Fatal("private marker in public log")
		}
	}
	if !strings.Contains(string(raw), `"reason_code":"EXPECTED_FACTOR_NOT_VISIBLE"`) || !strings.Contains(string(raw), `"activation_receipt":"unknown"`) {
		for _, e := range page.Entries {
			if e.Stage == "totp_reset_verify_enabled" || e.Stage == "failed" {
				t.Log(e.Stage, e.Message)
			}
		}
		t.Fatal("public records cannot identify the unknown POST and mismatched query")
	}
}

type jsonRoundtripVault struct {
	credentials.Store
	alter func(*credentials.Credentials)
}

func (v jsonRoundtripVault) Load(email string) (credentials.Credentials, error) {
	c, err := v.Store.Load(email)
	if err != nil {
		return c, err
	}
	b, err := json.Marshal(c)
	var got credentials.Credentials
	if err == nil {
		err = json.Unmarshal(b, &got)
	}
	if v.alter != nil {
		v.alter(&got)
	}
	return got, err
}
func TestBoundCredentialReadbackJSONEquivalence(t *testing.T) {
	value := credentials.Credentials{TOTPSecret: "seed-canary", PendingTOTPSecret: "pending-canary", TOTPFactorID: "factor-canary", PendingTOTPFactorID: "pending-id", TOTPResetOperationID: "operation-canary", MailHeaders: map[string]string{}}
	for _, tc := range []struct {
		name  string
		alter func(*credentials.Credentials)
		ok    bool
	}{
		{"empty-to-absent", nil, true}, {"current-seed", func(c *credentials.Credentials) { c.TOTPSecret += "x" }, false}, {"pending-seed", func(c *credentials.Credentials) { c.PendingTOTPSecret = "" }, false}, {"current-factor", func(c *credentials.Credentials) { c.TOTPFactorID = "" }, false}, {"pending-factor", func(c *credentials.Credentials) { c.PendingTOTPFactorID = "changed" }, false}, {"operation", func(c *credentials.Credentials) { c.TOTPResetOperationID = "new-op" }, false}, {"headers", func(c *credentials.Credentials) { c.MailHeaders = map[string]string{"X-Test": "changed"} }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := jsonRoundtripVault{credentials.NewMemoryStore(), tc.alter}
			if err := saveBoundTOTPCredentials(t.Context(), v, "fixture@example.test", value); (err == nil) != tc.ok {
				t.Fatal("wrong comparison", err)
			}
		})
	}
}
