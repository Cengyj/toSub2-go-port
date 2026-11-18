package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"tosub2-go-port/internal/console"
	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/privatestate"
	"tosub2-go-port/internal/protocol"
	"tosub2-go-port/internal/resetlog"
)

type checkpointFaultVault struct {
	*resetRunnerVault
	fault          string
	root           string
	faultApplied   atomic.Bool
	activeSaves    atomic.Int32
	metadataErrors chan error
}

func (v *checkpointFaultVault) Save(email string, c credentials.Credentials) error {
	if !c.TOTPResetPending && c.TOTPSecret == sessionAuditNew {
		v.activeSaves.Add(1)
	}
	if v.fault == "pending-write" && c.PendingTOTPSecret != "" {
		return errors.New("private-provider-canary")
	}
	return v.resetRunnerVault.Save(email, c)
}
func (v *checkpointFaultVault) Load(email string) (credentials.Credentials, error) {
	c, e := v.resetRunnerVault.Load(email)
	if v.fault == "pending-read" && c.PendingTOTPSecret != "" {
		c.PendingTOTPFactorID = "wrong-readback-canary"
	}
	if e == nil && !c.TOTPResetPending && c.TOTPSecret == sessionAuditNew {
		if v.fault == "active-read" && v.faultApplied.CompareAndSwap(false, true) {
			return credentials.Credentials{}, errors.New("private-provider-canary")
		}
		if v.fault == "final-metadata" {
			journal, err := protocol.ReadTOTPResetCompletion(filepath.Join(v.root, sessionAuditJobID, "totp-setup-result.json"), email)
			if err == nil && journal["credential_persisted"] == true && v.faultApplied.CompareAndSwap(false, true) {
				// The runner already saved/read the active credential and journal.
				// Block only the following metadata replacement; never commit or
				// recover any product state from this fault injector.
				path := filepath.Join(v.root, "job-meta.json")
				err = os.Rename(path, path+".fixture-backup")
				if err == nil {
					err = os.Mkdir(path, 0700)
				}
				v.metadataErrors <- err
			}
		}
	}
	return c, e
}

func lastResetDetail(t *testing.T, m *jobs.Manager) *resetlog.Detail {
	t.Helper()
	j, _ := m.Get(sessionAuditJobID)
	p, e := m.ReadLogPage(t.Context(), j, "")
	if e != nil {
		t.Fatal(e)
	}
	for i := len(p.Entries) - 1; i >= 0; i-- {
		if d := p.Entries[i].Diagnostic; d != nil && d.Terminal {
			return d
		}
	}
	t.Fatal("no terminal diagnostic")
	return nil
}

