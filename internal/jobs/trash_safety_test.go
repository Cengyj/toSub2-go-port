package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/store"
)

func trashOne(t *testing.T, m *Manager, id string) *TrashEntry {
	t.Helper()
	if _, err := m.TrashAccounts(t.Context(), []string{id}); err != nil {
		t.Fatal(err)
	}
	entries := m.ListTrash()
	for _, entry := range entries {
		for _, job := range entry.Jobs {
			if job.ID == id {
				return entry
			}
		}
	}
	t.Fatal("trashed entry not found")
	return nil
}

func blockTrashMetadata(t *testing.T, root string) func() {
	t.Helper()
	path, backup := filepath.Join(root, "job-meta.json"), filepath.Join(root, "backup-metadata.json")
	if err := os.Rename(path, backup); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	return func() {
		t.Helper()
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(backup, path); err != nil {
			t.Fatal(err)
		}
	}
}

func TestTrashPurgeCommitsTombstoneBeforeCleanupAndRetriesAfterRestart(t *testing.T) {
	m, st, vault := trashManager(t)
	email := "purge@example.test"
	if _, err := m.Submit(Spec{ID: "purge", Email: email, ImportOnly: true}); err != nil {
		t.Fatal(err)
	}
	discoveryFixture(t, st.Root, "purge", email, StatusQueued, true, true)
	if err := vault.Save(email, credentials.Credentials{Password: "synthetic-purge-secret"}); err != nil {
		t.Fatal(err)
	}
	entry := trashOne(t, m, "purge")
	calls := 0
	m.SetTrashPurgeHook(func(ctx context.Context, got *TrashEntry) error {
		calls++
		if got.State != TrashStatePurging || got.PurgeStartedAt == nil {
			t.Fatal("hook ran without irreversible marker")
		}
		restarted := NewManager(t.Context(), 1, st, nil)
		if err := restarted.Restore(ctx); err != nil {
			t.Fatal(err)
		}
		persisted, ok := restarted.GetTrash(entry.ID)
		if !ok || persisted.State != TrashStatePurging || len(restarted.List()) != 0 {
			t.Fatal("marker was not durable before cleanup")
		}
		return errors.New("synthetic external local cleanup failure")
	})
	if count, err := m.PurgeTrash(t.Context(), []string{entry.ID}); err == nil || count != 0 || calls != 1 {
		t.Fatalf("purge = %d, %v, calls=%d", count, err, calls)
	}
	if _, err := vault.Load(email); err != nil {
		t.Fatal("credential removed after cleanup hook failure")
	}
	if _, err := os.Stat(filepath.Join(st.Root, "purge")); err != nil {
		t.Fatal("private data removed after hook failure")
	}
	retained, _ := m.GetTrash(entry.ID)
	if retained.State != TrashStatePurging || retained.LastError == "" {
		t.Fatal("failure did not retain retryable purging state")
	}
	if _, err := m.RestoreTrash(t.Context(), []string{entry.ID}); !errors.Is(err, ErrTrashPurging) {
		t.Fatalf("partially purged account restored: %v", err)
	}
	restarted := NewManager(t.Context(), 1, st, nil)
	restarted.SetCredentialStore(vault)
	if err := restarted.Restore(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.RestoreTrash(t.Context(), []string{entry.ID}); !errors.Is(err, ErrTrashPurging) {
		t.Fatal("restart made purge reversible")
	}
	if count, err := restarted.PurgeTrash(t.Context(), []string{entry.ID}); err != nil || count != 1 {
		t.Fatalf("purge retry = %d, %v", count, err)
	}
	if _, err := os.Stat(filepath.Join(st.Root, "purge")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("directory remains: %v", err)
	}
	if _, err := vault.Load(email); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("credential remains: %v", err)
	}
	if len(restarted.RetainedAccountIDs()) != 0 || len(restarted.ListTrash()) != 0 {
		t.Fatal("purged account retained")
	}
	if err := restarted.SyncOutputs(t.Context(), true); err != nil || len(restarted.List()) != 0 {
		t.Fatal("purged artifacts resurrected")
	}
	if _, _, err := restarted.SubmitUniqueEmail(Spec{ID: "fresh", Email: email, ImportOnly: true}); err != nil {
		t.Fatal("purged email cannot be imported anew", err)
	}
}

