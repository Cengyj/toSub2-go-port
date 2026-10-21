package jobs

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/store"
)

func autoRepairPolicyFixture(t *testing.T, plan string) (*Manager, *Job) {
	t.Helper()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(t.Context(), 1, st, nil)
	m.SetCredentialStore(credentials.NewMemoryStore())
	t.Cleanup(func() { _ = m.Shutdown(context.Background()) })
	job, err := m.Submit(Spec{ID: "policy-account", Email: "policy@example.test", ImportOnly: true, Input: map[string]any{"password": "synthetic-password", "authorizationMethod": AuthorizationMethodBPS}})
	if err != nil {
		t.Fatal(err)
	}
	setAutoRepairFixturePlan(t, m, job, plan)
	m.RecordAuthorization(job.ID, true, "synthetic automated authorization", map[string]bool{"password": true})
	m.Emit(JobEvent{JobID: job.ID, Status: StatusSucceeded})
	return m, job
}

func setAutoRepairFixturePlan(t *testing.T, m *Manager, job *Job, plan string) {
	t.Helper()
	private, err := store.New(filepath.Join(m.store.Root, job.ID))
	if err != nil {
		t.Fatal(err)
	}
	if err := private.WriteJSON(t.Context(), "sub2api-import-oauth.json", map[string]any{"type": "sub2api-data", "accounts": []any{map[string]any{"credentials": map[string]any{"email": job.Email, "plan_type": plan, "access_token": "synthetic-token"}}}}); err != nil {
		t.Fatal(err)
	}
	m.UpdateResult(job.ID, true, "")
}

func waitAutoRepairOutcome(t *testing.T, m *Manager, id, status string) *Job {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		job, _ := m.Get(id)
		if job != nil && job.OperationStatus == status {
			return job
		}
		time.Sleep(time.Millisecond)
	}
	job, _ := m.Get(id)
	t.Fatalf("operation did not reach %s: status=%s operation=%s", status, job.Status, job.OperationStatus)
	return nil
}

func TestAutoRepairExecutionRechecksCurrentPolicyAndPreservesAuthorization(t *testing.T) {
	for _, change := range []string{"excluded", "disabled", "backend", "local-plan", "unknown", "owned-policy"} {
		t.Run(change, func(t *testing.T) {
			initialPlan := "free"
			if change == "local-plan" {
				initialPlan = "plus"
			}
			if change == "unknown" {
				initialPlan = "unknown"
			}
			m, job := autoRepairPolicyFixture(t, initialPlan)
			m.SetAutoRepairPolicy(AutoRepairPolicy{Enabled: true, Backend: "fixture-backend"})
			queued, err := m.QueueAutoRepair(job.ID, []int{11, 12}, "fixture-backend", "plus", "Plus", "not-a-plan")
			if err != nil {
				t.Fatal(err)
			}
			policy := AutoRepairPolicy{Enabled: true, Backend: "fixture-backend", ExcludedPlanTypes: []string{"free"}}
			switch change {
			case "disabled":
				policy.Enabled = false
			case "backend":
				policy.Backend = "different-backend"
				policy.ExcludedPlanTypes = nil
			case "local-plan":
				setAutoRepairFixturePlan(t, m, job, "free")
			case "unknown":
				policy.ExcludedPlanTypes = []string{"unknown"}
			}
			m.SetAutoRepairPolicy(policy)
			if change == "owned-policy" {
				policy.ExcludedPlanTypes[0] = "plus"
			}
			exportPath := filepath.Join(m.store.Root, job.ID, "sub2api-import-oauth.json")
			beforeExport, _ := os.ReadFile(exportPath)
			beforeCredentials, _ := m.LoadCredentialsContext(t.Context(), job.ID)
			var runs atomic.Int32
			m.SetRunner(func(context.Context, *Job, func(JobEvent)) error { runs.Add(1); return nil })
			m.Start()
			current := waitAutoRepairOutcome(t, m, job.ID, "skipped")
			if runs.Load() != 0 || current.Status == StatusSucceeded || current.AutoRepairSkippedReason == "" || len(current.AutoRepairPendingAccountIDs) != 0 || current.AutoRepairBackend != "" || current.Attempts != queued.Attempts || !current.ResultSaved || current.AuthorizationMethod != AuthorizationMethodBPS {
				t.Fatal("policy skip executed or discarded historical authorization")
			}
			if current.AutoRepairQueuedPlanType != initialPlan || !reflect.DeepEqual(current.AutoRepairQueuedRemotePlanTypes, []string{"plus", "unknown"}) {
				t.Fatal("admission plan evidence was lost")
			}
			afterExport, _ := os.ReadFile(exportPath)
			afterCredentials, _ := m.LoadCredentialsContext(t.Context(), job.ID)
			if !bytes.Equal(beforeExport, afterExport) || !reflect.DeepEqual(beforeCredentials, afterCredentials) {
				t.Fatal("skip changed authorization files or credentials")
			}
			var saved struct{ Jobs []map[string]any }
			if err := m.store.ReadJSON(t.Context(), "job-meta.json", &saved); err != nil || len(saved.Jobs) != 1 || saved.Jobs[0]["operation_status"] != "skipped" || saved.Jobs[0]["auto_repair_skipped_reason"] == "" {
				t.Fatal("skip was not durably recorded", err)
			}
		})
	}
}

