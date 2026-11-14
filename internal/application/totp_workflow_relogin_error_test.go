package application

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/protocol"
	"tosub2-go-port/internal/store"
)

type workflowReloginFailure struct {
	cause   error
	private string
}

func (e *workflowReloginFailure) Error() string {
	return "LEGACY_RUNTIME_CHALLENGE: GO_JS_RUNTIME: SCRIPT_EXCEPTION " + e.private
}

func (e *workflowReloginFailure) Unwrap() error { return e.cause }

func TestTOTPWorkflowReloginErrorPreservesCompletedReset(t *testing.T) {
	disk, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	manager := jobs.NewManager(t.Context(), 1, disk, nil)
	// This synthetic store exercises the durable-store gate without accessing
	// the operating system credential provider or a real account.
	vault := &strictWorkflowVault{credentials.NewMemoryStore()}
	manager.SetCredentialStore(vault)
	t.Cleanup(func() { _ = manager.Shutdown(context.Background()) })
	const password = "synthetic-relogin-error-password"
	const newSecret = "KRSXG5DSNFXGOIDB"
	initial, err := manager.Submit(jobs.Spec{ID: "relogin-error", Email: "relogin-error@example.test", ImportOnly: true, Input: map[string]any{
		"password": password, "totpSecret": "JBSWY3DPEHPK3PXP",
	}})
	if err != nil {
		t.Fatal(err)
	}
	job, err := manager.QueueOperation(initial.ID, "reset-2fa", map[string]any{
		"logoutAllDevicesAfterTotp": true, "reloginAfterTotp": true, "uploadAfterTotp": true,
		"sub2apiConfig": map[string]any{"baseUrl": "https://sub2api.example.test", "adminApiKey": "synthetic-admin-key"},
	})
	if err != nil {
		t.Fatal(err)
	}
	sentinel := errors.New("synthetic runtime failure")
	cause := &workflowReloginFailure{cause: sentinel, private: password}
	var calls []string
	operation := func(ctx context.Context, active *jobs.Job, emit func(jobs.JobEvent)) error {
		calls = append(calls, active.Operation)
		if active.Operation == "relogin" {
			current, _ := manager.Get(active.ID)
			if current.TOTPWorkflowStep != "relogin" || current.LogoutAllDevicesStatus != "succeeded" || !current.TOTPResetCommitted || current.ResultSaved {
				return errors.New("relogin started before the reset and logout were committed")
			}
			emit(jobs.JobEvent{Stage: "legacy_challenge", Status: jobs.StatusRunning})
			return cause
		}
		const operationID = "synthetic-relogin-error-reset"
		intent := map[string]any{
			"version": 1, "operation_scope": "current_session", "email": active.Email, "operation_id": operationID, "account_id": "fixture-account",
			"reset_totp_requested": true, "reset_totp_phase": "quarantine_intent", "old_totp_factor_id": "old-factor", "old_factor_invalidated": false,
			"mutation_schema": 1, "disable_dispatch": "not_sent", "enroll_dispatch": "not_sent", "activation_dispatch": "not_sent", "enroll_attempted": false, "activation_attempted": false,
		}
		if err := protocol.WritePrivateJSON(filepath.Join(disk.Root, active.ID, "totp-setup-result.json"), intent); err != nil {
			return err
		}
		if err := manager.BeginTOTPReset(ctx, active.ID, operationID); err != nil {
			return err
		}
		saved, err := vault.Load(active.Email)
		if err != nil {
			return err
		}
		saved.TOTPSecret, saved.TOTPResetPending = newSecret, false
		if err := vault.Save(active.Email, saved); err != nil {
			return err
		}
		writeCommittedResetFixture(t, disk.Root, manager, vault, active.ID, operationID)
		if err := manager.CommitTOTPReset(ctx, active.ID, saved); err != nil {
			return err
		}
		writeCommittedResetFixture(t, disk.Root, manager, vault, active.ID, operationID)
		if err := manager.MarkTOTPResetCommitted(ctx, active.ID, operationID); err != nil {
			return err
		}
		if err := manager.StartTOTPWorkflowStep(ctx, active.ID, "logout"); err != nil {
			return err
		}
		calls = append(calls, "logout")
		if err := manager.UpdateTOTPResetResult(active.ID, map[string]any{"logout_all_devices_requested": true, "logout_all_devices_status": "succeeded"}, nil); err != nil {
			return err
		}
		writeCommittedResetFixture(t, disk.Root, manager, vault, active.ID, operationID)
		return nil
	}
	upload := func(context.Context, *jobs.Job, func(jobs.JobEvent)) error {
		calls = append(calls, "upload")
		return errors.New("unexpected upload")
	}
	runner := TOTPWorkflowRunner(manager, vault, operation, upload)
	finished := make(chan error, 1)
	manager.SetRunner(func(ctx context.Context, active *jobs.Job, emit func(jobs.JobEvent)) error {
		err := runner(ctx, active, emit)
		finished <- err
		return err
	})
	manager.Start()
	select {
	case err = <-finished:
	case <-time.After(3 * time.Second):
		t.Fatal("workflow did not finish")
	}
	var typed *workflowReloginFailure
	if !errors.Is(err, sentinel) || !errors.As(err, &typed) || typed != cause {
		t.Fatalf("relogin error lost its cause or type: %T", err)
	}
	const contextMessage = "新 2FA 已保存；后续重登授权失败"
	if !strings.Contains(err.Error(), contextMessage) || strings.Contains(err.Error(), password) {
		t.Fatal("relogin error lacks saved-factor context or exposes a credential")
	}
	deadline := time.After(3 * time.Second)
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	var state *jobs.Job
	for {
		state, _ = manager.Get(job.ID)
		if state.FinishedAt != nil {
			break
		}
		select {
		case <-ticker.C:
		case <-deadline:
			t.Fatal("scheduler did not record the failed workflow")
		}
	}
	if state.Status != jobs.StatusFailed || state.OperationStatus != "failed" || state.TOTPWorkflowStatus != "failed" || state.TOTPReloginStatus != "failed" || state.TOTPUploadStatus != "skipped" {
		t.Fatal("relogin failure changed the workflow outcome or continued upload")
	}
	if state.Operation != "reset-2fa" || state.LastOperationType != "reset-2fa" || !state.LastOperationAt.Equal(job.LastOperationAt) || state.Stage != "legacy_challenge" {
		t.Fatal("error context changed the operation identity or failure stage")
	}
	if state.TOTPResetPhase != "credential_persisted" || state.TOTPResetStatus != "activated" || !state.TOTPResetCommitted || !state.TOTPActivationVerified || !state.HasTOTPKey || state.TOTPRecoveryPending || state.TOTPCredentialInvalidated || state.ResultSaved || state.LogoutAllDevicesStatus != "succeeded" {
		t.Fatal("relogin failure discarded the completed reset or reported a new authorization")
	}
	saved, err := vault.Load(job.Email)
	if err != nil || saved.TOTPSecret != newSecret || saved.TOTPResetPending {
		t.Fatal("relogin failure changed the saved credential")
	}
	if _, err := protocol.ReadTOTPResetCompletion(filepath.Join(disk.Root, job.ID, "totp-setup-result.json"), job.Email); err != nil {
		t.Fatal("relogin failure damaged the completed reset evidence", err)
	}
	metadata, err := os.ReadFile(filepath.Join(disk.Root, "job-meta.json"))
	if err != nil || !strings.Contains(string(metadata), contextMessage) || strings.Contains(string(metadata), password) || strings.Contains(string(metadata), newSecret) {
		t.Fatal("persisted workflow lost safe context or disclosed credentials")
	}
	if err := runner(t.Context(), state, func(jobs.JobEvent) {}); err == nil {
		t.Fatal("terminal workflow became replayable")
	}
	if !reflect.DeepEqual(calls, []string{"reset-2fa", "logout", "relogin"}) {
		t.Fatalf("completed steps replayed or later steps ran: %v", calls)
	}
}