func TestTrashRestoreAndPurgeRejectFailedMetadataCommit(t *testing.T) {
	for _, operation := range []string{"trash", "restore", "purge"} {
		t.Run(operation, func(t *testing.T) {
			m, st, vault := trashManager(t)
			if _, err := m.Submit(Spec{ID: "account", Email: "failure@example.test", ImportOnly: true}); err != nil {
				t.Fatal(err)
			}
			discoveryFixture(t, st.Root, "account", "failure@example.test", StatusImported, true, false)
			if err := vault.Save("failure@example.test", credentials.Credentials{Password: "retained-synthetic-secret"}); err != nil {
				t.Fatal(err)
			}
			id := "account"
			if operation != "trash" {
				id = trashOne(t, m, id).ID
			}
			unblock := blockTrashMetadata(t, st.Root)
			switch operation {
			case "trash":
				if _, err := m.TrashAccounts(t.Context(), []string{id}); err == nil {
					t.Fatal("trash accepted failed persistence")
				}
				if len(m.List()) != 1 || len(m.ListTrash()) != 0 {
					t.Fatal("failed trash publication changed ownership")
				}
			case "restore":
				if _, err := m.RestoreTrash(t.Context(), []string{id}); !errors.Is(err, ErrTrashPersistence) {
					t.Fatalf("restore error=%v", err)
				}
				if len(m.List()) != 0 || len(m.ListTrash()) != 1 {
					t.Fatal("failed restore publication changed ownership")
				}
			case "purge":
				called := false
				m.SetTrashPurgeHook(func(context.Context, *TrashEntry) error { called = true; return nil })
				if _, err := m.PurgeTrash(t.Context(), []string{id}); !errors.Is(err, ErrTrashPersistence) {
					t.Fatalf("purge error=%v", err)
				}
				got, _ := m.GetTrash(id)
				if called || got.State != TrashStateTrashed {
					t.Fatal("cleanup began before durable tombstone")
				}
			}
			unblock()
			if _, err := os.Stat(filepath.Join(st.Root, "account")); err != nil {
				t.Fatal("private artifacts deleted on commit failure")
			}
			if _, err := vault.Load("failure@example.test"); err != nil {
				t.Fatal("credential deleted on commit failure")
			}
		})
	}
}

func TestTrashPauseHookPrecedesCancellationAndGatesOtherOperations(t *testing.T) {
	m, _, _ := trashManager(t)
	t.Cleanup(func() { _ = m.Shutdown(context.Background()) })
	started := make(chan context.Context, 1)
	released := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-released:
		default:
			close(released)
		}
	})
	m.SetRunner(func(ctx context.Context, _ *Job, _ func(JobEvent)) error {
		started <- ctx
		<-ctx.Done()
		<-released
		return ctx.Err()
	})
	job, err := m.Submit(Spec{ID: "running", Email: "running@example.test"})
	if err != nil {
		t.Fatal(err)
	}
	m.Start()
	running := receiveAttemptContext(t, started)
	entered, finishPause := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-finishPause:
		default:
			close(finishPause)
		}
	})
	m.SetTrashPauseHook(func(ctx context.Context, jobs []*Job) error {
		if len(jobs) != 1 || jobs[0].ID != job.ID || jobs[0].Input != nil {
			t.Error("invalid public pause snapshot")
		}
		// Calls back into Manager: the hook must not be invoked with mu held.
		if _, ok := m.Get(job.ID); !ok {
			t.Error("account hidden before pause")
		}
		close(entered)
		<-finishPause
		return ctx.Err()
	})
	done := make(chan error, 1)
	go func() { _, err := m.TrashAccounts(t.Context(), []string{job.ID}); done <- err }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("pause hook blocked")
	}
	if running.Err() != nil {
		t.Fatal("runner canceled before pause completed")
	}
	if _, err := m.CancelWithError(job.ID); err == nil {
		t.Fatal("concurrent cancel bypassed pause gate")
	}
	if _, err := m.CancelAllWithError(); err == nil {
		t.Fatal("bulk cancel bypassed pause gate")
	}
	if _, err := m.SwitchOperation(t.Context(), job.ID, "setup-2fa", nil); err == nil {
		t.Fatal("switch bypassed pause gate")
	}
	if _, err := m.Update(job.ID, map[string]any{"password": "replacement-synthetic-secret"}); err == nil {
		t.Fatal("credential update bypassed pause gate")
	}
	if _, err := m.LoadCredentialsContext(t.Context(), job.ID); err == nil {
		t.Fatal("credential export bypassed pause gate")
	}
	if m.Input(job.ID, map[string]any{"email_otp": "123456"}) {
		t.Fatal("input bypassed pause gate")
	}
	if m.Delete([]string{job.ID}) != 0 {
		t.Fatal("legacy deletion bypassed pause gate")
	}
	if _, err := m.Submit(Spec{Email: job.Email, ImportOnly: true}); err == nil {
		t.Fatal("admission bypassed pause gate")
	}
	close(finishPause)
	select {
	case <-running.Done():
	case <-time.After(time.Second):
		t.Fatal("accepted trash did not cancel runner")
	}
	select {
	case err := <-done:
		t.Fatalf("trash ended before runner cleanup: %v", err)
	default:
	}
	close(released)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("trash did not wait for runner cleanup")
	}
	if len(m.List()) != 0 || len(m.ListTrash()) != 1 {
		t.Fatal("runner account was not archived")
	}
}