func TestResetStorageCheckpointsAndOfflineContinuation(t *testing.T) {
	clearCLISensitiveEnvironment(t)
	for _, fault := range []string{"preflight", "pending-write", "pending-read", "local-commit", "local-commit-with-other", "active-read", "final-metadata"} {
		t.Run(fault, func(t *testing.T) {
			remote := &sessionAuditRemote{t: t, sessionValid: true, factor: "old-factor"}
			if fault == "local-commit-with-other" {
				remote.failure = "activation-response-lost" // only reconciliation observes other factors
			}
			writeFault := fault == "local-commit" || fault == "local-commit-with-other"
			localFault := writeFault || fault == "active-read" || fault == "final-metadata"
			var requests, reads, logouts atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.URL.Path == "/backend-api/accounts/logout_all" {
					logouts.Add(1)
				}
				if r.URL.Path == "/backend-api/accounts/mfa_info" && reads.Add(1) > 1 && fault == "local-commit-with-other" {
					w.Header().Set("Content-Type", "application/json")
					fmt.Fprint(w, `{"mfa_enabled_v2":true,"factors":{"totp":[{"id":"new-factor","factor_type":"totp","is_recovery":false},{"id":"other-canary","factor_type":"totp","is_recovery":false}]}}`)
					return
				}
				remote.serveHTTP(w, r)
			}))
			defer server.Close()
			cfg := sessionAuditConfig(t.TempDir(), server.URL)
			cfg.Port, cfg.StartupToken = 80, "fixture-console-only"
			v := &checkpointFaultVault{resetRunnerVault: &resetRunnerVault{MemoryStore: credentials.NewMemoryStore()}, fault: fault, root: cfg.OutputRoot, metadataErrors: make(chan error, 1)}
			v.failCommit.Store(writeFault)
			m, st, logger := sessionAuditManager(t, cfg, v)
			api := console.NewServer(cfg, m, embed.FS{}, logger, v)
			defer api.Close()
			sessionAuditImport(t, cfg, m)
			wantLogout := fault == "active-read" || fault == "final-metadata"
			intent := fmt.Sprintf(`{"resetTotp":true,"logoutAllDevicesAfterTotp":%t}`, wantLogout)
			if w := resetConsoleRequest(api, cfg.StartupToken, "POST", "setup-2fa", intent); w.Code != 200 {
				t.Fatal("not queued", w.Code)
			}
			m.SetRunner(makeJobRunnerWithCredentials(cfg, m, logger, v, fault != "preflight"))
			m.Start()
			if sessionAuditWait(t, m) == nil {
				t.Fatal("injected fault completed")
			}
			if fault == "final-metadata" {
				select {
				case err := <-v.metadataErrors:
					if err != nil {
						t.Fatal("metadata fault could not be installed")
					}
				default:
					t.Fatal("final metadata checkpoint was not reached")
				}
			}
			s := remote.snapshot()
			before := requests.Load()
			d := lastResetDetail(t, m)
			w := resetConsoleRequest(api, cfg.StartupToken, "GET", "logs", "")
			if w.Code != 200 {
				t.Fatal("missing public logs", w.Code)
			}
			exportResetPage(t, fault, w.Body.Bytes())
			t.Logf("upstream received status/remove/enroll/activate=%v; total network=%d", [4]int{int(reads.Load()), s.disableCalls, s.enrollCalls, s.activateCalls}, before)
			if fault == "preflight" {
				if s.requests != 0 || d.Reason != "STORAGE_BLOCKED" || d.LastTrusted != "none" || d.Writes == nil || *d.Writes != (resetlog.Writes{}) {
					t.Fatal("storage preflight permitted a request or claimed progress")
				}
				return
			}
			if s.disableCalls != 1 || s.enrollCalls != 1 || d.Removal != "accepted" {
				t.Fatal("removal evidence lost or repeated")
			}
			if !localFault {
				want := "STORAGE_WRITE_FAILED"
				if fault == "pending-read" {
					want = "STORAGE_READBACK_FAILED"
				}
				if s.activateCalls != 0 || d.Pending != "failed" || d.Reason != want || d.LastTrusted != "enrollment" {
					t.Fatal("pending vault failure not distinguished", d.Reason, d.Pending)
				}
				return
			}
			wantReceipt, wantMatch, wantLast, wantCause := "accepted", "skipped", "activation", "RECEIPT_LOCAL_COMMIT_REQUIRED"
			if fault == "local-commit-with-other" {
				wantReceipt, wantMatch, wantLast, wantCause = "unknown", "matched", "verify_factor", "LOCAL_COMMIT_REQUIRED"
			}
			wantStorage := "STORAGE_WRITE_FAILED"
			if fault == "active-read" {
				wantStorage = "STORAGE_READBACK_FAILED"
			} else if fault == "final-metadata" {
				wantStorage = "STORAGE_COMMIT_FAILED"
			}
			if s.activateCalls != 1 || d.Activation != wantReceipt || d.Match != wantMatch || d.LastTrusted != wantLast || d.Reason != wantCause || d.Cause != wantStorage {
				t.Fatal("local failure erased successful remote verification", d.Reason, d.Cause)
			}
			if logouts.Load() != 0 || wantLogout && d.Logout != "not_performed" {
				t.Fatal("local failure permitted logout or lost the unperformed logout state")
			}
			if wantLogout {
				for _, response := range []*httptest.ResponseRecorder{
					resetConsoleRequest(api, cfg.StartupToken, "GET", "account-info", ""), resetConsoleExport(api, cfg.StartupToken),
				} {
					if response.Code != http.StatusConflict || strings.Contains(response.Body.String(), sessionAuditNew) || strings.Contains(response.Body.String(), sessionAuditOld) {
						t.Fatal("incomplete local commit exposed a key as valid")
					}
				}
			}
			if fault == "final-metadata" {
				j, _ := m.Get(sessionAuditJobID)
				if j.TOTPResetCommitted || !j.TOTPRecoveryPending {
					t.Fatal("failed final commit released credential quarantine")
				}
				// Repair only the synthetic storage obstruction. Completion below
				// still goes through the real console and product runner.
				path := filepath.Join(cfg.OutputRoot, "job-meta.json")
				if err := os.Remove(path); err != nil {
					t.Fatal("could not remove synthetic metadata obstruction")
				}
				if err := os.Rename(path+".fixture-backup", path); err != nil {
					t.Fatal("could not restore original metadata file")
				}
			}
			if e := m.Shutdown(context.Background()); e != nil {
				t.Fatal(e)
			}
			next := jobs.NewManager(t.Context(), 1, st, logger)
			defer next.Shutdown(context.Background())
			next.SetCredentialStore(v)
			if e := next.Restore(t.Context()); e != nil {
				t.Fatal(e)
			}
			restoredAPI := console.NewServer(cfg, next, embed.FS{}, logger, v)
			defer restoredAPI.Close()
			if w := resetConsoleRequest(restoredAPI, cfg.StartupToken, "POST", "complete-2fa", `{}`); w.Code != 200 {
				t.Fatal("local-only action not admitted", w.Code)
			}
			if e := sessionAuditExecute(t, cfg, next, logger, v); e != nil {
				t.Fatal(e)
			}
			if remote.snapshot() != s || requests.Load() != before {
				t.Fatal("local recovery made a network request")
			}
			after := lastResetDetail(t, next)
			w = resetConsoleRequest(restoredAPI, cfg.StartupToken, "GET", "logs", "")
			if w.Code != 200 {
				t.Fatal("missing resumed logs", w.Code)
			}
			wantReason, pageName := "LOCAL_COMPLETE", "local-only"
			if fault == "active-read" || fault == "final-metadata" {
				pageName += "-" + fault
				if v.activeSaves.Load() != 1 {
					t.Fatal("offline completion rewrote an already matching active credential")
				}
				view := resetConsoleRequest(restoredAPI, cfg.StartupToken, "GET", "account-info", "")
				var info struct{ TOTPSecret, TOTPStatus string }
				if view.Code != http.StatusOK || json.Unmarshal(view.Body.Bytes(), &info) != nil || info.TOTPSecret != sessionAuditNew || info.TOTPStatus != "saved" {
					t.Fatal("offline completion did not make the confirmed new key available")
				}
			}
			if after.Match != wantMatch || after.Activation != wantReceipt {
				t.Fatal("offline completion invented a different confirmation source")
			}
			if fault == "local-commit-with-other" {
				wantReason, pageName = "RESET_SAVED_OTHER_VISIBLE", "local-only-with-other"
				if after.VisibleCount == nil || *after.VisibleCount != 2 || after.OldVisible || after.Source != "protected_journal" {
					t.Error("local recovery lost the earlier read or misidentified the other authenticator")
				}
			}
			exportResetPage(t, pageName, w.Body.Bytes())
			if after.OpRef != d.OpRef || after.Seq <= d.Seq || after.Reason != wantReason || after.Commit != "committed" || after.Writes == nil || *after.Writes != (resetlog.Writes{}) {
				t.Fatal("resumed log did not preserve operation or invented writes", after.Reason)
			}
		})
	}
}

