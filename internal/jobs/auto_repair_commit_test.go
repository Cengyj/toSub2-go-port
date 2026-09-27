package jobs

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"tosub2-go-port/internal/store"
)

func repairCommitFixture(t *testing.T) (*Manager, *Job) {
	t.Helper()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	manager := NewManager(context.Background(), 1, st, nil)
	job, err := manager.Submit(Spec{ID: "repair", Email: "owner@example.test"})
	if err != nil {
		t.Fatal(err)
	}
	manager.Cancel(job.ID)
	manager.Emit(JobEvent{JobID: job.ID, Status: StatusSucceeded})
	manager.UpdateResult(job.ID, true, "")
	manager.UpdateAutoRepair(job.ID, []int{11, 12}, "fixture-backend", "", false)
	current, _ := manager.Get(job.ID)
	return manager, current
}

func TestAutoRepairCommitRejectsSupersededAuthorizationAndTargets(t *testing.T) {
	for _, change := range []string{"manual-relogin", "permanent-failure", "changed-targets", "same-target-new-attempt", "completed-new-operation", "newest-email-task"} {
		t.Run(change, func(t *testing.T) {
			manager, before := repairCommitFixture(t)
			switch change {
			case "manual-relogin":
				if _, err := manager.QueueOperation(before.ID, "relogin", nil); err != nil {
					t.Fatal(err)
				}
			case "permanent-failure":
				manager.UpdateAutoRepair(before.ID, nil, "", "account_disabled", true)
			case "changed-targets":
				manager.UpdateAutoRepair(before.ID, []int{99}, before.AutoRepairBackend, "", false)
			case "same-target-new-attempt":
				manager.UpdateAutoRepair(before.ID, before.AutoRepairPendingAccountIDs, before.AutoRepairBackend, "", false)
			case "completed-new-operation":
				if _, err := manager.QueueOperation(before.ID, "reauthorize", nil); err != nil {
					t.Fatal(err)
				}
				manager.Cancel(before.ID)
				manager.Emit(JobEvent{JobID: before.ID, Status: StatusSucceeded})
			case "newest-email-task":
				if _, err := manager.Submit(Spec{ID: "newer", Email: "OWNER@example.test"}); err != nil {
					t.Fatal(err)
				}
			}
			changed, _ := manager.Get(before.ID)
			if err := manager.CheckAutoRepair(before); !errors.Is(err, ErrAutoRepairSuperseded) {
				t.Fatal("obsolete authorization passed its remote-step guard", err)
			}
			if _, err := manager.CommitAutoRepair(before, nil, "", true); !errors.Is(err, ErrAutoRepairSuperseded) {
				t.Fatal("obsolete transfer committed over a new operation", err)
			}
			after, _ := manager.Get(before.ID)
			if !reflect.DeepEqual(changed, after) {
				t.Fatal("rejected transfer mutated current account state")
			}
		})
	}
}

func TestAutoRepairCommitPreservesPartialProgressAcrossUnrelatedEvents(t *testing.T) {
	manager, original := repairCommitFixture(t)
	manager.Emit(JobEvent{JobID: original.ID, Stage: "remote_transfer_progress"})
	partial, err := manager.CommitAutoRepair(original, []int{12}, "Sub2API returned HTTP 503", false)
	if err != nil {
		t.Fatal("an unrelated event invalidated a current authorization", err)
	}
	if !reflect.DeepEqual(partial.AutoRepairPendingAccountIDs, []int{12}) || partial.AutoRepairLastError == "" || partial.AutoRepairLastSuccessAt != nil || !partial.LastOperationAt.Equal(original.LastOperationAt) {
		t.Fatal("partial transfer did not retain its remaining target and operation time")
	}
	if _, err := manager.CommitAutoRepair(original, nil, "", true); !errors.Is(err, ErrAutoRepairSuperseded) {
		t.Fatal("an already-consumed snapshot was reused", err)
	}
	completed, err := manager.CommitAutoRepair(partial, nil, "", true)
	if err != nil || len(completed.AutoRepairPendingAccountIDs) != 0 || completed.AutoRepairBackend != "" || completed.AutoRepairLastError != "" || completed.AutoRepairLastSuccessAt == nil {
		t.Fatal("completed transfer state is incomplete", err)
	}
	var metadata struct{ Jobs []map[string]any }
	if err := manager.store.ReadJSON(context.Background(), "job-meta.json", &metadata); err != nil {
		t.Fatal(err)
	}
	if len(metadata.Jobs) != 1 || metadata.Jobs[0]["auto_repair_last_success_at"] == nil {
		t.Fatal("completed transfer state was not persisted")
	}
}

func TestAutoRepairCommitCannotIntroduceUnrelatedOrDuplicateTargets(t *testing.T) {
	for _, pending := range [][]int{{99}, {11, 11}} {
		manager, current := repairCommitFixture(t)
		if _, err := manager.CommitAutoRepair(current, pending, "", false); err == nil {
			t.Fatal("transfer introduced a target absent from its original operation")
		}
		after, _ := manager.Get(current.ID)
		if !reflect.DeepEqual(after, current) {
			t.Fatal("invalid target list changed repair state")
		}
	}
}