func TestTrashRestoreStaysPausedAcrossRestartAndKeepsPrivateResults(t *testing.T) {
	m, st, vault := trashManager(t)
	job, err := m.Submit(Spec{ID: "paused", Email: "paused@example.test", ImportOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	discoveryFixture(t, st.Root, job.ID, job.Email, StatusQueued, true, true)
	path := filepath.Join(st.Root, job.ID, "password-add-result.json")
	contents := []byte(`{"version":1,"email":"paused@example.test","password":"synthetic-new-password"}`)
	if err := os.WriteFile(path, contents, 0600); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	current := m.jobs[job.ID]
	current.Status, current.Operation, current.LastOperationType, current.ResultSaved = StatusSucceeded, "relogin", "automatic_relogin", true
	current.AutoRepairPendingAccountIDs = []int{42}
	current.AutoRepairBackend = "synthetic-backend"
	m.mu.Unlock()
	entry := trashOne(t, m, job.ID)
	if _, err := m.RestoreTrash(t.Context(), []string{entry.ID}); err != nil {
		t.Fatal(err)
	}
	restored, _ := m.Get(job.ID)
	if !restored.RecoveryPaused || !restored.AutoRepairSyncPaused || len(restored.AutoRepairPendingAccountIDs) != 1 {
		t.Fatal("restore lost historical transfer intent or pause")
	}
	if err := m.CheckAutoRepair(restored); !errors.Is(err, ErrAutoRepairSuperseded) {
		t.Fatalf("old sync intent became executable: %v", err)
	}
	restarted := NewManager(t.Context(), 1, st, nil)
	restarted.SetCredentialStore(vault)
	if err := restarted.Restore(t.Context()); err != nil {
		t.Fatal(err)
	}
	after, _ := restarted.Get(job.ID)
	if !after.RecoveryPaused || !after.AutoRepairSyncPaused || len(restarted.pending) != 0 || len(after.AutoRepairPendingAccountIDs) != 1 {
		t.Fatal("restart replayed or cleared paused history")
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != string(contents) {
		t.Fatal("startup consumed a paused private result")
	}
	if _, err := restarted.QueueOperation(job.ID, "relogin", nil); err != nil {
		t.Fatal(err)
	}
	explicit, _ := restarted.Get(job.ID)
	if explicit.RecoveryPaused || explicit.AutoRepairSyncPaused || restarted.QueuePosition(job.ID) != 1 {
		t.Fatal("new explicit operation did not release pause")
	}
}

func TestTrashMalformedMetadataFailsClosedWithoutReadingCredentials(t *testing.T) {
	m, st, _ := trashManager(t)
	discoveryFixture(t, st.Root, "private", "private@example.test", StatusQueued, true, true)
	if err := st.WriteJSON(t.Context(), "job-meta.json", map[string]any{"version": 2, "jobs": []any{}, "trash": []any{map[string]any{"id": "broken", "email": "private@example.test", "state": "trashed"}}}); err != nil {
		t.Fatal(err)
	}
	if err := m.Restore(t.Context()); err == nil {
		t.Fatal("malformed tombstone was silently discarded")
	}
	if len(m.List()) != 0 || len(m.ListTrash()) != 0 {
		t.Fatal("malformed tombstone leaked active data")
	}
}

func TestTrashPublicCopiesAndMetadataNeverExposeInput(t *testing.T) {
	m, st, _ := trashManager(t)
	job, err := m.Submit(Spec{ID: "secret", Email: "secret@example.test", ImportOnly: true, Input: map[string]any{"password": "SYNTHETIC-PRIVATE-SECRET"}})
	if err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	m.jobs[job.ID].Logs = []string{"password=SYNTHETIC-PRIVATE-SECRET"}
	m.mu.Unlock()
	entry := trashOne(t, m, job.ID)
	if entry.Jobs[0].Input != nil || strings.Contains(strings.Join(entry.Jobs[0].Logs, " "), "SYNTHETIC-PRIVATE-SECRET") {
		t.Fatal("public trash leaked private material")
	}
	entry.Jobs[0].Email = "mutated@example.test"
	got, _ := m.GetTrash(entry.ID)
	if got.Jobs[0].Email != job.Email {
		t.Fatal("public trash copy mutated manager state")
	}
	data, err := os.ReadFile(filepath.Join(st.Root, "job-meta.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "SYNTHETIC-PRIVATE-SECRET") {
		t.Fatal("trash metadata contains secret")
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil || len(raw["trash"]) == 0 {
		t.Fatal("trash omitted from atomic envelope")
	}
}

func TestTrashConcurrentDuplicateMutationPublishesOnce(t *testing.T) {
	m, _, _ := trashManager(t)
	job, err := m.Submit(Spec{ID: "once", Email: "once@example.test", ImportOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	var successes atomic.Int32
	done := make(chan struct{}, 8)
	for range 8 {
		go func() {
			if n, err := m.TrashAccounts(t.Context(), []string{job.ID}); err == nil && n == 1 {
				successes.Add(1)
			}
			done <- struct{}{}
		}()
	}
	for range 8 {
		<-done
	}
	if successes.Load() != 1 || len(m.ListTrash()) != 1 || len(m.List()) != 0 {
		t.Fatal("concurrent trash duplicated or lost account history")
	}
}

func TestTrashPurgeRemovesUnindexedSameEmailArtifactsAndRetainsOtherAccounts(t *testing.T) {
	m, st, _ := trashManager(t)
	if _, err := m.Submit(Spec{ID: "known", Email: "orphan@example.test", ImportOnly: true}); err != nil {
		t.Fatal(err)
	}
	discoveryFixture(t, st.Root, "known", "orphan@example.test", StatusImported, true, false)
	entry := trashOne(t, m, "known")
	discoveryFixture(t, st.Root, "unindexed", "ORPHAN@example.test", StatusQueued, true, true)
	discoveryFixture(t, st.Root, "unrelated", "other@example.test", StatusQueued, true, true)
	if err := m.SyncOutputs(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.Get("unindexed"); ok {
		t.Fatal("same-email discovery resurrected trash")
	}
	m.SetTrashPurgeHook(func(context.Context, *TrashEntry) error { return errors.New("first local cleanup fails") })
	if _, err := m.PurgeTrash(t.Context(), []string{entry.ID}); err == nil {
		t.Fatal("expected cleanup failure")
	}
	retained, _ := m.GetTrash(entry.ID)
	if len(retained.Jobs) != 2 {
		t.Fatal("unindexed private ID was not retained before cleanup")
	}
	m.SetTrashPurgeHook(func(_ context.Context, got *TrashEntry) error {
		ids := map[string]bool{}
		for _, j := range got.Jobs {
			ids[j.ID] = true
		}
		if !ids["known"] || !ids["unindexed"] {
			t.Fatal("external cleanup missed historical job IDs")
		}
		return nil
	})
	if _, err := m.PurgeTrash(t.Context(), []string{entry.ID}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"known", "unindexed"} {
		if _, err := os.Stat(filepath.Join(st.Root, id)); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("same-email private directory survived purge")
		}
	}
	if _, err := os.Stat(filepath.Join(st.Root, "unrelated")); err != nil {
		t.Fatal("unrelated account was deleted")
	}
	if err := m.SyncOutputs(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.Get("unindexed"); ok {
		t.Fatal("purged orphan resurrected")
	}
}

func TestTrashFinalMoveFailureKeepsStoppedAccountAndPrivateData(t *testing.T) {
	m, st, _ := trashManager(t)
	job, err := m.Submit(Spec{ID: "leased", Email: "leased@example.test", ImportOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := m.ReserveAccountQuery(t.Context(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := m.TrashAccounts(t.Context(), []string{job.ID}); done <- err }()
	select {
	case <-lease.Context().Done():
	case <-time.After(time.Second):
		t.Fatal("query was not canceled")
	}
	// Acquiring Manager's lock confirms initial cancellation persistence finished.
	if _, ok := m.Get(job.ID); !ok {
		t.Fatal("account hidden before query cleanup")
	}
	unblock := blockTrashMetadata(t, st.Root)
	lease.Release()
	select {
	case err := <-done:
		if !errors.Is(err, ErrTrashPersistence) {
			t.Fatalf("final move failure = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("trash did not finish")
	}
	unblock()
	got, ok := m.Get(job.ID)
	if !ok || got.RecoveryPaused || len(m.ListTrash()) != 0 {
		t.Fatal("failed final commit published trash")
	}
	if _, err := m.TrashAccounts(t.Context(), []string{job.ID}); err != nil {
		t.Fatal("repaired move could not retry", err)
	}
}

func TestTrashPurgeFinalCommitFailureRetainsIrreversibleRetryMarker(t *testing.T) {
	m, st, vault := trashManager(t)
	email := "final@example.test"
	if _, err := m.Submit(Spec{ID: "final", Email: email, ImportOnly: true}); err != nil {
		t.Fatal(err)
	}
	discoveryFixture(t, st.Root, "final", email, StatusImported, true, true)
	if err := vault.Save(email, credentials.Credentials{Password: "synthetic-final-password"}); err != nil {
		t.Fatal(err)
	}
	entry := trashOne(t, m, "final")
	var unblock func()
	m.SetTrashPurgeHook(func(context.Context, *TrashEntry) error { unblock = blockTrashMetadata(t, st.Root); return nil })
	if n, err := m.PurgeTrash(t.Context(), []string{entry.ID}); n != 0 || !errors.Is(err, ErrTrashPersistence) {
		t.Fatalf("final purge commit = %d, %v", n, err)
	}
	unblock()
	got, _ := m.GetTrash(entry.ID)
	if got.State != TrashStatePurging || got.LastError == "" {
		t.Fatal("final commit failure lost purging marker")
	}
	if _, err := m.RestoreTrash(t.Context(), []string{entry.ID}); !errors.Is(err, ErrTrashPurging) {
		t.Fatal("partially deleted account was restored")
	}
	if _, err := vault.Load(email); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("synthetic purge never reached credential removal")
	}
	restarted := NewManager(t.Context(), 1, st, nil)
	restarted.SetCredentialStore(vault)
	if err := restarted.Restore(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n, err := restarted.PurgeTrash(t.Context(), []string{entry.ID}); n != 1 || err != nil {
		t.Fatalf("final purge retry = %d, %v", n, err)
	}
}

type trashDeleteFailureVault struct {
	credentials.Store
	fail bool
}

func (v *trashDeleteFailureVault) Delete(email string) error {
	if v.fail {
		return errors.New("synthetic secure storage unavailable")
	}
	return v.Store.Delete(email)
}

func TestTrashCredentialCleanupFailureKeepsTombstoneAndCanRetry(t *testing.T) {
	m, st, base := trashManager(t)
	vault := &trashDeleteFailureVault{Store: base, fail: true}
	m.SetCredentialStore(vault)
	email := "vault-failure@example.test"
	if _, err := m.Submit(Spec{ID: "vault-failure", Email: email, ImportOnly: true}); err != nil {
		t.Fatal(err)
	}
	if err := vault.Save(email, credentials.Credentials{Password: "synthetic-protected-password"}); err != nil {
		t.Fatal(err)
	}
	discoveryFixture(t, st.Root, "vault-failure", email, StatusImported, true, true)
	entry := trashOne(t, m, "vault-failure")
	if count, err := m.PurgeTrash(t.Context(), []string{entry.ID}); count != 0 || !errors.Is(err, ErrTrashCredentialCleanup) {
		t.Fatalf("purge=%d,%v", count, err)
	}
	got, _ := m.GetTrash(entry.ID)
	if got.State != TrashStatePurging || got.LastError != ErrTrashCredentialCleanup.Error() {
		t.Fatal("credential failure dropped irreversible marker")
	}
	if _, err := os.Stat(filepath.Join(st.Root, "vault-failure")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("test did not reach partial directory removal")
	}
	if _, err := vault.Load(email); err != nil {
		t.Fatal("failed credential deletion lost protected data")
	}
	restarted := NewManager(t.Context(), 1, st, nil)
	restarted.SetCredentialStore(vault)
	if err := restarted.Restore(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(restarted.List()) != 0 || len(restarted.RetainedAccountIDs()) != 1 {
		t.Fatal("partial purge resurrected account or dropped retained IDs")
	}
	vault.fail = false
	if count, err := restarted.PurgeTrash(t.Context(), []string{entry.ID}); count != 1 || err != nil {
		t.Fatalf("retry=%d,%v", count, err)
	}
}

func TestTrashCanceledWhileWaitingPreservesAccountUntilQueryReleases(t *testing.T) {
	m, _, _ := trashManager(t)
	job, err := m.Submit(Spec{ID: "query-cancel", Email: "query-cancel@example.test", ImportOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := m.ReserveAccountQuery(t.Context(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(lease.Release)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := m.TrashAccounts(ctx, []string{job.ID}); done <- err }()
	select {
	case <-lease.Context().Done():
	case <-time.After(time.Second):
		t.Fatal("query not canceled")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("wait=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("trash ignored cancellation")
	}
	if _, ok := m.Get(job.ID); !ok || len(m.ListTrash()) != 0 {
		t.Fatal("canceled wait moved account while query owned cleanup")
	}
	if lease.Valid() {
		t.Fatal("canceled query retained publication rights")
	}
	lease.Release()
	if _, err := m.TrashAccounts(t.Context(), []string{job.ID}); err != nil {
		t.Fatal("query release did not permit explicit retry", err)
	}
}

func TestTrashDurabilityUncertainKeepsVisibleStateAndNeverStartsCleanup(t *testing.T) {
	for _, phase := range []string{"move", "restore", "purge-marker", "purge-final"} {
		t.Run(phase, func(t *testing.T) {
			m, st, vault := trashManager(t)
			email := "uncertain@example.test"
			if _, err := m.Submit(Spec{ID: "uncertain", Email: email, ImportOnly: true}); err != nil {
				t.Fatal(err)
			}
			discoveryFixture(t, st.Root, "uncertain", email, StatusQueued, true, true)
			if err := vault.Save(email, credentials.Credentials{Password: "synthetic-uncertain-password"}); err != nil {
				t.Fatal(err)
			}
			id := "uncertain"
			if phase != "move" {
				id = trashOne(t, m, id).ID
			}
			hookCalls := 0
			m.SetTrashPurgeHook(func(context.Context, *TrashEntry) error { hookCalls++; return nil })
			// Simulate only the uncertain acknowledgement: the real atomic replacement
			// has completed and is observable, exactly as after directory fsync fails.
			m.metadataWriter = func(ctx context.Context, name string, value any) error {
				if err := st.WriteJSON(ctx, name, value); err != nil {
					return err
				}
				metadata := value.(map[string]any)
				trash := metadata["trash"].([]map[string]any)
				inject := phase == "move" && len(trash) > 0 || phase == "restore" && len(trash) == 0 || phase == "purge-marker" && len(trash) > 0 && trash[0]["state"] == TrashStatePurging || phase == "purge-final" && len(trash) == 0
				if inject {
					return errors.Join(store.ErrDurabilityUncertain, errors.New("synthetic directory sync failure"))
				}
				return nil
			}
			var count int
			var err error
			switch phase {
			case "move":
				count, err = m.TrashAccounts(t.Context(), []string{id})
			case "restore":
				count, err = m.RestoreTrash(t.Context(), []string{id})
			default:
				count, err = m.PurgeTrash(t.Context(), []string{id})
			}
			if !errors.Is(err, ErrTrashUncertain) {
				t.Fatalf("%s returned %d,%v", phase, count, err)
			}
			restarted := NewManager(t.Context(), 1, st, nil)
			restarted.SetCredentialStore(vault)
			if err := restarted.Restore(t.Context()); err != nil {
				t.Fatal(err)
			}
			switch phase {
			case "move":
				if count != 1 || len(m.List()) != 0 || len(m.ListTrash()) != 1 || len(restarted.List()) != 0 || len(restarted.ListTrash()) != 1 {
					t.Fatal("uncertain move resurrected active data")
				}
			case "restore":
				if count != 1 || len(m.List()) != 1 || len(restarted.List()) != 1 || len(restarted.pending) != 0 {
					t.Fatal("uncertain restore published a replayable queue")
				}
				current, _ := restarted.Get("uncertain")
				if !current.RecoveryPaused {
					t.Fatal("uncertain restore lost pause")
				}
			case "purge-marker":
				if hookCalls != 0 || count != 0 {
					t.Fatal("uncertain marker permitted cleanup")
				}
				if _, err := os.Stat(filepath.Join(st.Root, "uncertain")); err != nil {
					t.Fatal("uncertain marker deleted files")
				}
				if _, err := vault.Load(email); err != nil {
					t.Fatal("uncertain marker deleted credentials")
				}
				if _, err := m.RestoreTrash(t.Context(), []string{id}); !errors.Is(err, ErrTrashPurging) {
					t.Fatal("uncertain irreversible marker allowed restore")
				}
			case "purge-final":
				if hookCalls != 1 || count != 0 || len(m.ListTrash()) != 1 || len(restarted.ListTrash()) != 0 || len(restarted.List()) != 0 {
					t.Fatal("final uncertain cleanup lost safe retry state or resurrected disk data")
				}
				m.metadataWriter = nil
				if count, err := m.PurgeTrash(t.Context(), []string{id}); count != 1 || err != nil {
					t.Fatalf("uncertain final cleanup cannot retry: %d,%v", count, err)
				}
			}
		})
	}
}

func TestTrashPurgeIncludesUnindexedTOTPOnlyRecoveryDirectory(t *testing.T) {
	m, st, _ := trashManager(t)
	email := "journal@example.test"
	if _, err := m.Submit(Spec{ID: "journal-known", Email: email, ImportOnly: true}); err != nil {
		t.Fatal(err)
	}
	entry := trashOne(t, m, "journal-known")
	orphan := filepath.Join(st.Root, "journal-only")
	if err := os.Mkdir(orphan, 0700); err != nil {
		t.Fatal(err)
	}
	journal := []byte(`{"version":1,"email":" JOURNAL@Example.Test ","reset_totp_requested":true,"phase":"disable_intent"}`)
	if err := os.WriteFile(filepath.Join(orphan, "totp-setup-result.json"), journal, 0600); err != nil {
		t.Fatal(err)
	}
	hookIDs := map[string]bool{}
	m.SetTrashPurgeHook(func(_ context.Context, got *TrashEntry) error {
		for _, job := range got.Jobs {
			hookIDs[job.ID] = true
		}
		return nil
	})
	if n, err := m.PurgeTrash(t.Context(), []string{entry.ID}); n != 1 || err != nil {
		t.Fatalf("purge=%d,%v", n, err)
	}
	if !hookIDs["journal-only"] {
		t.Error("TOTP-only historical ID omitted from durable tombstone and cleanup hook")
	}
	if _, err := os.Stat(orphan); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("TOTP-only recovery directory survived successful purge: %v", err)
	}
	restarted := NewManager(t.Context(), 1, st, nil)
	if err := restarted.Restore(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(restarted.List()) != 0 || len(restarted.ListTrash()) != 0 {
		t.Fatal("TOTP-only recovery journal resurrected permanently deleted account")
	}
}

func TestTrashExtraDirectoryOwnershipIsRecheckedAtRetentionCommit(t *testing.T) {
	for _, owner := range []string{"active", "other-trash"} {
		t.Run(owner, func(t *testing.T) {
			m, st, _ := trashManager(t)
			email := "original@example.test"
			if _, err := m.Submit(Spec{ID: "original-known", Email: email, ImportOnly: true}); err != nil {
				t.Fatal(err)
			}
			entry := trashOne(t, m, "original-known")
			discoveryFixture(t, st.Root, "discovered-late", email, StatusImported, true, true)
			m.mu.Lock()
			target := m.trash[entry.ID]
			now := time.Now().UTC()
			target.State, target.PurgeStartedAt = TrashStatePurging, &now
			m.deleting[emailKey(email)] = true
			m.persistLocked()
			m.mu.Unlock()
			// Deterministically execute the two phases of PurgeTrash around a new
			// owner admitted in their lock gap, without scheduling-dependent sleeps.
			directories, err := m.trashDirectories(t.Context(), target)
			if err != nil {
				t.Fatal(err)
			}
			newer, err := m.Submit(Spec{ID: "discovered-late", Email: "new-owner@example.test", ImportOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			if owner == "other-trash" {
				trashOne(t, m, newer.ID)
			}
			if err := m.retainPurgeDirectories(target, directories); !errors.Is(err, ErrTrashConflict) {
				t.Fatal("late ownership conflict was committed into the purge tombstone")
			}
			got, _ := m.GetTrash(entry.ID)
			if len(got.Jobs) != 1 || got.Jobs[0].ID != "original-known" {
				t.Fatal("failed ownership recheck changed retained history")
			}
			if owner == "active" {
				if current, ok := m.Get(newer.ID); !ok || current.Email != newer.Email {
					t.Fatal("late active account lost ownership")
				}
			} else if len(m.ListTrash()) != 2 {
				t.Fatal("late trash account lost ownership")
			}
		})
	}
}

func TestTrashPurgeTOTPIdentityDoesNotFollowSymlinksOrClaimOtherAccounts(t *testing.T) {
	for _, kind := range []string{"foreign-email", "not-reset", "symlink-file"} {
		t.Run(kind, func(t *testing.T) {
			m, st, _ := trashManager(t)
			email := "safe-journal@example.test"
			if _, err := m.Submit(Spec{ID: "safe-known", Email: email, ImportOnly: true}); err != nil {
				t.Fatal(err)
			}
			entry := trashOne(t, m, "safe-known")
			candidate := filepath.Join(st.Root, "unclaimed")
			if err := os.Mkdir(candidate, 0700); err != nil {
				t.Fatal(err)
			}
			journal := map[string]any{"version": 1, "operation_scope": "current_session", "email": email, "reset_totp_requested": true, "phase": "disable_intent"}
			if kind == "foreign-email" {
				journal["email"] = "different@example.test"
			}
			if kind == "not-reset" {
				journal["reset_totp_requested"] = false
			}
			data, err := json.Marshal(journal)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(candidate, "totp-setup-result.json")
			external := ""
			if kind == "symlink-file" {
				external = filepath.Join(t.TempDir(), "synthetic-journal.json")
				if err := os.WriteFile(external, data, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(external, path); err != nil {
					t.Skipf("symlink creation unavailable on this platform: %v", err)
				}
			} else if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			m.SetTrashPurgeHook(func(_ context.Context, got *TrashEntry) error {
				for _, job := range got.Jobs {
					if job.ID == "unclaimed" {
						t.Error("unsafe or foreign journal claimed by purge")
					}
				}
				return nil
			})
			if n, err := m.PurgeTrash(t.Context(), []string{entry.ID}); n != 1 || err != nil {
				t.Fatalf("purge=%d,%v", n, err)
			}
			if _, err := os.Lstat(path); err != nil {
				t.Fatal("unclaimed journal changed", err)
			}
			if external != "" {
				if got, err := os.ReadFile(external); err != nil || string(got) != string(data) {
					t.Fatal("symlink target was modified")
				}
			}
		})
	}
}
