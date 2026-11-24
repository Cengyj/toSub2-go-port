package jobs

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"tosub2-go-port/internal/accountlock"
	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/protocol"
	"tosub2-go-port/internal/store"
)

func TestRestoredCredentialWarningDoesNotPromiseLostProcessDraft(t *testing.T) {
	const legacyCleanup = "ACCOUNT_CREDENTIAL_LOCK_RELEASE: synthetic old cleanup warning"
	for _, fixture := range []struct {
		name, warning, code string
		paused              bool
	}{
		{"save-failed", credentialSaveWarning, "ACCOUNT_CREDENTIAL_SAVE_FAILED", false},
		{"save-unknown", credentialUncertainWarning, "ACCOUNT_CREDENTIAL_SAVE_UNCERTAIN", false},
		{"readback-unknown", credentialReadbackWarning, "ACCOUNT_CREDENTIAL_READBACK_UNCONFIRMED", false},
		{"active-draft", credentialEditPendingWarning, "ACCOUNT_CREDENTIAL_EDIT_PENDING", true},
		{"deferred-save-unknown", "ACCOUNT_CREDENTIAL_EDIT_PENDING: " + credentialUncertainWarning + "；未确认保存的草稿仅保留于本次进程，未自动重启", "ACCOUNT_CREDENTIAL_EDIT_PENDING", true},
		{"deferred-save-failed", "ACCOUNT_CREDENTIAL_EDIT_PENDING: " + credentialSaveWarning + "；未确认保存的草稿仅保留于本次进程，未自动重启", "ACCOUNT_CREDENTIAL_EDIT_PENDING", true},
		{"deferred-saved-lock-unknown", "ACCOUNT_CREDENTIAL_EDIT_PENDING: 凭据已经保存，但账号锁释放未确认；未确认保存的草稿仅保留于本次进程，未自动重启", "ACCOUNT_CREDENTIAL_EDIT_PENDING", true},
		{"deferred-saved-mirror-failed", "ACCOUNT_CREDENTIAL_EDIT_PENDING: 凭据已经保存，但当前记录读取失败；未重新提交保存；未确认保存的草稿仅保留于本次进程，未自动重启", "ACCOUNT_CREDENTIAL_EDIT_PENDING", true},
		{"legacy-failed", "系统凭据保存失败，登录资料仅保留于内存", "ACCOUNT_CREDENTIAL_SAVE_FAILED", false},
		{"legacy-unknown", "系统凭据保存结果待核实，当前登录资料已保留，请核对后再操作", "ACCOUNT_CREDENTIAL_SAVE_UNCERTAIN", false},
		{"legacy-combined-cleanup", "系统凭据保存失败，登录资料仅保留于内存；" + legacyCleanup, "ACCOUNT_CREDENTIAL_SAVE_FAILED", false},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			st, err := store.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			const email = "credential-warning-restart@example.invalid"
			const independent = "TASK_RESULT_PERSISTENCE: synthetic independent metadata warning"
			const business = "synthetic independently rejected authentication"
			warning := fixture.warning + "\n" + independent
			if err := st.WriteJSON(t.Context(), "job-meta.json", map[string]any{
				"version": 2, "trash": []any{}, "jobs": []any{map[string]any{
					"id": "credential-warning-restart", "email": email,
					"status": StatusImported, "operation_status": "failed", "operation_error": business,
					"credential_warning": warning, "recovery_paused": fixture.paused,
					"created_at": time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
				}},
			}); err != nil {
				t.Fatal(err)
			}
			vault := credentials.NewMemoryStore()
			committed := credentials.Credentials{Password: "synthetic-previously-committed"}
			if err := vault.Save(email, committed); err != nil {
				t.Fatal(err)
			}
			var runs atomic.Int32
			var normalized string
			for restart := range 2 {
				m := NewManager(t.Context(), 1, st, nil)
				m.SetCredentialStore(vault)
				m.SetRunner(func(context.Context, *Job, func(JobEvent)) error { runs.Add(1); return nil })
				if err := m.Restore(t.Context()); err != nil {
					m.cancel()
					t.Fatal(err)
				}
				job, ok := m.Get("credential-warning-restart")
				if !ok || job.OperationError != business || job.RecoveryPaused != fixture.paused || m.QueuePosition(job.ID) != 0 {
					t.Fatal("restoring a presentation warning changed the independent result or execution authority")
				}
				if !strings.HasPrefix(job.LastError, fixture.code+": ") || !strings.Contains(job.LastError, "上次进程") || !strings.Contains(job.LastError, independent) {
					t.Error("restart did not retain the warning category while describing the previous process draft")
				}
				if strings.Contains(fixture.warning, legacyCleanup) && !strings.Contains(job.LastError, legacyCleanup) {
					t.Error("restoring an old combined warning erased an independent ownership-cleanup failure")
				}
				for _, category := range []string{"ACCOUNT_CREDENTIAL_SAVE_UNCERTAIN", "ACCOUNT_CREDENTIAL_SAVE_FAILED"} {
					if strings.Contains(fixture.warning, category) && !strings.Contains(job.LastError, category) {
						t.Error("restoring a deferred warning erased its distinct write outcome")
					}
				}
				if strings.Contains(fixture.warning, "ACCOUNT_CREDENTIAL_SAVE_UNCERTAIN") && !strings.Contains(job.LastError, "勿自动重试") {
					t.Error("restoring an unknown result lost its prohibition against automatic replay")
				}
				if strings.Contains(fixture.warning, "凭据已经保存，但账号锁释放未确认") && (!strings.Contains(job.LastError, "上次凭据已经保存") || !strings.Contains(job.LastError, "ACCOUNT_CREDENTIAL_LOCK_RELEASE")) {
					t.Error("restart relabeled a confirmed write as an unconfirmed save")
				}
				if strings.Contains(fixture.warning, "凭据已经保存，但当前记录读取失败") && (!strings.Contains(job.LastError, "上次凭据已经保存") || !strings.Contains(job.LastError, "不要重复提交")) {
					t.Error("restart erased a confirmed write after its subsequent mirror read failed")
				}
				for _, stale := range []string{"当前资料已保留", "当前登录资料已保留", "登录资料仅保留于内存", "新资料已保留"} {
					if strings.Contains(job.LastError, stale) {
						t.Error("restart still promises an in-memory draft from the exited process")
					}
				}
				if restart == 0 {
					normalized = job.LastError
				} else if job.LastError != normalized {
					t.Error("a second restore duplicated or changed the historical warning")
				}
				public := m.PublicAccounts()
				if len(public) != 1 || public[0].Job.LastError != job.LastError || public[0].Job.OperationError != business {
					t.Error("public projection lost one of the independent historical results")
				}
				m.Start()
				closeCtx, cancel := context.WithTimeout(t.Context(), time.Second)
				closeErr := m.Shutdown(closeCtx)
				cancel()
				if closeErr != nil || runs.Load() != 0 {
					t.Fatal("warning restoration replayed a task or failed to close", closeErr)
				}
			}
			got, err := vault.Load(email)
			if err != nil || !credentials.EqualReadback(got, committed) {
				t.Fatal("warning restoration changed committed credentials", err)
			}
		})
	}
}