func TestAutoRepairPolicyNeverBlocksManualReloginOrPersistsOldSkip(t *testing.T) {
	m, job := autoRepairPolicyFixture(t, "free")
	m.SetAutoRepairPolicy(AutoRepairPolicy{})
	if _, err := m.QueueAutoRepair(job.ID, []int{11}, "fixture-backend"); err != nil {
		t.Fatal(err)
	}
	var runs atomic.Int32
	m.SetRunner(func(_ context.Context, current *Job, _ func(JobEvent)) error {
		if isAutomaticRepair(current) {
			t.Error("disabled automatic task reached runner")
		}
		runs.Add(1)
		return nil
	})
	m.Start()
	waitAutoRepairOutcome(t, m, job.ID, "skipped")
	if _, err := m.QueueOperation(job.ID, "relogin", nil); err != nil {
		t.Fatal(err)
	}
	current := waitAutoRepairOutcome(t, m, job.ID, "succeeded")
	if runs.Load() != 1 || current.AutoRepairSkippedReason != "" || current.AutoRepairQueuedPlanType != "" || len(current.AutoRepairQueuedRemotePlanTypes) != 0 || !current.ResultSaved {
		t.Fatal("manual action inherited automatic policy or stale skip state")
	}
}

func TestAutoRepairSkipPersistenceFailureNeverRunsOrRestoresOldQueue(t *testing.T) {
	m, job := autoRepairPolicyFixture(t, "free")
	if _, err := m.QueueAutoRepair(job.ID, []int{11}, "fixture-backend"); err != nil {
		t.Fatal(err)
	}
	repair := breakAdmissionMetadata(t, m.store)
	m.SetAutoRepairPolicy(AutoRepairPolicy{Enabled: true, Backend: "fixture-backend", ExcludedPlanTypes: []string{"free"}})
	var runs atomic.Int32
	m.SetRunner(func(context.Context, *Job, func(JobEvent)) error { runs.Add(1); return nil })
	m.Start()
	current := waitAutoRepairOutcome(t, m, job.ID, "skipped")
	if runs.Load() != 0 || !strings.Contains(current.OperationError, "未确认保存") || !current.ResultSaved || len(current.AutoRepairPendingAccountIDs) != 0 {
		t.Fatal("failed skip save admitted work or hid the storage warning")
	}
	if err := m.Shutdown(context.Background()); err == nil {
		t.Fatal("broken metadata write unexpectedly succeeded")
	}
	repair() // Restore the old, durably queued metadata after a definite failed write.
	restored := NewManager(t.Context(), 1, m.store, nil)
	t.Cleanup(func() { _ = restored.Shutdown(context.Background()) })
	restored.SetCredentialStore(m.vault)
	restored.SetAutoRepairPolicy(AutoRepairPolicy{Enabled: true, Backend: "fixture-backend"})
	if err := restored.Restore(t.Context()); err != nil {
		t.Fatal(err)
	}
	recovered, _ := restored.Get(job.ID)
	if recovered.OperationStatus != "skipped" || recovered.Status == StatusQueued || !recovered.ResultSaved || len(recovered.AutoRepairPendingAccountIDs) != 0 {
		t.Fatal("restart resurrected the old automatic queue")
	}
	restored.SetRunner(func(context.Context, *Job, func(JobEvent)) error { runs.Add(1); return nil })
	restored.Start()
	if err := restored.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if runs.Load() != 0 {
		t.Fatal("restored automatic queue executed")
	}
}

