package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/protocol"
)

// Only the aggregate metadata target fails. The protected enrollment result
// and credential store remain writable, modeling a failure of that independent
// commit boundary after the remote has issued enrollment material.
func TestTOTPSetupMetadataFailureStopsActivationAndRetainsRecovery(t *testing.T) {
	clearCLISensitiveEnvironment(t)
	for _, phase := range []string{"enrollment", "activation", "rejected-enrollment", "activation-readback"} {
		t.Run(phase, func(t *testing.T) {
			root := t.TempDir()
			remote := &sessionAuditRemote{t: t, sessionValid: true, factor: ""}
			if phase == "rejected-enrollment" {
				remote.failure = "enroll-refused"
			}
			var injected atomic.Bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				faultPath := "/backend-api/accounts/mfa/enroll"
				if strings.HasPrefix(phase, "activation") {
					faultPath = "/backend-api/accounts/mfa/user/activate_enrollment"
				}
				if r.URL.Path == faultPath && injected.CompareAndSwap(false, true) {
					path := filepath.Join(root, "job-meta.json")
					if err := os.Rename(path, path+".fixture-backup"); err != nil {
						t.Error(err)
						http.Error(w, "fixture injection failed", http.StatusInternalServerError)
						return
					}
					if err := os.Mkdir(path, 0700); err != nil {
						t.Error(err)
						http.Error(w, "fixture injection failed", http.StatusInternalServerError)
						return
					}
				}
				remote.serveHTTP(w, r)
			}))
			defer server.Close()
			cfg := sessionAuditConfig(root, server.URL)
			vault := &setupReadbackFaultVault{MemoryStore: credentials.NewMemoryStore()}
			if phase == "activation-readback" {
				vault.phase = "activation"
			}
			m, _, logger := sessionAuditManager(t, cfg, vault)
			sessionAuditImport(t, cfg, m)
			if _, err := m.QueueOperation(sessionAuditJobID, "setup-2fa", nil); err != nil {
				t.Fatal(err)
			}
			err := sessionAuditExecute(t, cfg, m, logger, vault)
			if err == nil {
				t.Fatal("metadata failure reported setup success")
			}
			if !strings.Contains(err.Error(), "无法可靠保存 2FA 状态") || phase == "rejected-enrollment" && !strings.Contains(err.Error(), "TOTP_ENROLL_UNCONFIRMED") || phase == "activation-readback" && !strings.Contains(err.Error(), "TOTP_SETUP_PERSISTENCE_REQUIRED") {
				t.Fatal("independent operation and metadata failures were not preserved", err)
			}
			state := remote.snapshot()
			wantActivation := 0
			if strings.HasPrefix(phase, "activation") {
				wantActivation = 1
			}
			if !injected.Load() || state.enrollCalls != 1 || state.activateCalls != wantActivation {
				t.Fatalf("metadata failure crossed or repeated mutation boundary: %+v", state)
			}
			if phase == "rejected-enrollment" {
				return
			}
			saved, err := vault.Load(sessionAuditEmail)
			if err != nil {
				t.Fatal(err)
			}
			if phase != "activation" && saved.PendingTOTPSecret != sessionAuditNew || phase == "activation" && saved.TOTPSecret != sessionAuditNew {
				t.Fatal("obtained factor material was not retained")
			}
			path := filepath.Join(root, sessionAuditJobID, "totp-setup-result.json")
			result, err := protocol.ReadTOTPResetJournal(path)
			if err != nil || result["secret"] != sessionAuditNew || result["activation_succeeded"] != strings.HasPrefix(phase, "activation") {
				t.Fatal("metadata failure removed or misclassified the private recovery result", err)
			}
			// Repair only the synthetic metadata target and restore locally.
			// Confirmed activation may finish its save; enrollment alone must
			// retain pending material without replaying an upstream mutation.
			_ = m.Shutdown(context.Background())
			metadata := filepath.Join(root, "job-meta.json")
			if err := os.Remove(metadata); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(metadata+".fixture-backup", metadata); err != nil {
				t.Fatal(err)
			}
			vault.phase = ""
			restored, _, _ := sessionAuditManager(t, cfg, vault)
			if err := restored.Restore(t.Context()); err != nil {
				t.Fatal(err)
			}
			job, ok := restored.Get(sessionAuditJobID)
			if !ok || job.Status == "queued" || remote.snapshot() != state {
				t.Fatal("local recovery changed the outcome or replayed remote work")
			}
			saved, err = vault.Load(sessionAuditEmail)
			if err != nil || phase == "enrollment" && (saved.TOTPSecret != sessionAuditOld || saved.PendingTOTPSecret != sessionAuditNew) || strings.HasPrefix(phase, "activation") && (saved.TOTPSecret != sessionAuditNew || saved.PendingTOTPSecret != "" || !job.TOTPKnownEnabled) {
				t.Fatal("local recovery did not preserve the confirmed or pending credential state", err)
			}
		})
	}
}
