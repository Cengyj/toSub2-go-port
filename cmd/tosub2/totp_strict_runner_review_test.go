package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/jobs"
)

func TestStrictTOTPWorkflowReloginRejectsSupersededCredentialBeforeNetwork(t *testing.T) {
	clearCLISensitiveEnvironment(t)
	for _, scenario := range []string{"new-operation", "same-operation-changed-seed", "unchanged", "setup-current", "setup-superseded", "pending-uri", "pending-factor"} {
		t.Run(scenario, func(t *testing.T) {
			var requests atomic.Int32
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				w.WriteHeader(http.StatusForbidden)
			}))
			defer remote.Close()
			cfg := sessionAuditConfig(t.TempDir(), remote.URL)
			vault := &resetRunnerVault{MemoryStore: credentials.NewMemoryStore()}
			m, st, logger := sessionAuditManager(t, cfg, vault)
			defer m.Shutdown(context.Background())
			job, err := m.Submit(jobs.Spec{ID: sessionAuditJobID, Email: sessionAuditEmail, ImportOnly: true, Input: map[string]any{"password": "fixture-password", "totpSecret": sessionAuditOld}})
			if err != nil {
				t.Fatal(err)
			}
			job, err = m.QueueOperation(job.ID, "reset-2fa", map[string]any{"reloginAfterTotp": true})
			if err != nil {
				t.Fatal(err)
			}
			if err := m.StartTOTPWorkflowStep(t.Context(), job.ID, "totp"); err != nil {
				t.Fatal(err)
			}
			runnerResetIntentJournal(t, cfg.OutputRoot, job.ID, job.Email, "original-workflow-operation")
			if err := m.BeginTOTPReset(t.Context(), job.ID, "original-workflow-operation"); err != nil {
				t.Fatal(err)
			}
			current, err := vault.Load(job.Email)
			if err != nil {
				t.Fatal(err)
			}
			current.TOTPSecret, current.TOTPResetPending = sessionAuditNew, false
			if err := vault.Save(job.Email, current); err != nil {
				t.Fatal(err)
			}
			runnerCompletionJournal(t, cfg.OutputRoot, job.ID, job.Email, current.TOTPResetOperationID, current.TOTPSecret, "not_requested")
			if err := m.CommitTOTPReset(t.Context(), job.ID, current); err != nil {
				t.Fatal(err)
			}
			if err := m.MarkTOTPResetCommitted(t.Context(), job.ID, current.TOTPResetOperationID); err != nil {
				t.Fatal(err)
			}
			if strings.HasPrefix(scenario, "setup-") {
				// Restore a finished reset's durable metadata as a later app session.
				if err := m.Shutdown(context.Background()); err != nil {
					t.Fatal(err)
				}
				m = jobs.NewManager(t.Context(), 1, st, logger)
				m.SetCredentialStore(vault)
				defer m.Shutdown(context.Background())
				if err := m.Restore(t.Context()); err != nil {
					t.Fatal(err)
				}
				if _, err := m.QueueOperation(job.ID, "setup-2fa", map[string]any{"reloginAfterTotp": true}); err != nil {
					t.Fatal(err)
				}
				if err := m.StartTOTPWorkflowStep(t.Context(), job.ID, "totp"); err != nil {
					t.Fatal(err)
				}
				// A later setup may legitimately observe an already enabled factor
				// belonging to another reset. Historical operation metadata is retained.
				current.TOTPResetOperationID = "later-setup-credential-operation"
				if err := vault.Save(job.Email, current); err != nil {
					t.Fatal(err)
				}
				m.UpdateStoredCredentials(job.ID, current)
			}
			if err := m.StartTOTPWorkflowStep(t.Context(), job.ID, "relogin"); err != nil {
				t.Fatal(err)
			}
			job, _ = m.Get(job.ID)
			job.Operation, job.ResetTOTP = "relogin", false
			// Another process may finish an independent reset between workflow steps,
			// which have separate OS account leases. The manager still mirrors the
			// earlier seed; it must not authenticate with or save that stale candidate.
			newer := current
			if scenario == "new-operation" {
				newer.TOTPResetOperationID = "newer-independent-operation"
			}
			if scenario != "unchanged" && scenario != "setup-current" && !strings.HasPrefix(scenario, "pending-") {
				newer.TOTPSecret = "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"
			}
			if scenario == "pending-uri" {
				newer.PendingTOTPURI = "otpauth://totp/synthetic-pending"
			}
			if scenario == "pending-factor" {
				newer.PendingTOTPFactorID = "synthetic-pending-factor"
			}
			if err := vault.Save(job.Email, newer); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			err = makeJobRunnerWithCredentials(cfg, m, logger, vault, true)(ctx, job, func(jobs.JobEvent) {})
			if scenario == "unchanged" || scenario == "setup-current" {
				if requests.Load() == 0 || err == nil || strings.Contains(err.Error(), "TOTP_WORKFLOW_CREDENTIAL_CHANGED") {
					t.Fatal("matching reset could not enter the selected re-login step", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), "TOTP_WORKFLOW_CREDENTIAL_CHANGED") || requests.Load() != 0 {
				t.Fatalf("superseded reset reached reauthentication: requests=%d err=%v", requests.Load(), err)
			}
			after, err := vault.Load(job.Email)
			if err != nil || !reflect.DeepEqual(after, newer) {
				t.Fatal("old workflow changed newer credentials", err)
			}
		})
	}
}