func TestRestoredIssuedGrantKeepsIndependentCredentialWarning(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const id, email = "grant-warning", "grant-warning@example.invalid"
	if err := st.WriteJSON(t.Context(), "job-meta.json", map[string]any{
		"version": 2, "trash": []any{}, "jobs": []any{map[string]any{
			"id": id, "email": email, "status": StatusFailed, "operation": "full",
			"credential_warning": credentialUncertainWarning,
		}},
	}); err != nil {
		t.Fatal(err)
	}
	path := OAuthGrantPath(st.Root, &Job{ID: id})
	if err := protocol.WriteSavedOAuthGrant(path, issuedGrantFixture(email, "chatgpt")); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(t.Context(), 1, st, nil)
	defer m.cancel()
	if err := m.Restore(t.Context()); err != nil {
		t.Fatal(err)
	}
	got, ok := m.Get(id)
	if !ok || got.Status != "resume_available" || got.Operation != "resume" || m.QueuePosition(id) != 0 || OAuthGrantPath(st.Root, got) != path {
		t.Fatal("warning restoration changed the independent issued-grant recovery")
	}
	if !strings.HasPrefix(got.LastError, "ACCOUNT_CREDENTIAL_SAVE_UNCERTAIN: ") || !strings.Contains(got.LastError, "上次进程") {
		t.Error("discovering an issued grant erased the independent unconfirmed credential edit")
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("warning restoration consumed or changed issued tokens", err)
	}
}

