package jobs

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/store"
)

func TestMailObservationLogsClassifyFailureWithoutProviderText(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(t.Context(), 1, st, nil)
	t.Cleanup(func() { _ = m.Shutdown(context.Background()) })
	job, err := m.Submit(Spec{Email: "mail-log@example.invalid"})
	if err != nil {
		t.Fatal(err)
	}
	m.Emit(JobEvent{JobID: job.ID, Stage: "email_otp", Status: StatusWaitingInput, Prompt: "请输入邮箱验证码"})
	for _, fixture := range []struct{ status, level, message, diagnostic string }{
		{"ready", "info", "邮箱自动收码已就绪", ""},
		{"polling", "info", "正在自动读取邮箱验证码", ""},
		{"error", "warning", "邮箱自动收码发生错误", ""},
		{"error", "warning", "邮箱服务暂不可用", "邮箱服务暂不可用"},
		{"timeout", "warning", "邮箱自动收码等待超时", ""},
		{"found", "info", "已读取邮箱验证码", ""},
	} {
		m.Emit(JobEvent{JobID: job.ID, Data: map[string]any{"mailStatus": fixture.status, "mailApiError": fixture.diagnostic}})
		current, _ := m.Get(job.ID)
		last := current.LogEntries[len(current.LogEntries)-1]
		if last.Stage != "mail_status" || last.Status != fixture.status || last.Level != fixture.level || last.Message != fixture.message {
			t.Errorf("%s observation lost its warning or safe description: %+v", fixture.status, last)
		}
		if current.OperationStatus != "waiting" || current.Stage != "email_otp" || current.Prompt != "请输入邮箱验证码" || current.MailStatus != fixture.status {
			t.Fatal("mail observation changed the active input or operation outcome")
		}
	}
	current, _ := m.Get(job.ID)
	if err := m.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	restored := NewManager(t.Context(), 1, st, nil)
	t.Cleanup(func() { _ = restored.Shutdown(context.Background()) })
	if err := restored.Restore(t.Context()); err != nil {
		t.Fatal(err)
	}
	restarted, _ := restored.Get(job.ID)
	if !reflect.DeepEqual(current.LogEntries, restarted.LogEntries) {
		t.Fatal("restart rewrote the historical observation levels or messages")
	}
	page, err := restored.ReadLogPage(t.Context(), restarted, "")
	if err != nil || page.Warning != "" || len(page.Entries) != len(current.LogEntries) {
		t.Fatal("persisted observations were not available from the public log page", err)
	}
}

func TestUnverifiedTOTPSeedLogRequiresAttentionWithoutClaimingCompletion(t *testing.T) {
	m := NewManager(t.Context(), 1, nil, nil)
	t.Cleanup(func() { _ = m.Shutdown(context.Background()) })
	job, err := m.Submit(Spec{Email: "seed-log@example.invalid", Operation: "setup-2fa"})
	if err != nil {
		t.Fatal(err)
	}
	const message = "官方已确认人工验证码激活成功；保存的候选密钥尚未验证"
	m.Emit(JobEvent{JobID: job.ID, Stage: "totp_setup_seed_unverified", Status: StatusRunning, Prompt: message})
	current, _ := m.Get(job.ID)
	last := current.LogEntries[len(current.LogEntries)-1]
	if last.Level != "warning" || last.Message != message || current.OperationStatus != "running" || current.TOTPWorkflowStatus == "succeeded" {
		t.Fatal("unverified candidate seed was logged as ordinary progress or became a completed workflow")
	}
}