func TestAutoRepairQueuedCancellationClearsTransferIntent(t *testing.T) {
	for _, all := range []bool{false, true} {
		t.Run(map[bool]string{false: "cancel", true: "cancel-all"}[all], func(t *testing.T) {
			m, job := autoRepairPolicyFixture(t, "free")
			if _, err := m.QueueAutoRepair(job.ID, []int{11}, "fixture-backend"); err != nil {
				t.Fatal(err)
			}
			if all {
				if m.CancelAll() != 1 {
					t.Fatal("cancel-all missed queued task")
				}
			} else if !m.Cancel(job.ID) {
				t.Fatal("cancel missed queued task")
			}
			current, _ := m.Get(job.ID)
			if current.OperationStatus != "canceled" || current.AutoRepairSkippedReason != "" || !current.ResultSaved || len(current.AutoRepairPendingAccountIDs) != 0 || current.AutoRepairBackend != "" {
				t.Fatal("cancellation left an automatic transfer intent")
			}
		})
	}
}

func TestAutoRepairPolicySecondCheckAndCanceledStartAvoidRunner(t *testing.T) {
	for _, cancelBeforeStart := range []bool{false, true} {
		t.Run(map[bool]string{false: "policy-changed-after-dispatch", true: "canceled-before-runner"}[cancelBeforeStart], func(t *testing.T) {
			m, job := autoRepairPolicyFixture(t, "free")
			queued, err := m.QueueAutoRepair(job.ID, []int{11}, "fixture-backend")
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			m.mu.Lock()
			m.removePendingLocked(job.ID)
			m.active[job.ID], m.emailUse[emailKey(job.Email)] = cancel, job.ID
			m.jobs[job.ID].Status = StatusRunning
			m.jobs[job.ID].Attempts++
			m.mu.Unlock()
			m.SetAutoRepairPolicy(AutoRepairPolicy{})
			if cancelBeforeStart {
				cancel()
			}
			m.SetRunner(func(context.Context, *Job, func(JobEvent)) error {
				t.Error("rejected/canceled start invoked runner")
				return nil
			})
			m.wg.Add(1)
			m.run(ctx, cancel, queued)
			current, _ := m.Get(job.ID)
			want := "skipped"
			if cancelBeforeStart {
				want = "canceled"
			}
			if current.OperationStatus != want || len(m.active) != 0 || len(m.emailUse) != 0 || len(current.AutoRepairPendingAccountIDs) != 0 || !current.ResultSaved {
				t.Fatal("preflight did not release scheduler resources or preserve the correct outcome")
			}
		})
	}
}

func TestAutoRepairTransferPauseIsIdempotentAndPreservesPendingTargets(t *testing.T) {
	m, current := repairCommitFixture(t)
	defer m.cancel()
	before := cloneJob(current)
	paused, err := m.PauseAutoRepairTransfer(current, "当前套餐已排除，等待手动同步或修改规则")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(paused.AutoRepairPendingAccountIDs, before.AutoRepairPendingAccountIDs) || paused.AutoRepairBackend != before.AutoRepairBackend || !sameRepairTime(paused.AutoRepairLastSuccessAt, before.AutoRepairLastSuccessAt) || paused.Status != before.Status || paused.OperationStatus != before.OperationStatus || !paused.ResultSaved {
		t.Fatal("transfer pause changed authorization or pending synchronization")
	}
	repair := breakAdmissionMetadata(t, m.store)
	repeated, err := m.PauseAutoRepairTransfer(paused, paused.AutoRepairSkippedReason)
	if err != nil || !reflect.DeepEqual(paused, repeated) {
		t.Fatal("same-reason pause rewrote metadata or logs", err)
	}
	repair()
	committed, err := m.CommitAutoRepair(paused, []int{12}, "synthetic temporary failure", false)
	if err != nil || committed.AutoRepairSkippedReason != "" {
		t.Fatal("valid transfer attempt retained its old pause reason", err)
	}
}