func TestTrashRestartDoesNotRestoreCredentialDraftPromise(t *testing.T) {
	m, st, _ := trashManager(t)
	defer m.cancel()
	vault := &credentialCommitWarningStore{MemoryStore: credentials.NewMemoryStore()}
	m.SetCredentialStore(vault)
	job, err := m.Submit(Spec{Email: "trash-draft@example.invalid", ImportOnly: true, Input: map[string]any{"password": "synthetic-committed"}})
	if err != nil {
		t.Fatal(err)
	}
	vault.failure = credentials.ErrUnavailable
	if failed, err := m.Update(job.ID, map[string]any{"password": "synthetic-lost-draft"}); err != nil || failed.LastError != credentialSaveWarning {
		t.Fatal("fixture did not retain the uncommitted credential draft", err)
	}
	if count, err := m.TrashAccounts(t.Context(), []string{job.ID}); err != nil || count != 1 {
		t.Fatal("fixture could not archive the account", err)
	}
	if err := m.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	vault.failure = nil
	writes := vault.writes
	restarted := NewManager(t.Context(), 1, st, nil)
	defer restarted.cancel()
	restarted.SetCredentialStore(vault)
	if err := restarted.Restore(t.Context()); err != nil {
		t.Fatal(err)
	}
	trash := restarted.ListTrash()
	if len(trash) != 1 || len(trash[0].Jobs) != 1 || len(restarted.List()) != 0 {
		t.Fatal("restart changed trash visibility")
	}
	if count, err := restarted.RestoreTrash(t.Context(), []string{trash[0].ID}); err != nil || count != 1 {
		t.Fatal("fixture could not restore account visibility", err)
	}
	restored, ok := restarted.Get(job.ID)
	if !ok || !restored.RecoveryPaused || restarted.QueuePosition(job.ID) != 0 || vault.writes != writes {
		t.Fatal("restoring a warning resumed old execution or wrote credentials")
	}
	if !strings.HasPrefix(restored.LastError, "ACCOUNT_CREDENTIAL_SAVE_FAILED: ") || !strings.Contains(restored.LastError, "上次进程") || strings.Contains(restored.LastError, "登录资料仅保留于内存") {
		t.Error("trash restored a promise that the previous process draft still exists")
	}
	saved, err := vault.Load(job.Email)
	if err != nil || saved.Password != "synthetic-committed" || restored.Input["password"] == "synthetic-lost-draft" {
		t.Fatal("trash startup changed committed credentials or reconstructed an uncommitted draft", err)
	}
}