func TestCanceledOperationLogRetainsIndependentCredentialWarning(t *testing.T) {
	for _, outcome := range []string{"canceled", "interrupted"} {
		t.Run(outcome, func(t *testing.T) {
			vault := &deferredCredentialVault{MemoryStore: credentials.NewMemoryStore()}
			m, st := sharedVaultWriterManager(t, vault, "https://credential-warning-log.example.invalid")
			ready := make(chan struct{})
			var runs atomic.Int32
			m.SetRunner(func(ctx context.Context, _ *Job, emit func(JobEvent)) error {
				runs.Add(1)
				emit(JobEvent{Stage: "password", Status: "awaiting_input", Prompt: "请输入密码"})
				close(ready)
				<-ctx.Done()
				return ctx.Err()
			})
			job, err := m.Submit(Spec{Email: "credential-log@example.invalid", Input: map[string]any{"mailRequestBody": "synthetic-original-body"}})
			if err != nil {
				t.Fatal(err)
			}
			m.Start()
			waitDeferredSignal(t, ready, "waiting operation")
			beforeWrites := vault.writes.Load()
			if !m.InputForStage(job.ID, "password", map[string]any{"mailRequestBody": "synthetic-private-draft"}) {
				t.Fatal("fixture credential draft was rejected")
			}
			vault.mu.Lock()
			vault.failure = "save-failure"
			vault.mu.Unlock()
			if outcome == "canceled" {
				if !m.Cancel(job.ID) {
					t.Fatal("cancellation was rejected")
				}
			} else if err := m.Shutdown(t.Context()); err != nil {
				t.Fatal(err)
			}
			current := waitDeferredFinished(t, m, job.ID)
			if current.OperationStatus != outcome || !current.RecoveryPaused || !strings.HasPrefix(current.LastError, "ACCOUNT_CREDENTIAL_EDIT_PENDING:") || runs.Load() != 1 || m.QueuePosition(job.ID) != 0 {
				t.Fatal("credential failure changed cancellation, lost the draft protection, or restarted work")
			}
			last := current.LogEntries[len(current.LogEntries)-1]
			if last.Stage != "credential_warning" || last.Level != "warning" || last.Message != current.LastError || last.OperationStatus != outcome {
				t.Errorf("independent credential warning became a generic cancellation: %+v", last)
			}
			terminal := current.LogEntries[len(current.LogEntries)-2]
			if terminal.OperationStatus != outcome || terminal.Level != "warning" || strings.Contains(last.Message, "synthetic-private-draft") {
				t.Fatal("cancellation became an error or the warning disclosed its draft")
			}
			if err := m.Shutdown(t.Context()); err != nil {
				t.Fatal(err)
			}
			restored := NewManager(t.Context(), 1, st, nil)
			restored.SetCredentialStore(vault)
			t.Cleanup(func() { _ = restored.Shutdown(context.Background()) })
			if err := restored.Restore(t.Context()); err != nil {
				t.Fatal(err)
			}
			restarted, _ := restored.Get(job.ID)
			if restarted.OperationStatus != outcome || !reflect.DeepEqual(current.LogEntries, restarted.LogEntries) || runs.Load() != 1 {
				t.Fatal("restart changed the historical outcome or warning evidence")
			}
			wantWrites := beforeWrites
			if outcome == "canceled" {
				wantWrites++
			}
			if vault.writes.Load() != wantWrites {
				t.Fatal("failure or restart replayed a credential write")
			}
		})
	}
}

func TestProfileCompletionLogsKeepMutationOutcomeVisible(t *testing.T) {
	m := NewManager(t.Context(), 1, nil, nil)
	t.Cleanup(func() { _ = m.Shutdown(context.Background()) })
	job, err := m.Submit(Spec{Email: "profile-log@example.invalid"})
	if err != nil {
		t.Fatal(err)
	}
	for _, fixture := range []struct{ outcome, level string }{
		{"required", "info"}, {"submitting", "info"}, {"not_sent", "warning"},
		{"rejected", "warning"}, {"unknown", "warning"}, {"succeeded", "info"},
	} {
		m.Emit(JobEvent{JobID: job.ID, Stage: "profile_" + fixture.outcome, Status: StatusRunning, Prompt: "资料补全状态", Data: map[string]any{
			"profileCompletion": map[string]any{"trigger": "about_you", "source": "generated_adult", "outcome": fixture.outcome},
		}})
		current, _ := m.Get(job.ID)
		last := current.LogEntries[len(current.LogEntries)-1]
		if last.Level != fixture.level || last.Message != "资料补全状态" || current.OperationStatus != "running" {
			t.Errorf("%s profile observation hid its outcome or changed the business state: %+v", fixture.outcome, last)
		}
	}
}

