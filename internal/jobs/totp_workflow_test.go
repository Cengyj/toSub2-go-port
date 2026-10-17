package jobs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"tosub2-go-port/internal/store"
)

func TestTOTPWorkflowRequestDeadlineRemainsFailedAcrossRestore(t *testing.T) {
	m, vault, job, _ := resetManagerFixture(t)
	if _, err := m.QueueOperation(job.ID, "reset-2fa", map[string]any{"reloginAfterTotp": true, "uploadAfterTotp": true}); err != nil {
		t.Fatal(err)
	}
	m.SetRunner(func(ctx context.Context, active *Job, emit func(JobEvent)) error {
		if err := m.StartTOTPWorkflowStep(ctx, active.ID, "totp"); err != nil {
			return err
		}
		emit(JobEvent{JobID: active.ID, Stage: "legacy_session_start", Status: StatusRunning})
		requestCtx, stop := context.WithTimeout(ctx, time.Millisecond)
		defer stop()
		<-requestCtx.Done()
		failure := fmt.Errorf("request timed out: %w", requestCtx.Err())
		if ctx.Err() != nil {
			t.Error("synthetic request deadline canceled its job")
		}
		return errors.Join(failure, m.FinishTOTPWorkflowContext(ctx, active.ID, failure))
	})
	m.Start()
	t.Cleanup(func() { _ = m.Shutdown(context.Background()) })
	current := waitForStatus(t, m, job.ID, StatusFailed)
	if current.OperationStatus != "failed" || current.TOTPWorkflowStatus != "failed" || current.TOTPResetPhase != "initial" || current.TOTPRecoveryPending || current.TOTPCredentialInvalidated || current.TOTPReloginStatus != "skipped" || current.TOTPUploadStatus != "skipped" || !strings.Contains(current.Error, "request timed out") {
		t.Fatal("request deadline was mislabeled or advanced account mutations")
	}
	if err := m.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	restored := NewManager(context.Background(), 1, m.store, nil)
	restored.SetCredentialStore(vault)
	if err := restored.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	current, _ = restored.Get(job.ID)
	if current.Status != StatusFailed || current.OperationStatus != "failed" || current.TOTPWorkflowStatus != "failed" || !strings.Contains(current.OperationError, "request timed out") || len(restored.pending) != 0 {
		t.Fatal("restore relabeled request failure as interruption or replayed the reset")
	}
}

func TestTOTPWorkflowContextDistinguishesChildFailureFromJobInterruption(t *testing.T) {
	for _, scenario := range []string{"request_timeout", "request_canceled", "job_timeout", "job_canceled"} {
		t.Run(scenario, func(t *testing.T) {
			m, _, job, _ := resetManagerFixture(t)
			if _, err := m.QueueOperation(job.ID, "reset-2fa", map[string]any{"reloginAfterTotp": true, "uploadAfterTotp": true}); err != nil {
				t.Fatal(err)
			}
			if err := m.StartTOTPWorkflowStep(context.Background(), job.ID, "totp"); err != nil {
				t.Fatal(err)
			}
			jobCtx := context.Background()
			failure := error(context.DeadlineExceeded)
			want := "failed"
			switch scenario {
			case "request_canceled":
				failure = context.Canceled
			case "job_timeout":
				var stop context.CancelFunc
				jobCtx, stop = context.WithDeadline(jobCtx, time.Now().Add(-time.Second))
				defer stop()
				want = "interrupted"
			case "job_canceled":
				var stop context.CancelFunc
				jobCtx, stop = context.WithCancel(jobCtx)
				stop()
				failure, want = context.Canceled, "interrupted"
			}
			if err := m.FinishTOTPWorkflowContext(jobCtx, job.ID, failure); err != nil {
				t.Fatal(err)
			}
			current, _ := m.Get(job.ID)
			if current.TOTPWorkflowStatus != want || current.TOTPReloginStatus != "skipped" || current.TOTPUploadStatus != "skipped" || current.TOTPRecoveryPending {
				t.Fatalf("workflow status=%s, want=%s", current.TOTPWorkflowStatus, want)
			}
		})
	}
}