func TestAutoRepairTransferPauseFailureRollsBackAndRejectsSupersededWork(t *testing.T) {
	m, current := repairCommitFixture(t)
	defer m.cancel()
	repair := breakAdmissionMetadata(t, m.store)
	if result, err := m.PauseAutoRepairTransfer(current, "套餐排除"); result != nil || !errors.Is(err, ErrAdmissionPersistence) {
		t.Fatal("failed pause save returned success", err)
	}
	after, _ := m.Get(current.ID)
	if !reflect.DeepEqual(current, after) {
		t.Fatal("failed pause save mutated account or logs")
	}
	repair()
	paused, err := m.PauseAutoRepairTransfer(current, "套餐排除")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.QueueOperation(current.ID, "relogin", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := m.PauseAutoRepairTransfer(paused, "套餐排除"); !errors.Is(err, ErrAutoRepairSuperseded) {
		t.Fatal("stale pause changed a new manual operation", err)
	}
}

func TestAutoRepairAllowedPolicyAndUnsetPolicyExecute(t *testing.T) {
	for _, setup := range []string{"unset", "allowed-plan", "unknown-is-not-free"} {
		t.Run(setup, func(t *testing.T) {
			plan := "plus"
			if setup == "unknown-is-not-free" {
				plan = "unknown"
			}
			m, job := autoRepairPolicyFixture(t, plan)
			if setup != "unset" {
				m.SetAutoRepairPolicy(AutoRepairPolicy{Enabled: true, Backend: "fixture-backend", ExcludedPlanTypes: []string{"free", "misspelled-tier"}})
			}
			queued, err := m.QueueAutoRepair(job.ID, []int{11}, "fixture-backend", "PLUS", "plus", "unknown")
			if err != nil {
				t.Fatal(err)
			}
			queued.AutoRepairQueuedRemotePlanTypes[0] = "free"
			var runs atomic.Int32
			m.SetRunner(func(_ context.Context, current *Job, _ func(JobEvent)) error {
				if current.AutoRepairQueuedRemotePlanTypes[0] != "plus" {
					t.Error("caller mutated the manager's plan snapshot")
				}
				runs.Add(1)
				return nil
			})
			m.Start()
			current := waitAutoRepairOutcome(t, m, job.ID, "succeeded")
			if runs.Load() != 1 || current.AutoRepairSkippedReason != "" || !current.ResultSaved || len(current.AutoRepairPendingAccountIDs) != 1 {
				t.Fatal("allowed repair did not preserve its pending transfer")
			}
		})
	}
}

func TestAutoRepairTransferPauseAndPlanEvidenceSurviveRestore(t *testing.T) {
	m, job := autoRepairPolicyFixture(t, "plus")
	if _, err := m.QueueAutoRepair(job.ID, []int{11, 12}, "fixture-backend", "PLUS", "plus", "business", "untrusted-plan-value"); err != nil {
		t.Fatal(err)
	}
	m.SetRunner(func(context.Context, *Job, func(JobEvent)) error { return nil })
	m.Start()
	current := waitAutoRepairOutcome(t, m, job.ID, "succeeded")
	paused, err := m.PauseAutoRepairTransfer(current, "当前套餐已排除，暂不回传")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	restored := NewManager(t.Context(), 1, m.store, nil)
	t.Cleanup(func() { _ = restored.Shutdown(context.Background()) })
	restored.SetCredentialStore(m.vault)
	if err := restored.Restore(t.Context()); err != nil {
		t.Fatal(err)
	}
	recovered, _ := restored.Get(job.ID)
	if recovered.AutoRepairSkippedReason != paused.AutoRepairSkippedReason || recovered.Status != StatusSucceeded || recovered.OperationStatus != "succeeded" || !recovered.ResultSaved || !reflect.DeepEqual(recovered.AutoRepairPendingAccountIDs, []int{11, 12}) || recovered.AutoRepairBackend != "fixture-backend" {
		t.Fatal("restart lost the paused transfer or relabeled authorization as skipped")
	}
	if recovered.AutoRepairQueuedPlanType != "plus" || !reflect.DeepEqual(recovered.AutoRepairQueuedRemotePlanTypes, []string{"plus", "team", "unknown"}) {
		t.Fatal("restart lost safe admission plan evidence")
	}
	if strings.Contains(strings.Join(recovered.Logs, " "), "untrusted-plan-value") {
		t.Fatal("raw remote plan value entered operational logs")
	}
}

func TestAutoRepairExecutionPolicyUsesSharedLocalAndRemotePlanRules(t *testing.T) {
	for _, tc := range []struct {
		name, local, remote, excluded, outcome string
	}{
		{"remote-known-over-unknown-fallback", "unknown", "plus", "unknown", "succeeded"},
		{"new-remote-snapshot-exclusion", "unknown", "plus", "plus", "skipped"},
		{"explicit-unknown", "unknown", "unknown", "unknown", "skipped"},
		{"known-local-fallback", "plus", "unknown", "unknown", "succeeded"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, job := autoRepairPolicyFixture(t, tc.local)
			m.SetAutoRepairPolicy(AutoRepairPolicy{Enabled: true, Backend: "fixture-backend"})
			if _, err := m.QueueAutoRepair(job.ID, []int{11}, "fixture-backend", tc.remote); err != nil {
				t.Fatal(err)
			}
			m.SetAutoRepairPolicy(AutoRepairPolicy{Enabled: true, Backend: "fixture-backend", ExcludedPlanTypes: []string{tc.excluded}})
			var runs atomic.Int32
			m.SetRunner(func(context.Context, *Job, func(JobEvent)) error { runs.Add(1); return nil })
			m.Start()
			current := waitAutoRepairOutcome(t, m, job.ID, tc.outcome)
			if tc.outcome == "skipped" {
				if runs.Load() != 0 || len(current.AutoRepairPendingAccountIDs) != 0 || !strings.Contains(current.AutoRepairSkippedReason, "本地或远端") {
					t.Fatal("excluded remote snapshot reached runner or reported the wrong source")
				}
			} else if runs.Load() != 1 || current.AutoRepairSkippedReason != "" {
				t.Fatal("known paid plan was incorrectly treated as unknown")
			}
		})
	}
}