func TestTOTPWorkflowReloginErrorContextRequiresCompletedSave(t *testing.T) {
	for _, scenario := range []string{"factor-failed", "credential-missing", "standalone-relogin"} {
		t.Run(scenario, func(t *testing.T) {
			manager, vault, initial, _ := workflowPanicFixture(t)
			op := "setup-2fa"
			if scenario == "standalone-relogin" {
				op = "relogin"
			}
			job, err := manager.QueueOperation(initial.ID, op, map[string]any{"reloginAfterTotp": true})
			if err != nil {
				t.Fatal(err)
			}
			cause := errors.New("synthetic authorization failure")
			var calls []string
			operation := func(_ context.Context, active *jobs.Job, _ func(jobs.JobEvent)) error {
				calls = append(calls, active.Operation)
				if scenario == "credential-missing" {
					return nil
				}
				return cause
			}
			err = TOTPWorkflowRunner(manager, vault, operation, nil)(t.Context(), job, func(jobs.JobEvent) {})
			if err == nil || strings.Contains(err.Error(), "新 2FA 已保存；后续重登授权失败") || !reflect.DeepEqual(calls, []string{op}) {
				t.Fatal("failure before the save gate falsely claimed a saved factor or started relogin")
			}
			if scenario != "credential-missing" && !errors.Is(err, cause) {
				t.Fatal("original operation error lost its cause")
			}
		})
	}
}