func persistWorkflowFactor(t *testing.T, m *Manager, vault *memoryVault, job *Job) {
	t.Helper()
	if err := m.BeginTOTPReset(t.Context(), job.ID); err != nil {
		t.Fatal(err)
	}
	c, _ := vault.Load(job.Email)
	c.TOTPSecret, c.TOTPResetPending = "NEWFACTORRESTART", false
	if err := vault.Save(job.Email, c); err != nil {
		t.Fatal(err)
	}
	if err := m.CommitTOTPReset(t.Context(), job.ID, c); err != nil {
		t.Fatal(err)
	}
}

func TestTOTPWorkflowRestartNeverReplaysAnyStage(t *testing.T) {
	for _, step := range []string{"queued", "totp", "logout", "relogin", "upload"} {
		t.Run(step, func(t *testing.T) {
			m, vault, job, _ := resetManagerFixture(t)
			job, err := m.QueueOperation(job.ID, "reset-2fa", map[string]any{"reloginAfterTotp": true, "uploadAfterTotp": true, "logoutAllDevicesAfterTotp": true, "sub2apiConfig": map[string]any{"baseUrl": "https://private.example.test", "adminApiKey": "restart-key-canary"}})
			if err != nil {
				t.Fatal(err)
			}
			if step != "queued" {
				if err := m.StartTOTPWorkflowStep(t.Context(), job.ID, "totp"); err != nil {
					t.Fatal(err)
				}
				if step == "totp" {
					if err := m.BeginTOTPReset(t.Context(), job.ID); err != nil {
						t.Fatal(err)
					}
				} else {
					persistWorkflowFactor(t, m, vault, job)
					if err := m.StartTOTPWorkflowStep(t.Context(), job.ID, "logout"); err != nil {
						t.Fatal(err)
					}
					if step != "logout" {
						if err := m.UpdateTOTPResetResult(job.ID, map[string]any{"logout_all_devices_status": "succeeded"}, nil); err != nil {
							t.Fatal(err)
						}
					}
					if step != "logout" {
						if err := m.StartTOTPWorkflowStep(t.Context(), job.ID, "relogin"); err != nil {
							t.Fatal(err)
						}
						if step == "upload" {
							m.UpdateResult(job.ID, true, "fresh grant saved")
							if err := m.CompleteTOTPWorkflowStep(t.Context(), job.ID, "relogin"); err != nil {
								t.Fatal(err)
							}
							if err := m.StartTOTPWorkflowStep(t.Context(), job.ID, "upload"); err != nil {
								t.Fatal(err)
							}
						}
					}
				}
			}
			before, _ := os.ReadFile(filepath.Join(m.store.Root, "job-meta.json"))
			if strings.Contains(string(before), "restart-key-canary") || strings.Contains(string(before), "private.example.test") {
				t.Fatal("workflow settings written to public metadata")
			}
			restored := NewManager(context.Background(), 1, m.store, nil)
			restored.SetCredentialStore(vault)
			if err := restored.Restore(t.Context()); err != nil {
				t.Fatal(err)
			}
			state, _ := restored.Get(job.ID)
			if state.TOTPWorkflowStatus != "interrupted" || !state.ReloginAfterTOTP || !state.UploadAfterTOTP || !state.LogoutAllDevicesAfterTOTP || state.Status != StatusFailed || len(restored.pending) != 0 {
				t.Fatal("restart lost intent or queued a mutation", state)
			}
			if state.Input["sub2apiConfig"] != nil {
				t.Fatal("restart restored private integration settings")
			}
			if step == "upload" && state.TOTPUploadStatus != "unknown" {
				t.Fatal("in-flight upload result must remain uncertain")
			}
			if step == "relogin" && state.TOTPReloginStatus != "unknown" {
				t.Fatal("in-flight authorization result must remain uncertain")
			}
			if step == "totp" && !state.TOTPRecoveryPending {
				t.Fatal("restart discarded factor quarantine")
			}
			var ran atomic.Int32
			restored.SetRunner(func(context.Context, *Job, func(JobEvent)) error { ran.Add(1); return nil })
			restored.Start()
			if err := restored.Shutdown(context.Background()); err != nil {
				t.Fatal(err)
			}
			if ran.Load() != 0 {
				t.Fatal("restart replayed external work")
			}
		})
	}
}

