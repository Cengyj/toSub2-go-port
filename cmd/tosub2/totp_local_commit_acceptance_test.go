package main

import (
	"bytes"
	"embed"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"tosub2-go-port/internal/console"
	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/privatestate"
	"tosub2-go-port/internal/resetlog"
)

// Count local writes only during explicit completion. For readback faults,
// the save actually succeeds; a later completion must not rewrite that key.
type completionProbeVault struct {
	*resetRunnerVault
	tracking, breakRead atomic.Bool
	writes              atomic.Int32
	fault               string
}

func (v *completionProbeVault) Save(email string, c credentials.Credentials) error {
	if v.tracking.Load() {
		v.writes.Add(1)
		switch v.fault {
		case "write":
			return errors.New("private-provider-canary")
		case "readback":
			v.breakRead.Store(true)
		}
	}
	return v.resetRunnerVault.Save(email, c)
}

func (v *completionProbeVault) Load(email string) (credentials.Credentials, error) {
	if v.breakRead.Swap(false) {
		return credentials.Credentials{}, errors.New("private-provider-canary")
	}
	return v.resetRunnerVault.Load(email)
}

func TestResetConsoleCompletesLocalBindings(t *testing.T) {
	clearCLISensitiveEnvironment(t)
	for _, tc := range []struct{ name, fault, cause string }{
		{"checkpoint-only", "", ""}, {"legacy-binding", "", ""}, {"already-bound", "", ""},
		{"legacy-binding-write", "write", "STORAGE_WRITE_FAILED"}, {"legacy-binding-read", "readback", "STORAGE_READBACK_FAILED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			remote := &sessionAuditRemote{t: t, sessionValid: true, factor: "old-factor"}
			server := httptest.NewServer(http.HandlerFunc(remote.serveHTTP))
			defer server.Close()
			cfg := sessionAuditConfig(t.TempDir(), server.URL)
			cfg.Port, cfg.StartupToken = 80, "fixture-console-only"
			v := &completionProbeVault{resetRunnerVault: &resetRunnerVault{MemoryStore: credentials.NewMemoryStore()}, fault: tc.fault}
			v.failCommit.Store(true)
			m, _, logger := sessionAuditManager(t, cfg, v)
			api := console.NewServer(cfg, m, embed.FS{}, logger, v)
			defer api.Close()
			sessionAuditImport(t, cfg, m)
			if w := resetConsoleRequest(api, cfg.StartupToken, "POST", "setup-2fa", `{"resetTotp":true,"logoutAllDevicesAfterTotp":true}`); w.Code != 200 {
				t.Fatal("reset not admitted", w.Code)
			}
			if err := sessionAuditExecute(t, cfg, m, logger, v); err == nil {
				t.Fatal("initial vault fault did not leave completion pending")
			}
			baseline := remote.snapshot()
			first := lastResetDetail(t, m)
			if baseline.infoReads != 1 || baseline.disableCalls != 1 || baseline.enrollCalls != 1 || baseline.activateCalls != 1 || first.Activation != "accepted" || first.Logout != "not_performed" {
				t.Fatal("fixture did not reach the receipt-only local commit boundary")
			}
			// Model an older protected record. This is fixture preparation, not
			// a recovery step: the only product action below is the console POST.
			path := filepath.Join(cfg.OutputRoot, sessionAuditJobID, "totp-setup-result.json")
			current, err := v.Load(sessionAuditEmail)
			if err != nil || !current.TOTPResetPending || current.PendingTOTPSecret != sessionAuditNew {
				t.Fatal("missing protected pending binding", err)
			}
			const private = "synthetic-session-projection-canary"
			if tc.name == "checkpoint-only" {
				out := sessionAuditJournal(t, cfg)
				out["recovery_checkpoint"] = map[string]any{"session_ref": private}
				if err := privatestate.WriteJSON(t.Context(), path, "totp-reset", sessionAuditEmail, current.TOTPResetOperationID, out); err != nil {
					t.Fatal(err)
				}
			} else {
				current.TOTPSecret, current.TOTPFactorID = current.PendingTOTPSecret, ""
				if tc.name == "already-bound" {
					current.TOTPFactorID = current.PendingTOTPFactorID
				}
				current.TOTPResetPending = false
				current.PendingTOTPSecret, current.PendingTOTPURI, current.PendingTOTPFactorID = "", "", ""
				if err := v.Save(sessionAuditEmail, current); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			v.tracking.Store(true)
			if w := resetConsoleRequest(api, cfg.StartupToken, "POST", "complete-2fa", `{}`); w.Code != 200 {
				t.Fatal("local completion not admitted", w.Code)
			}
			err = sessionAuditWait(t, m)
			d := lastResetDetail(t, m)
			success := tc.cause == ""
			done, _ := m.Get(sessionAuditJobID)
			if (err == nil) != success || done.TOTPResetCommitted != success || d.Cause != tc.cause {
				t.Errorf("local completion: committed=%t cause=%q; want committed=%t cause=%q", done.TOTPResetCommitted, d.Cause, success, tc.cause)
			}
			if d.OpRef != first.OpRef || d.Seq <= first.Seq || d.Activation != "accepted" || d.Writes == nil || *d.Writes != (resetlog.Writes{}) || remote.snapshot() != baseline {
				t.Fatal("completion lost the original receipt or contacted upstream")
			}
			if tc.name == "checkpoint-only" || !success {
				if after, err := os.ReadFile(path); err != nil || !bytes.Equal(before, after) {
					t.Error("completion rewrote historical or uncommitted evidence")
				}
			}
			wantWrites := int32(1)
			if tc.name == "already-bound" {
				wantWrites = 0
			}
			if got := v.writes.Load(); got != wantWrites {
				t.Errorf("vault saves=%d, want %d", got, wantWrites)
			}
			if success {
				c, err := v.Load(sessionAuditEmail)
				if err != nil || c.TOTPSecret != sessionAuditNew || c.TOTPFactorID != "new-factor" || c.TOTPResetPending || c.TOTPResetOperationID != current.TOTPResetOperationID {
					t.Fatal("completed credential does not match the original activation")
				}
			}
			w := resetConsoleRequest(api, cfg.StartupToken, "GET", "logs", "")
			if w.Code != 200 || strings.Contains(w.Body.String(), private) || strings.Contains(w.Body.String(), "private-provider-canary") {
				t.Fatal("public log unavailable or contained a private marker")
			}
			exportResetPage(t, tc.name, w.Body.Bytes())
		})
	}
}
