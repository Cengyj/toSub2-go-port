package jobs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

func cpaAutoRepairFixture(t *testing.T) (*Manager, *Job, map[string]any) {
	t.Helper()
	m, job := autoRepairPolicyFixture(t, "plus")
	m.mu.Lock()
	m.jobs[job.ID].AuthorizationMethod = AuthorizationMethodChatGPT
	m.mu.Unlock()
	m.SetCPAAutoRepairPolicy(AutoRepairPolicy{Enabled: true, Backend: "cpa-backend"})
	return m, job, map[string]any{
		"cpaRepairAccountId": "workspace-cpa",
		"cpaConfig":          map[string]any{"baseUrl": "https://cpa.example.test", "adminApiKey": "private-cpa-repair-key", "priority": 3},
	}
}

func TestCPAAutoRepairAdmissionFreezesDestinationAndDoesNotCreateSub2Targets(t *testing.T) {
	m, job, values := cpaAutoRepairFixture(t)
	values["uploadTarget"], values["uploadAfterRelogin"] = UploadTargetSub2API, false
	queued, err := m.QueueCPAAutoRepair(job.ID, "cpa-backend", values, "plus")
	if err != nil {
		t.Fatal(err)
	}
	values["cpaConfig"].(map[string]any)["adminApiKey"] = "changed"
	values["cpaRepairAccountId"] = "changed"
	input, _ := m.InputCopy(job.ID)
	if queued.UploadTarget != UploadTargetCPA || !queued.UploadAfterRelogin || queued.LastOperationType != "automatic_relogin" || len(queued.AutoRepairPendingAccountIDs) != 0 || input["cpaConfig"].(map[string]any)["adminApiKey"] != "private-cpa-repair-key" || input["cpaRepairAccountId"] != "workspace-cpa" {
		t.Fatal("automatic CPA admission did not freeze its destination and credentials")
	}
	data, err := os.ReadFile(filepath.Join(m.store.Root, "job-meta.json"))
	if err != nil || strings.Contains(string(data), "private-cpa-repair-key") || strings.Contains(string(data), "workspace-cpa") {
		t.Fatal("CPA private admission inputs reached metadata", err)
	}
}

func TestCPAAutoRepairExecutionUsesCurrentIndependentPolicy(t *testing.T) {
	for _, scenario := range []string{"enabled", "disabled", "backend", "local-plan", "remote-plan", "unset"} {
		t.Run(scenario, func(t *testing.T) {
			m, job, values := cpaAutoRepairFixture(t)
			m.SetAutoRepairPolicy(AutoRepairPolicy{})
			if _, err := m.QueueCPAAutoRepair(job.ID, "cpa-backend", values, "pro"); err != nil {
				t.Fatal(err)
			}
			policy := AutoRepairPolicy{Enabled: true, Backend: "cpa-backend"}
			switch scenario {
			case "disabled":
				policy.Enabled = false
			case "backend":
				policy.Backend = "changed"
			case "local-plan":
				policy.ExcludedPlanTypes = []string{"plus"}
			case "remote-plan":
				policy.ExcludedPlanTypes = []string{"pro"}
			}
			m.SetCPAAutoRepairPolicy(policy)
			if scenario == "unset" {
				m.cpaAutoRepairPolicy = nil
			}
			var runs atomic.Int32
			m.SetRunner(func(context.Context, *Job, func(JobEvent)) error { runs.Add(1); return nil })
			m.Start()
			want := "skipped"
			if scenario == "enabled" {
				want = "succeeded"
			}
			current := waitAutoRepairOutcome(t, m, job.ID, want)
			if scenario == "enabled" {
				if runs.Load() != 1 {
					t.Fatal("disabled Sub2 policy blocked CPA")
				}
			} else {
				input, _ := m.InputCopy(job.ID)
				if runs.Load() != 0 || current.ReloginUploadStatus != "skipped" || input["cpaConfig"] != nil || current.AutoRepairSkippedReason == "" || !current.ResultSaved {
					t.Fatal("CPA policy did not prevent login or clean up its credentials")
				}
			}
		})
	}
}

func TestCPAAutoRepairAdmissionRejectsBPSBlockedAndUnresolvedUpload(t *testing.T) {
	for _, scenario := range []string{"bps", "blocked", "paused", "missing-identity"} {
		t.Run(scenario, func(t *testing.T) {
			m, job, values := cpaAutoRepairFixture(t)
			m.mu.Lock()
			switch scenario {
			case "bps":
				m.jobs[job.ID].AuthorizationMethod = AuthorizationMethodBPS
			case "blocked":
				m.jobs[job.ID].AutoRepairBlocked = true
			case "paused":
				m.jobs[job.ID].AutoRepairSyncPaused = true
			case "missing-identity":
				delete(values, "cpaRepairAccountId")
			}
			m.mu.Unlock()
			if _, err := m.QueueCPAAutoRepair(job.ID, "cpa-backend", values); err == nil {
				t.Fatal("ineligible CPA automatic operation was admitted")
			}
		})
	}
}