func TestTOTPWorkflowCancellationAndConfigSecrecy(t *testing.T) {
	m, vault, job, _ := resetManagerFixture(t)
	job, err := m.QueueOperation(job.ID, "reset-2fa", map[string]any{"reloginAfterTotp": true, "uploadAfterTotp": true, "sub2apiConfig": map[string]any{"adminApiKey": "workflow-safe-error-canary"}})
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
	m.UpdateResult(job.ID, true, "fresh grant saved")
	if err := m.CompleteTOTPWorkflowStep(t.Context(), job.ID, "relogin"); err != nil {
		t.Fatal(err)
	}
	if err := m.StartTOTPWorkflowStep(t.Context(), job.ID, "upload"); err != nil {
		t.Fatal(err)
	}
	if err := m.FinishTOTPWorkflow(job.ID, errors.New("remote returned workflow-safe-error-canary")); err != nil {
		t.Fatal(err)
	}
	current, _ := m.Get(job.ID)
	if current.TOTPWorkflowStatus != "failed" || current.TOTPUploadStatus != "unknown" || current.TOTPReloginStatus != "succeeded" {
		t.Fatal("failed stage allowed downstream continuation")
	}
	data, _ := os.ReadFile(filepath.Join(m.store.Root, "job-meta.json"))
	if strings.Contains(string(data), "workflow-safe-error-canary") {
		t.Fatal("nested API key leaked in free-form error")
	}
	m, _, job, _ = resetManagerFixture(t)
	if _, err := m.QueueOperation(job.ID, "reset-2fa", map[string]any{"reloginAfterTotp": true, "uploadAfterTotp": true}); err != nil {
		t.Fatal(err)
	}
	if !m.Cancel(job.ID) {
		t.Fatal("queued workflow not canceled")
	}
	current, _ = m.Get(job.ID)
	if current.TOTPWorkflowStatus != "interrupted" || current.TOTPReloginStatus != "skipped" || current.TOTPUploadStatus != "skipped" {
		t.Fatal("cancel left active downstream stages")
	}
	if !current.UpdatedAt.After(time.Time{}) {
		t.Fatal("workflow update missing")
	}
}