func TestLogArchiveWarningPreservesUnresolvedCredentialWarning(t *testing.T) {
	vault := &deferredCredentialVault{MemoryStore: credentials.NewMemoryStore()}
	m, st := sharedVaultWriterManager(t, vault, "https://archive-warning.example.invalid")
	job, err := m.Submit(Spec{Email: "archive-warning@example.invalid", ImportOnly: true, Input: map[string]any{"mailRequestBody": "synthetic-committed-body"}})
	if err != nil {
		t.Fatal(err)
	}
	vault.mu.Lock()
	vault.failure = "save-failure"
	vault.mu.Unlock()
	job, err = m.Update(job.ID, map[string]any{"mailRequestBody": "synthetic-unsaved-body"})
	if err != nil || job.LastError != credentialSaveWarning {
		t.Fatal("fixture did not retain a failed credential draft", err)
	}
	writes := vault.writes.Load()
	dir := filepath.Join(st.Root, job.ID, "operation-logs")
	if err := os.MkdirAll(filepath.Dir(dir), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir, []byte("synthetic archive blocker"), 0600); err != nil {
		t.Fatal(err)
	}
	for i := range 202 {
		m.Emit(JobEvent{JobID: job.ID, Stage: "synthetic_progress", Prompt: fmt.Sprintf("合成记录 %d", i)})
	}
	current, _ := m.Get(job.ID)
	const archiveWarning = "完整日志尚未安全归档，请检查本地存储；回收操作已受保护"
	if !strings.Contains(current.LastError, credentialSaveWarning) || strings.Count(current.LastError, archiveWarning) != 1 {
		t.Errorf("archive failure overwrote or duplicated independent credential warning: %q", current.LastError)
	}
	if len(current.LogEntries) <= 200 {
		t.Fatal("archive failure truncated unsaved evidence")
	}
	if err := m.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	restored := NewManager(t.Context(), 1, st, nil)
	restored.SetCredentialStore(vault)
	t.Cleanup(func() { _ = restored.Shutdown(context.Background()) })
	if err := restored.Restore(t.Context()); err != nil {
		t.Fatal(err)
	}
	restarted, _ := restored.Get(job.ID)
	if !strings.Contains(restarted.LastError, "ACCOUNT_CREDENTIAL_SAVE_FAILED:") || restarted.RecoveryPaused != current.RecoveryPaused || !reflect.DeepEqual(current.LogEntries, restarted.LogEntries) {
		t.Error("restart lost unresolved credentials, recovery pause or historical evidence")
	}
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	readable, err := restored.ReadLogPage(t.Context(), restarted, "")
	if err != nil || readable.Warning != archiveWarning || len(readable.Entries) != len(current.LogEntries) {
		t.Fatal("readable pending archive hid its warning or copied unrelated credential errors", err)
	}
	restored.Emit(JobEvent{JobID: job.ID, Stage: "synthetic_progress", Prompt: "合成归档恢复"})
	repaired, _ := restored.Get(job.ID)
	if strings.Contains(repaired.LastError, archiveWarning) || !strings.Contains(repaired.LastError, "ACCOUNT_CREDENTIAL_SAVE_FAILED:") || repaired.RecoveryPaused != current.RecoveryPaused || vault.writes.Load() != writes {
		t.Error("archive recovery cleared unresolved credentials or replayed a protected write")
	}
	page, err := restored.ReadLogPage(t.Context(), repaired, "")
	if err != nil || page.Warning != "" || len(page.Entries) != len(current.LogEntries)+1 {
		t.Fatal("archive recovery lost historical records", err)
	}
}
