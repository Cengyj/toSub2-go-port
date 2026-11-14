package jobs

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"tosub2-go-port/internal/credentials"
)

func TestTrashPurgeSelectionClassifiesChangedRecordsWithoutStartingDeletion(t *testing.T) {
	for _, state := range []string{"restored", "busy", "conflict"} {
		t.Run(state, func(t *testing.T) {
			m, st, vault := trashManager(t)
			const email = "selection@example.test"
			if _, err := m.Submit(Spec{ID: "selection", Email: email, ImportOnly: true}); err != nil {
				t.Fatal(err)
			}
			discoveryFixture(t, st.Root, "selection", email, StatusImported, true, true)
			if err := vault.Save(email, credentials.Credentials{Password: "synthetic-retained-password"}); err != nil {
				t.Fatal(err)
			}
			entry := trashOne(t, m, "selection")
			want := ErrTrashNotFound
			switch state {
			case "restored":
				if _, err := m.RestoreTrash(t.Context(), []string{entry.ID}); err != nil {
					t.Fatal(err)
				}
			case "busy":
				m.mu.Lock()
				m.deleting[email] = true
				m.mu.Unlock()
				want = ErrTrashBusy
			case "conflict":
				m.mu.Lock()
				m.jobs["other-owner"] = &Job{ID: "other-owner", Email: email}
				m.mu.Unlock()
				want = ErrTrashConflict
			}
			before, err := os.ReadFile(filepath.Join(st.Root, "job-meta.json"))
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			m.SetTrashPurgeHook(func(context.Context, *TrashEntry) error { calls++; return nil })
			if count, err := m.PurgeTrash(t.Context(), []string{entry.ID}); count != 0 || !errors.Is(err, want) || calls != 0 {
				t.Fatalf("purge = %d, %v, hooks=%d; want %v before any deletion", count, err, calls, want)
			}
			after, err := os.ReadFile(filepath.Join(st.Root, "job-meta.json"))
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("unaccepted purge changed durable metadata")
			}
			if got, ok := m.GetTrash(entry.ID); ok && (got.State != TrashStateTrashed || got.PurgeStartedAt != nil) {
				t.Fatal("unaccepted purge committed an irreversible marker")
			}
			if _, err := vault.Load(email); err != nil {
				t.Fatal("unaccepted purge deleted credentials")
			}
			if _, err := os.Stat(filepath.Join(st.Root, "selection")); err != nil {
				t.Fatal("unaccepted purge deleted private files")
			}
		})
	}
}

func TestTrashPurgeHookKeepsSafeStageAndPrivateCause(t *testing.T) {
	for _, stage := range []error{ErrTrashBusy, ErrTrashConflict, ErrTrashInsightCleanup, ErrTrashQualityCleanup, ErrTrashSMSCleanup, nil} {
		name := "unclassified"
		if stage != nil {
			name = stage.Error()
		}
		t.Run(name, func(t *testing.T) {
			m, st, vault := trashManager(t)
			const email = "hook-stage@example.test"
			if _, err := m.Submit(Spec{ID: "hook-stage", Email: email, ImportOnly: true}); err != nil {
				t.Fatal(err)
			}
			discoveryFixture(t, st.Root, "hook-stage", email, StatusImported, true, true)
			if err := vault.Save(email, credentials.Credentials{Password: "synthetic-retained-password"}); err != nil {
				t.Fatal(err)
			}
			entry := trashOne(t, m, "hook-stage")
			privateCause := &os.PathError{Op: "remove", Path: "/private/synthetic-account@example.test/token-secret", Err: os.ErrPermission}
			m.SetTrashPurgeHook(func(context.Context, *TrashEntry) error {
				return errors.Join(stage, privateCause)
			})
			want := stage
			if want == nil {
				want = ErrTrashLocalCleanup
			}
			count, err := m.PurgeTrash(t.Context(), []string{entry.ID})
			if count != 0 || !errors.Is(err, want) || !errors.Is(err, os.ErrPermission) || err.Error() != want.Error() {
				t.Fatalf("purge = %d, %v; lost safe stage or private cause", count, err)
			}
			got, ok := m.GetTrash(entry.ID)
			if !ok || got.State != TrashStatePurging || got.LastError != want.Error() {
				t.Fatal("failed cleanup did not retain a safe, retryable diagnostic")
			}
			data, err := os.ReadFile(filepath.Join(st.Root, "job-meta.json"))
			if err != nil || bytes.Contains(data, []byte(privateCause.Path)) {
				t.Fatal("private cleanup path leaked into durable metadata")
			}
			if _, err := vault.Load(email); err != nil {
				t.Fatal("hook failure deleted credentials")
			}
			if _, err := os.Stat(filepath.Join(st.Root, "hook-stage")); err != nil {
				t.Fatal("hook failure deleted private files")
			}
			restarted := NewManager(t.Context(), 1, st, nil)
			if err := restarted.Restore(t.Context()); err != nil {
				t.Fatal(err)
			}
			got, ok = restarted.GetTrash(entry.ID)
			if !ok || got.LastError != want.Error() || got.State != TrashStatePurging {
				t.Fatal("safe cleanup stage or irreversible marker was lost on restart")
			}
		})
	}
}

func TestTrashCleanupFailureRejectsPrivateStageText(t *testing.T) {
	privateStage := errors.New("private stage account@example.test secret-value")
	privateCause := errors.Join(os.ErrPermission, context.DeadlineExceeded)
	err := TrashCleanupFailure(privateStage, privateCause)
	if err.Error() != ErrTrashLocalCleanup.Error() || !errors.Is(err, ErrTrashLocalCleanup) || !errors.Is(err, os.ErrPermission) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("cleanup wrapper exposed private stage or lost cause classification")
	}
}

func TestTrashPurgeInspectionFailureRetainsFilesAndIdentifiesStage(t *testing.T) {
	m, st, _ := trashManager(t)
	if _, err := m.Submit(Spec{ID: "inspect", Email: "inspect@example.test", ImportOnly: true}); err != nil {
		t.Fatal(err)
	}
	discoveryFixture(t, st.Root, "inspect", "inspect@example.test", StatusImported, true, true)
	entry := trashOne(t, m, "inspect")
	// A nonregular historical metadata file must stop inspection, not be
	// skipped in a way that could resurrect this account after final deletion.
	if err := os.MkdirAll(filepath.Join(st.Root, "unindexed", "job-meta.json"), 0700); err != nil {
		t.Fatal(err)
	}
	if count, err := m.PurgeTrash(t.Context(), []string{entry.ID}); count != 0 || !errors.Is(err, ErrTrashInspection) {
		t.Fatalf("purge = %d, %v; want inspection-stage failure", count, err)
	}
	got, ok := m.GetTrash(entry.ID)
	if !ok || got.State != TrashStatePurging || got.LastError != ErrTrashInspection.Error() {
		t.Fatal("inspection failure lost safe retry marker")
	}
	if _, err := os.Stat(filepath.Join(st.Root, "inspect")); err != nil {
		t.Fatal("inspection failure deleted known account files")
	}
}