func TestCPAAutoRepairUploadFenceSurvivesRestart(t *testing.T) {
	for _, started := range []bool{false, true} {
		t.Run(map[bool]string{false: "authorized", true: "uploading"}[started], func(t *testing.T) {
			m, job, values := cpaAutoRepairFixture(t)
			queued, err := m.QueueCPAAutoRepair(job.ID, "cpa-backend", values)
			if err != nil {
				t.Fatal(err)
			}
			m.Emit(JobEvent{JobID: job.ID, Status: StatusRunning})
			m.UpdateResult(job.ID, true, "new grant")
			if started {
				if err := m.StartReloginUpload(t.Context(), job.ID); err != nil {
					t.Fatal(err)
				}
				current, _ := m.Get(job.ID)
				if !current.AutoRepairSyncPaused {
					t.Fatal("remote write was not fenced")
				}
			}
			restored := NewManager(t.Context(), 1, m.store, nil)
			restored.SetCredentialStore(m.vault)
			t.Cleanup(func() { _ = restored.Shutdown(context.Background()) })
			if err := restored.Restore(t.Context()); err != nil {
				t.Fatal(err)
			}
			current, _ := restored.Get(job.ID)
			wantUpload := "skipped"
			if started {
				wantUpload = "unknown"
			}
			if current.ReloginUploadStatus != wantUpload || !current.AutoRepairSyncPaused || !current.ResultSaved || current.Status != StatusSucceeded || current.OperationStatus != "succeeded" {
				t.Fatal("restart lost grant or allowed an uncertain CPA upload to replay")
			}
			if _, err := restored.QueueCPAAutoRepair(queued.ID, "cpa-backend", values); err == nil {
				t.Fatal("monitor could readmit an unresolved CPA write")
			}
			if _, err := restored.QueueAutoRepair(queued.ID, []int{11}, "sub2-backend"); err == nil {
				t.Fatal("Sub2 monitor could replace an unresolved CPA operation after restart")
			}
			if err := m.FinishReloginUpload(job.ID, errors.New("uncertain result"), true); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestAutoRepairAdmissionPreservesCrossDestinationRecoveryFences(t *testing.T) {
	for _, destination := range []string{UploadTargetSub2API, UploadTargetCPA} {
		for _, pause := range []string{"sync", "recovery"} {
			t.Run(destination+"/"+pause, func(t *testing.T) {
				m, job, values := cpaAutoRepairFixture(t)
				m.mu.Lock()
				m.jobs[job.ID].AutoRepairSyncPaused = pause == "sync"
				m.jobs[job.ID].RecoveryPaused = pause == "recovery"
				m.mu.Unlock()
				before, _ := m.Get(job.ID)
				var err error
				if destination == UploadTargetCPA {
					_, err = m.QueueCPAAutoRepair(job.ID, "cpa-backend", values)
				} else {
					_, err = m.QueueAutoRepair(job.ID, []int{11}, "sub2-backend")
				}
				after, _ := m.Get(job.ID)
				if err == nil || !reflect.DeepEqual(before, after) {
					t.Fatal("automatic admission bypassed or modified a recovery fence")
				}
			})
		}
	}
	// The transfer already holding a fence can still commit; only a new login
	// admission is refused, so confirmed Sub2 uploads retain their original path.
	m, before := repairCommitFixture(t)
	t.Cleanup(m.cancel)
	started, err := m.BeginAutoRepairTransfer(before)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.QueueAutoRepair(before.ID, []int{11}, before.AutoRepairBackend); err == nil {
		t.Fatal("new Sub2 login replaced an in-flight transfer")
	}
	if _, err := m.CommitAutoRepair(started, nil, "", true); err != nil {
		t.Fatal("existing Sub2 transfer could not finish", err)
	}
}

func TestCPAAutoRepairUploadCheckpointFailureKeepsFence(t *testing.T) {
	m, job, values := cpaAutoRepairFixture(t)
	if _, err := m.QueueCPAAutoRepair(job.ID, "cpa-backend", values); err != nil {
		t.Fatal(err)
	}
	m.UpdateResult(job.ID, true, "new grant saved")
	m.metadataWriter = func(context.Context, string, any) error { return errors.New("synthetic disk failure") }
	if err := m.StartReloginUpload(t.Context(), job.ID); err == nil {
		t.Fatal("failed checkpoint authorized upload")
	}
	current, _ := m.Get(job.ID)
	if !current.AutoRepairSyncPaused || !current.ResultSaved || !current.ReloginAuthorizationSaved {
		t.Fatal("checkpoint failure discarded the grant or pause fence")
	}
}