func TestRestoredDeferredCredentialPromptKeepsProcessBoundary(t *testing.T) {
	for _, archived := range []bool{false, true} {
		t.Run(map[bool]string{false: "active", true: "trash"}[archived], func(t *testing.T) {
			const origin = "https://credential-prompt-restart.synthetic.invalid"
			const email = "credential-prompt-restart@example.invalid"
			vault := &deferredCredentialVault{MemoryStore: credentials.NewMemoryStore()}
			m, st := sharedVaultWriterManager(t, vault, origin)
			ready, complete := make(chan struct{}), make(chan struct{})
			var once sync.Once
			finish := func() { once.Do(func() { close(complete) }) }
			t.Cleanup(finish)
			var runs atomic.Int32
			m.SetRunner(func(ctx context.Context, job *Job, emit func(JobEvent)) error {
				runs.Add(1)
				release, err := accountlock.Acquire(origin, job.Email)
				if err != nil {
					return err
				}
				defer release()
				emit(JobEvent{Stage: "password", Status: "awaiting_input"})
				close(ready)
				<-complete
				return nil
			})
			job, err := m.Submit(Spec{Email: email, Input: map[string]any{
				"password": "synthetic-confirmed-password", "mailRequestBody": "synthetic-confirmed-body",
			}})
			if err != nil {
				t.Fatal(err)
			}
			m.Start()
			waitDeferredSignal(t, ready, "independent successful business operation")
			beforeWrites := vault.writes.Load()
			if !m.InputForStage(job.ID, "password", map[string]any{"mailRequestBody": "synthetic-unpersisted-draft"}) {
				t.Fatal("independent credential edit was rejected")
			}
			vault.mu.Lock()
			vault.failure = "save-failure"
			vault.mu.Unlock()
			finish()
			ended := waitDeferredFinished(t, m, job.ID)
			if ended.OperationStatus != "succeeded" || ended.OperationError != "" || !ended.RecoveryPaused || ended.Status != StatusFailed || ended.Input["mailRequestBody"] != "synthetic-unpersisted-draft" {
				t.Fatal("fixture did not reach succeeded business with failed deferred credential save")
			}
			if vault.writes.Load() != beforeWrites+1 || runs.Load() != 1 || m.QueuePosition(job.ID) != 0 {
				t.Fatal("fixture repeated credential writes or business work")
			}
			history, err := json.Marshal(ended.LogEntries)
			if err != nil || len(ended.LogEntries) == 0 {
				t.Fatal("fixture lacks historical events", err)
			}
			persisted := credentialPresentationMetadata(t, st, job.ID)
			if persisted["prompt"] != ended.Prompt || persisted["recovery_paused"] != true {
				t.Fatal("fixture did not persist the observed prompt and recovery pause")
			}
			if archived {
				if count, err := m.TrashAccounts(t.Context(), []string{job.ID}); err != nil || count != 1 {
					t.Fatal("fixture could not archive the failed credential edit", err)
				}
			}
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			err = m.Shutdown(shutdownCtx)
			cancel()
			if err != nil {
				t.Fatal(err)
			}
			vault.mu.Lock()
			vault.failure = ""
			vault.mu.Unlock()
			var normalizedPrompt string
			for restart := range 2 {
				restarted := NewManager(t.Context(), 1, st, nil)
				restarted.SetCredentialStore(vault)
				restarted.SetRunner(func(context.Context, *Job, func(JobEvent)) error { runs.Add(1); return nil })
				t.Cleanup(func() { _ = restarted.Shutdown(context.Background()) })
				if err := restarted.Restore(t.Context()); err != nil {
					t.Fatal(err)
				}
				if archived && restart == 0 {
					trash := restarted.ListTrash()
					if len(trash) != 1 || len(trash[0].Jobs) != 1 || len(restarted.List()) != 0 {
						t.Fatal("restart changed trash visibility")
					}
					if !strings.Contains(trash[0].Jobs[0].Prompt, "上次进程") || strings.Contains(trash[0].Jobs[0].Prompt, "已保留当前资料") {
						t.Error("retained trash prompt still promises the exited process draft")
					}
					if count, err := restarted.RestoreTrash(t.Context(), []string{trash[0].ID}); err != nil || count != 1 {
						t.Fatal("fixture could not restore account visibility", err)
					}
				}
				current, ok := restarted.Get(job.ID)
				if !ok || current.OperationStatus != "succeeded" || current.OperationError != "" || !current.RecoveryPaused || current.Input["mailRequestBody"] == "synthetic-unpersisted-draft" {
					t.Fatal("restart lost the independent outcome or reconstructed a process-local draft")
				}
				public := restarted.PublicAccounts()
				if len(public) != 1 || public[0].Job.ID != job.ID {
					t.Fatal("unexpected public accounts")
				}
				visible := public[0].Job
				if !strings.HasPrefix(visible.Prompt, "ACCOUNT_CREDENTIAL_EDIT_INCOMPLETE: ") || !strings.Contains(visible.Prompt, "上次进程") || strings.Contains(visible.Prompt, "已保留当前资料") {
					t.Errorf("public restart prompt promises a lost credential draft: %q", visible.Prompt)
				}
				if restart == 0 {
					normalizedPrompt = visible.Prompt
				} else if visible.Prompt != normalizedPrompt {
					t.Error("repeated restore changed the historical prompt")
				}
				persisted = credentialPresentationMetadata(t, st, job.ID)
				if current.Prompt != visible.Prompt || persisted["prompt"] != visible.Prompt {
					t.Error("public and persisted restart prompts disagree")
				}
				restoredHistory, err := json.Marshal(current.LogEntries)
				if err != nil || !bytes.Equal(history, restoredHistory) {
					t.Error("restoring current presentation rewrote historical log entries", err)
				}
				if archived {
					if _, err := restarted.ReadLogPage(t.Context(), current, ""); err != nil {
						t.Error("restoring presentation invalidated retained log integrity", err)
					}
				}
				saved, err := vault.Load(email)
				if err != nil || saved.MailBody != "synthetic-confirmed-body" || vault.writes.Load() != beforeWrites+1 || runs.Load() != 1 || restarted.QueuePosition(job.ID) != 0 {
					t.Fatal("restart changed durable credentials or replayed work", err)
				}
				if err := restarted.Shutdown(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestRestoredCredentialPromptKeepsIndependentBusinessText(t *testing.T) {
	for _, prompt := range []string{
		"synthetic independent business failure",
		"UPSTREAM_FAILURE: business detail mentions ACCOUNT_CREDENTIAL_EDIT_INCOMPLETE",
	} {
		t.Run(prompt, func(t *testing.T) {
			st, err := store.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			const id = "independent-business-prompt"
			const business = "ACCOUNT_CREDENTIAL_EDIT_INCOMPLETE: synthetic independent business evidence"
			history := []LogEntry{{At: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Level: "warning", Message: "ACCOUNT_CREDENTIAL_EDIT_INCOMPLETE: historical event originally retained the process draft"}}
			if err := st.WriteJSON(t.Context(), "job-meta.json", map[string]any{
				"version": 2, "trash": []any{}, "jobs": []any{map[string]any{
					"id": id, "email": "business-prompt@example.invalid", "status": StatusFailed,
					"prompt": prompt, "operation_status": "failed", "operation_error": business,
					"credential_warning": credentialEditPendingWarning, "recovery_paused": true,
					"log_entries": history,
				}},
			}); err != nil {
				t.Fatal(err)
			}
			m := NewManager(t.Context(), 1, st, nil)
			defer m.cancel()
			if err := m.Restore(t.Context()); err != nil {
				t.Fatal(err)
			}
			current, ok := m.Get(id)
			public := m.PublicAccounts()
			if !ok || current.Prompt != prompt || current.OperationError != business || len(public) != 1 || public[0].Job.Prompt != prompt || public[0].Job.OperationError != business {
				t.Fatal("credential presentation conversion rewrote an independent business result")
			}
			before, err := json.Marshal(history)
			if err != nil {
				t.Fatal(err)
			}
			after, err := json.Marshal(current.LogEntries)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("startup presentation conversion rewrote archived diagnostic text", err)
			}
		})
	}
}