func TestTOTPWorkflowLogoutErrorStaysRedactedAfterConfigRemoval(t *testing.T) {
	m, vault, job, _ := resetManagerFixture(t)
	job, err := m.QueueOperation(job.ID, "reset-2fa", map[string]any{"logoutAllDevicesAfterTotp": true, "reloginAfterTotp": true, "uploadAfterTotp": true, "sub2apiConfig": map[string]any{"adminApiKey": "logout-error-private-canary"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.StartTOTPWorkflowStep(t.Context(), job.ID, "totp"); err != nil {
		t.Fatal(err)
	}
	persistWorkflowFactor(t, m, vault, job)
	if err := m.StartTOTPWorkflowStep(t.Context(), job.ID, "logout"); err != nil {
		t.Fatal(err)
	}
	if err := m.UpdateTOTPResetResult(job.ID, map[string]any{"logout_all_devices_status": "failed", "logout_all_devices_error": "HTTP 400 logout-error-private-canary NEWFACTORRESTART"}, nil); err != nil {
		t.Fatal(err)
	}
	before, _ := m.Get(job.ID)
	if strings.Contains(before.LogoutAllDevicesError, "logout-error-private-canary") || strings.Contains(before.LogoutAllDevicesError, "NEWFACTORRESTART") {
		t.Fatal("raw logout error retained secret in manager memory")
	}
	if err := m.FinishTOTPWorkflow(job.ID, errors.New(before.LogoutAllDevicesError)); err != nil {
		t.Fatal(err)
	}
	current, _ := m.Get(job.ID)
	if current.Input["sub2apiConfig"] != nil || !strings.Contains(current.LogoutAllDevicesError, "HTTP 400") || !strings.Contains(current.TOTPWorkflowError, "HTTP 400") {
		t.Fatal("cleanup lost safe logout diagnostic or retained integration config")
	}
	data, err := os.ReadFile(filepath.Join(m.store.Root, "job-meta.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"logout-error-private-canary", "NEWFACTORRESTART"} {
		if strings.Contains(string(data), secret) || strings.Contains(current.TOTPWorkflowError, secret) {
			t.Fatal("logout error exposed secret after private config removal")
		}
	}
}

func TestTOTPWorkflowInterruptedReceivedGrantNeverExposesOldOutput(t *testing.T) {
	m, vault, job, _ := resetManagerFixture(t)
	job, err := m.QueueOperation(job.ID, "reset-2fa", map[string]any{"reloginAfterTotp": true})
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
	dir, err := store.New(filepath.Join(m.store.Root, job.ID))
	if err != nil {
		t.Fatal(err)
	}
	if err := dir.WriteJSON(t.Context(), "sub2api-import-oauth.json", map[string]any{"type": "sub2api-data", "accounts": []any{map[string]any{"credentials": map[string]any{"email": job.Email, "access_token": "old-grant"}}}}); err != nil {
		t.Fatal(err)
	}
	if err := dir.WriteJSON(t.Context(), "login-checkpoint.json", map[string]any{"version": 1, "email": job.Email, "stage": "tokens_received", "oauth": map[string]any{"tokenResponse": map[string]any{"access_token": "new-grant"}}}); err != nil {
		t.Fatal(err)
	}
	restored := NewManager(context.Background(), 1, m.store, nil)
	restored.SetCredentialStore(vault)
	if err := restored.Restore(t.Context()); err != nil {
		t.Fatal(err)
	}
	got, _ := restored.Get(job.ID)
	if got.ResultSaved || got.TOTPWorkflowStatus != "interrupted" || got.TOTPReloginStatus != "unknown" || got.Status != StatusFailed || len(restored.pending) != 0 {
		t.Fatal("interrupted workflow exposed or replayed stale output", got)
	}
}

func TestCompletedTOTPWorkflowDoesNotOverrideLaterOperations(t *testing.T) {
	for _, op := range []string{"relogin", "regenerate", "add-password"} {
		t.Run(op, func(t *testing.T) {
			m, _, job, _ := resetManagerFixture(t)
			m.mu.Lock()
			prior := m.jobs[job.ID]
			prior.Status, prior.Operation, prior.SecurityActionReturnStatus = StatusSucceeded, "reset-2fa", ""
			prior.ResetTOTP, prior.ResultSaved = true, true
			prior.TOTPResetPhase = "credential_persisted"
			prior.ReloginAfterTOTP, prior.UploadAfterTOTP = true, true
			prior.TOTPWorkflowStatus, prior.TOTPReloginStatus, prior.TOTPUploadStatus = "succeeded", "succeeded", "succeeded"
			m.mu.Unlock()
			queued, err := m.QueueOperation(job.ID, op, nil)
			if err != nil {
				t.Fatal(err)
			}
			if queued.ReloginAfterTOTP || queued.UploadAfterTOTP || queued.TOTPWorkflowStatus != "" || queued.TOTPReloginStatus != "" || queued.TOTPUploadStatus != "" {
				t.Fatal("new operation retained completed workflow state")
			}
			m.SetRunner(func(context.Context, *Job, func(JobEvent)) error { return errors.New("later operation failed") })
			m.Start()
			t.Cleanup(func() { _ = m.Shutdown(context.Background()) })
			want := StatusFailed
			if isSuboperation(op) {
				want = StatusSucceeded
			}
			got := waitForStatus(t, m, job.ID, want)
			if got.Error != "later operation failed" || strings.Contains(got.Prompt, "流程已完成") {
				t.Fatal("completed workflow hid later failure", got)
			}
			// Restore also has to ignore historical workflow fields when an
			// older metadata record retained them on an ordinary operation.
			got.ReloginAfterTOTP, got.TOTPWorkflowStatus, got.TOTPReloginStatus = true, "succeeded", "succeeded"
			restoreOperationState(got, true, false, privateResultRecovery{})
			if got.Error != "later operation failed" || strings.Contains(got.Prompt, "流程已完成") {
				t.Fatal("restore hid later failure with historical workflow", got)
			}
		})
	}
}

func TestTOTPWorkflowRequiresAcknowledgedPreviousStages(t *testing.T) {
	m, vault, job, _ := resetManagerFixture(t)
	m.UpdateTOTPSetup(job.ID, "", "", "historical setup failure", false)
	job, err := m.QueueOperation(job.ID, "reset-2fa", map[string]any{"reloginAfterTotp": true, "uploadAfterTotp": true, "logoutAllDevicesAfterTotp": true})
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range []string{"logout", "relogin", "upload"} {
		if err := m.StartTOTPWorkflowStep(t.Context(), job.ID, step); err == nil {
			t.Fatal("stage bypassed factor setup", step)
		}
	}
	if err := m.StartTOTPWorkflowStep(t.Context(), job.ID, "totp"); err != nil {
		t.Fatal(err)
	}
	if err := m.StartTOTPWorkflowStep(t.Context(), job.ID, "totp"); err == nil {
		t.Fatal("duplicate factor execution accepted")
	}
	persistWorkflowFactor(t, m, vault, job)
	if err := m.StartTOTPWorkflowStep(t.Context(), job.ID, "relogin"); err == nil {
		t.Fatal("relogin before logout confirmation accepted")
	}
	m.UpdateResult(job.ID, true, "old output")
	if err := m.StartTOTPWorkflowStep(t.Context(), job.ID, "logout"); err != nil {
		t.Fatal(err)
	}
	state, _ := m.Get(job.ID)
	if state.ResultSaved {
		t.Fatal("logout did not invalidate old grant metadata")
	}
	if err := m.UpdateTOTPResetResult(job.ID, map[string]any{"logout_all_devices_status": "succeeded"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := m.StartTOTPWorkflowStep(t.Context(), job.ID, "relogin"); err != nil {
		t.Fatal(err)
	}
	if err := m.CompleteTOTPWorkflowStep(t.Context(), job.ID, "relogin"); err == nil {
		t.Fatal("missing new grant completed relogin")
	}
	if err := m.StartTOTPWorkflowStep(t.Context(), job.ID, "upload"); err == nil {
		t.Fatal("upload before grant acknowledgement accepted")
	}
	if err := m.FinishTOTPWorkflow(job.ID, nil); err == nil {
		t.Fatal("unfinished selected stages reported success")
	}
	state, _ = m.Get(job.ID)
	if state.TOTPWorkflowStatus != "failed" || state.TOTPReloginStatus != "failed" || state.TOTPUploadStatus != "skipped" {
		t.Fatal("incomplete stages not stopped", state)
	}
}

func TestTOTPWorkflowClearedAtEveryIndependentOperationBoundary(t *testing.T) {
	for _, boundary := range []string{"retry", "requeue", "repair", "update", "credential-edit-restart"} {
		t.Run(boundary, func(t *testing.T) {
			m, _, job, _ := resetManagerFixture(t)
			m.mu.Lock()
			j := m.jobs[job.ID]
			j.Status, j.Operation, j.ResultSaved = StatusFailed, "relogin", true
			j.ReloginAfterTOTP, j.UploadAfterTOTP, j.LogoutAllDevicesAfterTOTP = true, true, true
			j.TOTPWorkflowStatus, j.TOTPReloginStatus, j.TOTPUploadStatus = "succeeded", "succeeded", "succeeded"
			j.Input["sub2apiConfig"] = map[string]any{"adminApiKey": "boundary-canary"}
			m.mu.Unlock()
			var err error
			switch boundary {
			case "retry":
				_, err = m.Retry(job.ID)
			case "requeue":
				_, err = m.Requeue(job.ID, "relogin")
			case "repair":
				_, err = m.QueueAutoRepair(job.ID, []int{1}, "https://sub2.example.test")
			case "update":
				_, err = m.Update(job.ID, map[string]any{"operation": "full"})
			case "credential-edit-restart":
				m.mu.Lock()
				m.jobs[job.ID].Status = StatusRunning
				m.jobs[job.ID].CreatedAt = time.Now().UTC()
				m.active[job.ID] = func() {}
				m.mu.Unlock()
				_, _, err = m.SubmitUniqueEmail(Spec{Email: job.Email, Input: map[string]any{"password": "edited-password"}})
				m.SetRunner(func(context.Context, *Job, func(JobEvent)) error { return context.Canceled })
				m.wg.Add(1)
				ctx, cancel := context.WithCancel(t.Context())
				m.run(ctx, cancel, job)
			}
			if err != nil {
				t.Fatal(err)
			}
			got, _ := m.Get(job.ID)
			if got.ResetTOTP || got.ReloginAfterTOTP || got.UploadAfterTOTP || got.LogoutAllDevicesAfterTOTP || got.TOTPWorkflowStatus != "" || got.TOTPReloginStatus != "" || got.TOTPUploadStatus != "" || got.Input["sub2apiConfig"] != nil {
				t.Fatal("independent operation retained previous workflow", got)
			}
		})
	}
}

func TestTOTPWorkflowLogoutOnlyKeepsOldGrantUnusableAcrossRestart(t *testing.T) {
	for _, logout := range []string{"succeeded", "unknown"} {
		t.Run(logout, func(t *testing.T) {
			m, vault, job, _ := resetManagerFixture(t)
			job, err := m.QueueOperation(job.ID, "reset-2fa", map[string]any{"logoutAllDevicesAfterTotp": true})
			if err != nil {
				t.Fatal(err)
			}
			dir, err := store.New(filepath.Join(m.store.Root, job.ID))
			if err != nil {
				t.Fatal(err)
			}
			if err := dir.WriteJSON(t.Context(), "sub2api-import-oauth.json", map[string]any{"type": "sub2api-data", "accounts": []any{map[string]any{"credentials": map[string]any{"email": job.Email, "access_token": "old-grant"}}}}); err != nil {
				t.Fatal(err)
			}
			m.UpdateResult(job.ID, true, "old output")
			if err := m.StartTOTPWorkflowStep(t.Context(), job.ID, "totp"); err != nil {
				t.Fatal(err)
			}
			persistWorkflowFactor(t, m, vault, job)
			if err := m.StartTOTPWorkflowStep(t.Context(), job.ID, "logout"); err != nil {
				t.Fatal(err)
			}
			if err := m.UpdateTOTPResetResult(job.ID, map[string]any{"logout_all_devices_status": logout}, nil); err != nil {
				t.Fatal(err)
			}
			var operationErr error
			if logout == "unknown" {
				operationErr = errors.New("logout uncertain")
			}
			if err := m.FinishTOTPWorkflow(job.ID, operationErr); err != nil {
				t.Fatal(err)
			}
			restored := NewManager(context.Background(), 1, m.store, nil)
			restored.SetCredentialStore(vault)
			if err := restored.Restore(t.Context()); err != nil {
				t.Fatal(err)
			}
			state, _ := restored.Get(job.ID)
			if state.ResultSaved {
				t.Fatal("logout-only restart exposed prior grant", state)
			}
			if _, err := restored.QueueOperation(job.ID, "relogin", nil); err != nil {
				t.Fatal(err)
			}
			if err := dir.WriteJSON(t.Context(), "sub2api-import-oauth.json", map[string]any{"type": "sub2api-data", "accounts": []any{map[string]any{"credentials": map[string]any{"email": job.Email, "access_token": "fresh-grant"}}}}); err != nil {
				t.Fatal(err)
			}
			restored.UpdateResult(job.ID, true, "fresh grant")
			secondRestore := NewManager(context.Background(), 1, m.store, nil)
			secondRestore.SetCredentialStore(vault)
			if err := secondRestore.Restore(t.Context()); err != nil {
				t.Fatal(err)
			}
			state, _ = secondRestore.Get(job.ID)
			if !state.ResultSaved || state.TOTPWorkflowStatus != "" || state.LogoutAllDevicesAfterTOTP {
				t.Fatal("prior logout invalidated a fresh independent grant", state)
			}
		})
	}
}