// Freeze only this test's temporary aggregate, journal and in-memory vault
// while the upstream holds activation open, before it has returned any headers.
// Restore receives no request oracle, transport callback or prebuilt terminal log.
func TestResetCancellationAndInterruptedRestore(t *testing.T) {
	clearCLISensitiveEnvironment(t)
	held, release := make(chan struct{}), make(chan struct{})
	var activations, requests atomic.Int32
	remote := &sessionAuditRemote{t: t, sessionValid: true, factor: "old-factor"}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path != "/backend-api/accounts/mfa/user/activate_enrollment" {
			remote.serveHTTP(w, r)
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		if activations.Add(1) == 1 {
			close(held)
		} else {
			t.Error("activation replayed")
		}
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer server.Close()
	defer close(release)
	cfg := sessionAuditConfig(t.TempDir(), server.URL)
	cfg.Port, cfg.StartupToken = 80, "fixture-console-only"
	v := &resetRunnerVault{MemoryStore: credentials.NewMemoryStore()}
	m, st, logger := sessionAuditManager(t, cfg, v)
	api := console.NewServer(cfg, m, embed.FS{}, logger, v)
	defer api.Close()
	sessionAuditImport(t, cfg, m)
	if w := resetConsoleRequest(api, cfg.StartupToken, "POST", "setup-2fa", `{"resetTotp":true}`); w.Code != 200 {
		t.Fatal("reset not admitted", w.Code)
	}
	m.SetRunner(makeJobRunnerWithCredentials(cfg, m, logger, v, true))
	m.Start()
	select {
	case <-held:
	case <-time.After(10 * time.Second):
		t.Fatal("activation was not received")
	}
	before := requests.Load()
	// Observe the real console while activation is held by the fake upstream.
	// Read-only observers do not advance or recover the product workflow.
	for _, response := range []*httptest.ResponseRecorder{
		resetConsoleRequest(api, cfg.StartupToken, "GET", "account-info", ""), resetConsoleExport(api, cfg.StartupToken),
	} {
		if response.Code != http.StatusConflict || strings.Contains(response.Body.String(), sessionAuditNew) || strings.Contains(response.Body.String(), sessionAuditOld) {
			t.Fatal("pending key was shown or exported as a valid credential")
		}
	}
	pendingLogs := resetConsoleRequest(api, cfg.StartupToken, "GET", "logs", "")
	if pendingLogs.Code != http.StatusOK || !strings.Contains(pendingLogs.Body.String(), "新密钥已安全暂存，尚未激活，暂不可领取") || strings.Contains(pendingLogs.Body.String(), "新 2FA 密钥已安全保存") {
		t.Fatal("public pending progress did not distinguish staging from key delivery")
	}
	for _, marker := range []string{sessionAuditNew, sessionAuditOld, sessionAuditEmail, sessionAuditCookie, "synthetic-password", "otpauth://"} {
		if strings.Contains(pendingLogs.Body.String(), marker) {
			t.Fatal("pending log API exposed a synthetic private marker")
		}
	}
	snapshotCfg := sessionAuditConfig(t.TempDir(), server.URL)
	snapshotVault := &resetRunnerVault{MemoryStore: credentials.NewMemoryStore()}
	restarted, ss, _ := sessionAuditManager(t, snapshotCfg, snapshotVault)
	c, e := v.Load(sessionAuditEmail)
	if e != nil || !c.TOTPResetPending || c.PendingTOTPSecret != sessionAuditNew {
		t.Fatal("fixture not at the pending checkpoint")
	}
	if e = snapshotVault.Save(sessionAuditEmail, c); e != nil {
		t.Fatal(e)
	}
	var metadata json.RawMessage
	if e = st.ReadJSON(t.Context(), "job-meta.json", &metadata); e != nil {
		t.Fatal(e)
	}
	if e = ss.WriteJSON(t.Context(), "job-meta.json", metadata); e != nil {
		t.Fatal(e)
	}
	out := sessionAuditJournal(t, cfg)
	if _, ok := out["activation_receipt"]; ok {
		t.Fatal("snapshot already had an activation receipt")
	}
	if e = privatestate.WriteJSON(t.Context(), filepath.Join(ss.Root, sessionAuditJobID, "totp-setup-result.json"), "totp-reset", sessionAuditEmail, c.TOTPResetOperationID, out); e != nil {
		t.Fatal(e)
	}
	if w := resetConsoleRequest(api, cfg.StartupToken, "POST", "cancel", `{}`); w.Code != 200 {
		t.Fatal("cancel not accepted", w.Code)
	}
	if e = sessionAuditWait(t, m); e == nil {
		t.Fatal("canceled activation reported success")
	}
	canceled := lastResetDetail(t, m)
	w := resetConsoleRequest(api, cfg.StartupToken, "GET", "logs", "")
	if w.Code != 200 {
		t.Fatal("missing canceled logs", w.Code)
	}
	exportResetPage(t, "canceled", w.Body.Bytes())
	if canceled.Reason != "REQUEST_CANCELED" || canceled.Writes == nil || canceled.Writes.Activation != (resetlog.Count{Possible: 1}) || canceled.Pending != "verified" || canceled.Activation != "unknown" {
		t.Fatal("lost terminal cancellation evidence", canceled)
	}
	if e = restarted.Restore(t.Context()); e != nil {
		t.Fatal(e)
	}
	d := lastResetDetail(t, restarted)
	snapshotCfg.Port, snapshotCfg.StartupToken = 80, cfg.StartupToken
	snapshotAPI := console.NewServer(snapshotCfg, restarted, embed.FS{}, logger, snapshotVault)
	defer snapshotAPI.Close()
	w = resetConsoleRequest(snapshotAPI, cfg.StartupToken, "GET", "logs", "")
	if w.Code != 200 {
		t.Fatal("missing interrupted logs", w.Code)
	}
	exportResetPage(t, "interrupted", w.Body.Bytes())
	if d.OpRef != canceled.OpRef || d.Reason != "INTERRUPTED_WRITE" || d.Stage != "activation" || d.LastTrusted != "pending" || d.JournalWrites == nil || d.JournalWrites.Activation != (resetlog.Count{Possible: 1}) || d.Writes != nil || d.Removal != "accepted" {
		t.Fatal("restart changed an intent into a receipt or lost the operation", d)
	}
	if activations.Load() != 1 || requests.Load() != before || restarted.QueuePosition(sessionAuditJobID) != 0 {
		t.Fatal("cancel or restore replayed network work")
	}
}
