package jobs

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"tosub2-go-port/internal/store"
)

type workflowUploadOutcomeFixture struct{ uncertain bool }

func (*workflowUploadOutcomeFixture) Error() string                  { return "synthetic upload failure" }
func (e *workflowUploadOutcomeFixture) UploadOutcomeUncertain() bool { return e.uncertain }

func TestTOTPWorkflowUploadOutcomeRetainsNewAuthorization(t *testing.T) {
	for _, tc := range []struct {
		name     string
		err      error
		canceled bool
		want     string
	}{
		{name: "confirmed_rejection", err: &workflowUploadOutcomeFixture{}, want: "failed"},
		{name: "wrapped_before_write_failure", err: fmt.Errorf("lookup: %w", &workflowUploadOutcomeFixture{}), want: "failed"},
		{name: "confirmed_unknown", err: &workflowUploadOutcomeFixture{uncertain: true}, want: "unknown"},
		{name: "unclassified_failure", err: errors.New("synthetic response lost"), want: "unknown"},
		{name: "canceled_before_write", err: &workflowUploadOutcomeFixture{}, canceled: true, want: "failed"},
		{name: "canceled_after_send", err: &workflowUploadOutcomeFixture{uncertain: true}, canceled: true, want: "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, vault, job, _ := resetManagerFixture(t)
			job, err := m.QueueOperation(job.ID, "reset-2fa", map[string]any{"reloginAfterTotp": true, "uploadAfterTotp": true})
			if err != nil {
				t.Fatal(err)
			}
			if err := m.StartTOTPWorkflowStep(t.Context(), job.ID, "totp"); err != nil {
				t.Fatal(err)
			}
			persistWorkflowFactor(t, m, vault, job)
			if err := m.StartTOTPWorkflowStep(t.Context(), job.ID, "relogin"); err != nil {
				t.Fatal(err)
			}
			private, err := store.New(filepath.Join(m.store.Root, job.ID))
			if err != nil {
				t.Fatal(err)
			}
			if err := private.WriteJSON(t.Context(), "sub2api-import-oauth.json", map[string]any{"type": "sub2api-data", "accounts": []any{map[string]any{"credentials": map[string]any{"email": job.Email, "access_token": "synthetic-new-grant"}}}}); err != nil {
				t.Fatal(err)
			}
			m.UpdateResult(job.ID, true, "fresh grant saved")
			if err := m.CompleteTOTPWorkflowStep(t.Context(), job.ID, "relogin"); err != nil {
				t.Fatal(err)
			}
			if err := m.StartTOTPWorkflowStep(t.Context(), job.ID, "upload"); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			wantWorkflow := "failed"
			if tc.canceled {
				cancel()
				wantWorkflow = "interrupted"
			}
			if err := m.FinishTOTPWorkflowContext(ctx, job.ID, tc.err); err != nil {
				t.Fatal(err)
			}
			check := func(current *Job) {
				t.Helper()
				if current.TOTPUploadStatus != tc.want || current.TOTPWorkflowStatus != wantWorkflow {
					t.Fatalf("upload=%s workflow=%s, want %s %s", current.TOTPUploadStatus, current.TOTPWorkflowStatus, tc.want, wantWorkflow)
				}
				if !current.ResultSaved || !current.HasTOTPKey || current.TOTPReloginStatus != "succeeded" || current.TOTPRecoveryPending || current.TOTPCredentialInvalidated {
					t.Fatal("upload failure discarded committed factor or authorization")
				}
			}
			current, _ := m.Get(job.ID)
			check(current)
			stored, err := vault.Load(job.Email)
			if err != nil || stored.TOTPSecret != "NEWFACTORRESTART" || stored.TOTPResetPending {
				t.Fatal("upload failure changed the new protected factor")
			}
			restored := NewManager(t.Context(), 1, m.store, nil)
			restored.SetCredentialStore(vault)
			if err := restored.Restore(t.Context()); err != nil {
				t.Fatal(err)
			}
			current, _ = restored.Get(job.ID)
			check(current)
			if len(restored.pending) != 0 {
				t.Fatal("failed optional upload replayed a workflow on restore")
			}
		})
	}
}