func TestAutoRepairLegacyTargetClearingRemovesPauseReason(t *testing.T) {
	for _, operation := range []string{"update-empty", "update-blocked", "clear", "clear-success"} {
		t.Run(operation, func(t *testing.T) {
			m, current := repairCommitFixture(t)
			defer m.cancel()
			if _, err := m.PauseAutoRepairTransfer(current, "套餐排除，暂停回传"); err != nil {
				t.Fatal(err)
			}
			switch operation {
			case "update-empty":
				m.UpdateAutoRepair(current.ID, nil, "", "", false)
			case "update-blocked":
				m.UpdateAutoRepair(current.ID, []int{11}, "fixture-backend", "account_disabled", true)
			case "clear":
				m.ClearAutoRepair(current.ID, false)
			case "clear-success":
				m.ClearAutoRepair(current.ID, true)
			}
			after, _ := m.Get(current.ID)
			if after.AutoRepairSkippedReason != "" || len(after.AutoRepairPendingAccountIDs) != 0 {
				t.Fatal("cleared transfer retained an obsolete pause reason")
			}
		})
	}
}

func TestAutoRepairRestorePreservesPrivateFinalizationAndPendingTargets(t *testing.T) {
	for _, artifact := range []string{"received-grant", "refresh-grant", "interrupted-checkpoint"} {
		t.Run(artifact, func(t *testing.T) {
			m, job := autoRepairPolicyFixture(t, "plus")
			queued, err := m.QueueAutoRepair(job.ID, []int{11, 12}, "fixture-backend", "plus")
			if err != nil {
				t.Fatal(err)
			}
			private, err := store.New(filepath.Join(m.store.Root, job.ID))
			if err != nil {
				t.Fatal(err)
			}
			name := "login-checkpoint.json"
			document := map[string]any{"version": 1, "email": job.Email, "stage": "tokens_received", "oauth": map[string]any{"tokenResponse": map[string]any{"access_token": "synthetic-new-access", "refresh_token": "synthetic-new-refresh"}}}
			if artifact == "refresh-grant" {
				name = refreshRecoveryFilename
				document = map[string]any{"version": 1, "email": job.Email, "access_token": "synthetic-new-access", "refresh_token": "synthetic-new-refresh"}
			} else if artifact == "interrupted-checkpoint" {
				document = map[string]any{"version": 1, "email": job.Email, "stage": "phone"}
			}
			if err := private.WriteJSON(t.Context(), name, document); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(private.Path(name))
			if err != nil {
				t.Fatal(err)
			}
			if err := m.Shutdown(context.Background()); err != nil {
				t.Fatal(err)
			}
			restored := NewManager(t.Context(), 1, m.store, nil)
			t.Cleanup(func() { _ = restored.Shutdown(context.Background()) })
			vault := &rejectingGrantVault{}
			if artifact == "interrupted-checkpoint" {
				restored.SetCredentialStore(m.vault)
			} else {
				restored.SetCredentialStore(vault)
			}
			restored.SetAutoRepairPolicy(AutoRepairPolicy{})
			if err := restored.Restore(t.Context()); err != nil {
				t.Fatal(err)
			}
			recovered, _ := restored.Get(job.ID)
			if recovered.Status == StatusQueued || recovered.OperationStatus == "skipped" || recovered.AutoRepairSkippedReason != "" || !recovered.ResultSaved || !reflect.DeepEqual(recovered.AutoRepairPendingAccountIDs, queued.AutoRepairPendingAccountIDs) || recovered.AutoRepairBackend != queued.AutoRepairBackend {
				t.Fatal("recovery discarded a saved grant, its pending targets, or its manual recovery route")
			}
			if artifact == "received-grant" && (recovered.Status != "resume_available" || recovered.Operation != "resume") {
				t.Fatal("received grant no longer routes to local finalization")
			}
			if artifact == "refresh-grant" && (recovered.Stage != refreshRecoveryStage || recovered.Operation != "refresh") {
				t.Fatal("refresh result no longer routes to local finalization")
			}
			if vault.calls.Load() != 0 {
				t.Fatal("saved one-time result recovery consulted login credentials")
			}
			after, err := os.ReadFile(private.Path(name))
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("recovery changed the private grant/checkpoint artifact", err)
			}
			if len(restored.pending) != 0 {
				t.Fatal("recovery automatically queued a login")
			}
			if artifact == "interrupted-checkpoint" {
				restored.SetRunner(func(context.Context, *Job, func(JobEvent)) error {
					t.Error("checkpoint recovery started a new login")
					return nil
				})
				restored.Start()
			} else {
				wantOperation := recovered.Operation
				restored.SetRunner(func(_ context.Context, current *Job, _ func(JobEvent)) error {
					if current.Operation != wantOperation || current.LastOperationType != "automatic_relogin" || !reflect.DeepEqual(current.AutoRepairPendingAccountIDs, queued.AutoRepairPendingAccountIDs) || current.AutoRepairBackend != queued.AutoRepairBackend {
						t.Error("explicit finalization requested a fresh login or discarded transfer targets")
					}
					return nil
				})
				finalization, err := restored.Retry(job.ID)
				if err != nil || finalization.Operation != wantOperation {
					t.Fatal("cannot explicitly finish the saved authorization", err)
				}
				restored.Start()
				finished := waitAutoRepairOutcome(t, restored, job.ID, "succeeded")
				if finished.LastOperationType != "automatic_relogin" || !reflect.DeepEqual(finished.AutoRepairPendingAccountIDs, queued.AutoRepairPendingAccountIDs) || finished.AutoRepairBackend != queued.AutoRepairBackend || finished.AutoRepairQueuedPlanType != queued.AutoRepairQueuedPlanType || !reflect.DeepEqual(finished.AutoRepairQueuedRemotePlanTypes, queued.AutoRepairQueuedRemotePlanTypes) {
					t.Fatal("local finalization lost the pending synchronization decision")
				}
			}
			if err := restored.Shutdown(context.Background()); err != nil {
				t.Fatal(err)
			}
		})
	}
}
