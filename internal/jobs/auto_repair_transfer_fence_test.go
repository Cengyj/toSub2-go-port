package jobs

import (
	"context"
	"errors"
	"testing"

	"tosub2-go-port/internal/store"
)

func TestAutoRepairTransferFencePersistsBeforeRemoteWork(t *testing.T) {
	m, current := repairCommitFixture(t)
	defer m.cancel()
	started, err := m.BeginAutoRepairTransfer(current)
	if err != nil || !started.AutoRepairSyncPaused {
		t.Fatal("transfer not fenced", err)
	}
	if err := m.CheckAutoRepair(current); !errors.Is(err, ErrAutoRepairSuperseded) {
		t.Fatal("old caller retained admission", err)
	}
	if err := m.CheckAutoRepair(started); err != nil {
		t.Fatal("current transfer cannot finish", err)
	}
	if _, err := m.BeginAutoRepairTransfer(started); !errors.Is(err, ErrAutoRepairSuperseded) {
		t.Fatal("paused transfer was re-admitted", err)
	}
	restored := NewManager(t.Context(), 1, m.store, nil)
	defer restored.cancel()
	if err := restored.Restore(t.Context()); err != nil {
		t.Fatal(err)
	}
	readback, ok := restored.Get(current.ID)
	if !ok || !readback.AutoRepairSyncPaused || len(readback.AutoRepairPendingAccountIDs) == 0 {
		t.Fatal("restart lost transfer fence")
	}
	finished, err := m.CommitAutoRepair(started, nil, "", true)
	if err != nil || finished.AutoRepairSyncPaused || len(finished.AutoRepairPendingAccountIDs) != 0 {
		t.Fatal("confirmed completion did not release fence", err)
	}
}

func TestAutoRepairTransferFenceWriteFailureDoesNotAdmit(t *testing.T) {
	for _, failure := range []error{errors.New("fixture write failure"), store.ErrDurabilityUncertain} {
		m, current := repairCommitFixture(t)
		m.metadataWriter = func(context.Context, string, any) error { return failure }
		started, err := m.BeginAutoRepairTransfer(current)
		expected := ErrAdmissionPersistence
		if errors.Is(failure, store.ErrDurabilityUncertain) {
			expected = ErrAdmissionUncertain
		}
		if started != nil || !errors.Is(err, expected) {
			t.Fatal("failed fence authorized transfer", err)
		}
		after, _ := m.Get(current.ID)
		if !after.AutoRepairSyncPaused || len(after.AutoRepairPendingAccountIDs) == 0 {
			t.Fatal("failed fence discarded pending data or allowed process retry")
		}
		m.cancel()
	}
}
