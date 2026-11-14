package jobs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"tosub2-go-port/internal/store"
)

// This tests the job layer at durable protocol boundaries. It does not claim
// to simulate the remote mutation or cookie lifetime: those belong to the
// protocol/runner tests. In particular, rejecting retry is not recovery.
func TestTOTPResumeAuditRetryCannotContinueUnverifiedReset(t *testing.T) {
	for _, tc := range []struct {
		name       string
		phase      string
		pendingKey bool
		activated  bool
		verified   bool
	}{
		{name: "removal_response_lost", phase: "disable_intent"},
		{name: "disabled_enrollment_failed", phase: "disabled_verified"},
		{name: "activation_response_lost", phase: "activation_intent", pendingKey: true},
		{name: "activation_acknowledged_unverified", phase: "activation_succeeded", pendingKey: true, activated: true},
		{name: "verified_activation_local_commit_failed", phase: "activation_verified", pendingKey: true, activated: true, verified: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, vault, job, peer := resetManagerFixture(t)
			const newSeed = "AUDIT_NEW_SEED"
			var runs atomic.Int32
			journalPath := filepath.Join(m.store.Root, job.ID, "totp-setup-result.json")
			m.SetRunner(func(ctx context.Context, active *Job, _ func(JobEvent)) error {
				runs.Add(1)
				if err := beginTOTPResetFixture(t, m, ctx, active.ID, "resume-audit-fixture"); err != nil {
					return err
				}
				if tc.pendingKey {
					c, err := vault.Load(active.Email)
					if err != nil {
						return err
					}
					c.PendingTOTPSecret, c.PendingTOTPURI = newSeed, "otpauth://totp/audit"
					if err := vault.Save(active.Email, c); err != nil {
						return err
					}
				}
				journal := map[string]any{
					"version": 1, "email": active.Email, "operation_id": "resume-audit-fixture", "reset_totp_requested": true,
					"reset_totp_phase": tc.phase, "reset_totp_status": "unknown",
					"old_factor_invalidated": true, "activation_succeeded": tc.activated,
					"activation_verified": tc.verified,
				}
				if tc.pendingKey {
					journal["secret"] = newSeed
				}
				private, err := store.New(filepath.Dir(journalPath))
				if err != nil {
					return err
				}
				if err := private.WriteJSON(ctx, filepath.Base(journalPath), journal); err != nil {
					return err
				}
				failure := errors.New("synthetic interruption after durable protocol boundary")
				return errors.Join(failure, m.UpdateTOTPResetResult(active.ID, journal, failure))
			})
			if _, err := m.QueueOperation(job.ID, "reset-2fa", nil); err != nil {
				t.Fatal(err)
			}
			m.Start()
			t.Cleanup(func() { _ = m.Shutdown(context.Background()) })
			waitForStatus(t, m, job.ID, StatusFailed)
			for _, id := range []string{job.ID, peer.ID} {
				if _, err := m.QueueOperation(id, "reset-2fa", nil); !errors.Is(err, ErrTOTPRecoveryRequired) {
					t.Fatal("second click unexpectedly admitted a fresh reset", err)
				}
				if _, err := m.Retry(id); !errors.Is(err, ErrTOTPRecoveryRequired) {
					t.Fatal("retry unexpectedly provided a continuation", err)
				}
			}
			if err := m.Shutdown(context.Background()); err != nil {
				t.Fatal(err)
			}
			if runs.Load() != 1 {
				t.Fatal("clicking retry reached the runner")
			}
			restored := NewManager(t.Context(), 1, m.store, nil)
			restored.SetCredentialStore(vault)
			restored.SetRunner(func(context.Context, *Job, func(JobEvent)) error { runs.Add(1); return nil })
			if err := restored.Restore(t.Context()); err != nil {
				t.Fatal(err)
			}
			got, _ := restored.Get(job.ID)
			c, err := vault.Load(job.Email)
			if err != nil {
				t.Fatal(err)
			}
			if !got.TOTPRecoveryPending || !c.TOTPResetPending || !got.TOTPCredentialInvalidated || c.TOTPSecret != "OLDSEEDCANARY" {
				t.Fatal("startup changed credentials or released reset protection")
			}
			if _, err := restored.QueueOperation(job.ID, "reset-2fa", nil); !errors.Is(err, ErrTOTPRecoveryRequired) {
				t.Fatal("legacy journal was promoted into mutable recovery", err)
			}
			if _, err := os.Stat(journalPath); err != nil {
				t.Fatal("startup discarded private evidence", err)
			}
			if tc.pendingKey && c.PendingTOTPSecret != newSeed {
				t.Fatal("startup discarded the pending key")
			}
			if len(restored.pending) != 0 {
				t.Fatal("restart queued unfinished reset work")
			}
			restored.Start()
			if err := restored.Shutdown(context.Background()); err != nil {
				t.Fatal(err)
			}
			if runs.Load() != 1 {
				t.Fatal("restart invoked remote-operation runner")
			}
		})
	}
}
