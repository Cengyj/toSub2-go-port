package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"tosub2-go-port/internal/accountlock"
	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/store"
)

func credentialPresentationMetadata(t *testing.T, st *store.Store, id string) map[string]any {
	t.Helper()
	var document struct {
		Jobs []map[string]any `json:"jobs"`
	}
	if err := st.ReadJSON(t.Context(), "job-meta.json", &document); err != nil {
		t.Fatal(err)
	}
	for _, row := range document.Jobs {
		if row["id"] == id {
			return row
		}
	}
	t.Fatal("credential presentation metadata did not contain the account")
	return nil
}

func TestCredentialPresentationTracksLatestSaveWarning(t *testing.T) {
	const origin, email = "https://credential-presentation.synthetic.invalid", "save-warning-presentation@example.test"
	vault := &credentialCommitWarningStore{MemoryStore: credentials.NewMemoryStore()}
	m, st := sharedVaultWriterManager(t, vault, origin)
	job, err := m.Submit(Spec{Email: email, Input: map[string]any{"password": "synthetic-password", "totpSecret": "SYNTHETICSEED"}})
	if err != nil {
		t.Fatal(err)
	}
	const businessError = "synthetic authentication declined"
	m.SetRunner(func(context.Context, *Job, func(JobEvent)) error { return errors.New(businessError) })
	m.Start()
	ended := waitForStatus(t, m, job.ID, StatusFailed)
	if ended.OperationError != businessError {
		t.Fatal("fixture did not retain its independent business outcome")
	}
	beforeWrites := vault.writes
	vault.failure = credentials.ErrUnavailable
	delta := map[string]any{"mailRequestBody": "synthetic-new-private-body"}
	failed, err := m.Update(job.ID, delta)
	if err != nil || failed.LastError == "" || failed.OperationError != businessError {
		t.Fatal("failed credential edit lost its warning or business error", err)
	}
	public := m.PublicAccounts()[0]
	if public.Job.LastError != failed.LastError || public.Job.OperationError != businessError {
		t.Error("public projection did not retain the latest independent outcomes")
	}
	row := credentialPresentationMetadata(t, st, job.ID)
	if row["credential_warning"] != failed.LastError || row["operation_error"] != businessError {
		t.Error("accepted edit did not persist its current save warning independently of the operation result")
	}
	vault.failure = nil
	repaired, err := m.Update(job.ID, delta)
	if err != nil {
		t.Fatal(err)
	}
	if repaired.LastError != "" || repaired.OperationError != businessError {
		t.Error("confirmed explicit save retained the old credential failure or erased the business error")
	}
	row = credentialPresentationMetadata(t, st, job.ID)
	if row["credential_warning"] != "" || row["operation_error"] != businessError {
		t.Error("metadata projection retained a superseded save warning")
	}
	public = m.PublicAccounts()[0]
	if public.Job.LastError != "" || public.Job.OperationError != businessError {
		t.Error("public account retained a superseded save warning")
	}
	saved, err := vault.Load(email)
	if err != nil || saved.MailBody != delta["mailRequestBody"] || saved.Password != "synthetic-password" || vault.writes != beforeWrites+2 {
		t.Error("explicit repair replayed a write or lost an omitted credential", err)
	}
	raw, err := json.Marshal(public)
	if err != nil || strings.Contains(string(raw), "synthetic-new-private-body") || strings.Contains(string(raw), "synthetic-password") {
		t.Error("credential presentation exposed private material", err)
	}
}

func TestCredentialPresentationCommitsDeferredExplicitClear(t *testing.T) {
	const origin, email = "https://credential-clear-presentation.synthetic.invalid", "clear-presentation@example.test"
	vault := &deferredCredentialVault{MemoryStore: credentials.NewMemoryStore()}
	m, st := sharedVaultWriterManager(t, vault, origin)
	ready, complete := make(chan struct{}), make(chan struct{})
	var once sync.Once
	finish := func() { once.Do(func() { close(complete) }) }
	t.Cleanup(finish)
	m.SetRunner(func(ctx context.Context, job *Job, emit func(JobEvent)) error {
		release, err := accountlock.Acquire(origin, job.Email)
		if err != nil {
			return err
		}
		defer release()
		emit(JobEvent{Stage: "password", Status: "awaiting_input"})
		close(ready)
		<-complete
		return errors.New("synthetic independent authentication failure")
	})
	job, err := m.Submit(Spec{Email: email, Input: map[string]any{"password": "synthetic-retained-password", "totpSecret": "SYNTHETICOLDSEED", "proxyUrl": "http://proxy.synthetic.invalid:8080", "mailApiUrl": "https://mail.synthetic.invalid/inbox"}})
	if err != nil {
		t.Fatal(err)
	}
	m.Start()
	waitDeferredSignal(t, ready, "clear fixture waiting for input")
	beforeWrites := vault.writes.Load()
	if !m.InputForStage(job.ID, "password", map[string]any{"totpSecret": "", "proxyUrl": "", "mailApiUrl": ""}) {
		t.Fatal("explicit clear at the compatibility input boundary was rejected")
	}
	finish()
	ended := waitForStatus(t, m, job.ID, StatusFailed)
	saved, err := vault.Load(email)
	if err != nil || saved.TOTPSecret != "" || saved.ProxyURL != "" || saved.MailAPIURL != "" || saved.Password != "synthetic-retained-password" || vault.writes.Load() != beforeWrites+1 {
		t.Fatal("deferred clear did not save its exact delta once", err)
	}
	if ended.HasTOTPKey || ended.ProxyConfigured || ended.MailAPIURL != "" || !ended.HasPassword || ended.LastError != "" {
		t.Error("confirmed clear left stale credential availability flags")
	}
	public := m.PublicAccounts()[0]
	if public.TOTPAvailable || public.AutoEmailOTP || public.Job.HasTOTPKey || public.Job.ProxyConfigured || !public.PasswordAvailable {
		t.Error("public account advertised a cleared credential or discarded an omitted password")
	}
	row := credentialPresentationMetadata(t, st, job.ID)
	if row["has_totp_key"] != false || row["proxy_configured"] != false || row["has_password"] != true || row["credential_warning"] != "" {
		t.Error("metadata projection retained pre-clear credential flags")
	}
}

func TestCredentialPresentationKeepsBusinessFailureWhenDeferredSaveFails(t *testing.T) {
	const origin, email = "https://credential-two-results.synthetic.invalid", "two-results@example.test"
	vault := &deferredCredentialVault{MemoryStore: credentials.NewMemoryStore()}
	m, st := sharedVaultWriterManager(t, vault, origin)
	ready, complete := make(chan struct{}), make(chan struct{})
	var once sync.Once
	finish := func() { once.Do(func() { close(complete) }) }
	t.Cleanup(finish)
	const businessError = "synthetic independently rejected password"
	m.SetRunner(func(ctx context.Context, job *Job, emit func(JobEvent)) error {
		release, err := accountlock.Acquire(origin, job.Email)
		if err != nil {
			return err
		}
		defer release()
		emit(JobEvent{Stage: "password", Status: "awaiting_input"})
		close(ready)
		<-complete
		return errors.New(businessError)
	})
	job, err := m.Submit(Spec{Email: email, Input: map[string]any{"password": "synthetic-verified-password", "mailRequestBody": "synthetic-original-body"}})
	if err != nil {
		t.Fatal(err)
	}
	m.Start()
	waitDeferredSignal(t, ready, "two independent outcomes fixture")
	beforeWrites := vault.writes.Load()
	if !m.InputForStage(job.ID, "password", map[string]any{"mailRequestBody": "synthetic-edited-body"}) {
		t.Fatal("independent mail edit was rejected")
	}
	vault.mu.Lock()
	vault.failure = "save-failure"
	vault.mu.Unlock()
	finish()
	ended := waitForStatus(t, m, job.ID, StatusFailed)
	if ended.OperationError != businessError || ended.OperationStatus != "failed" || ended.LastError == "" || !ended.RecoveryPaused {
		t.Error("deferred cleanup replaced the business failure instead of keeping an independent warning")
	}
	public := m.PublicAccounts()[0]
	row := credentialPresentationMetadata(t, st, job.ID)
	if public.Job.OperationError != businessError || public.Job.LastError != ended.LastError || row["operation_error"] != businessError || row["credential_warning"] != ended.LastError {
		t.Error("public or durable projection lost one of the two independent failures")
	}
	saved, err := vault.Load(email)
	if err != nil || saved.MailBody != "synthetic-original-body" || ended.Input["mailRequestBody"] != "synthetic-edited-body" || vault.writes.Load() != beforeWrites+1 || m.QueuePosition(job.ID) != 0 {
		t.Error("failed cleanup replayed remote work, lost the draft, or overwrote committed credentials", err)
	}
	assertCredentialPresentationRestartOutcome(t, st, vault, job.ID, "failed", businessError)
}

func assertCredentialPresentationRestartOutcome(t *testing.T, st *store.Store, vault credentials.Store, id, status, operationError string) {
	t.Helper()
	restarted := NewManager(t.Context(), 1, st, nil)
	restarted.SetCredentialStore(vault)
	t.Cleanup(func() { _ = restarted.Shutdown(context.Background()) })
	if err := restarted.Restore(t.Context()); err != nil {
		t.Fatal(err)
	}
	current, ok := restarted.Get(id)
	if !ok || current.OperationStatus != status || current.OperationError != operationError || current.LastError == "" || restarted.ActivitySnapshot().Queued != 0 {
		t.Error("restart lost an independent business result/warning or resumed the old intent")
	}
	public := restarted.PublicAccounts()[0]
	if public.Job.OperationStatus != status || public.Job.OperationError != operationError || public.Job.LastError == "" {
		t.Error("restart public projection hid an independent result")
	}
}

func TestCredentialPresentationKeepsTerminalOutcomeOnDeferredFailure(t *testing.T) {
	for _, outcome := range []string{"succeeded", "canceled", "shutdown"} {
		t.Run(outcome, func(t *testing.T) {
			const origin, email = "https://credential-terminal.synthetic.invalid", "terminal-presentation@example.test"
			vault := &deferredCredentialVault{MemoryStore: credentials.NewMemoryStore()}
			m, st := sharedVaultWriterManager(t, vault, origin)
			ready, complete, canceled := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var once sync.Once
			finish := func() { once.Do(func() { close(complete) }) }
			t.Cleanup(finish)
			m.SetRunner(func(ctx context.Context, job *Job, emit func(JobEvent)) error {
				release, err := accountlock.Acquire(origin, job.Email)
				if err != nil {
					return err
				}
				defer release()
				emit(JobEvent{Stage: "password", Status: "awaiting_input"})
				close(ready)
				if outcome != "succeeded" {
					<-ctx.Done()
					close(canceled)
				}
				<-complete
				return ctx.Err()
			})
			job, err := m.Submit(Spec{Email: email, Input: map[string]any{"password": "synthetic-confirmed-password", "mailRequestBody": "synthetic-confirmed-body"}})
			if err != nil {
				t.Fatal(err)
			}
			m.Start()
			waitDeferredSignal(t, ready, "terminal outcome fixture")
			beforeWrites := vault.writes.Load()
			if !m.InputForStage(job.ID, "password", map[string]any{"mailRequestBody": "synthetic-process-only-draft"}) {
				t.Fatal("independent draft was rejected")
			}
			vault.mu.Lock()
			vault.failure = "save-failure"
			vault.mu.Unlock()
			var shutdown <-chan error
			if outcome == "shutdown" {
				result := make(chan error, 1)
				shutdown = result
				go func() {
					ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
					defer cancel()
					result <- m.Shutdown(ctx)
				}()
				waitDeferredSignal(t, canceled, "shutdown cancellation")
			} else if outcome == "canceled" {
				if !m.Cancel(job.ID) {
					t.Fatal("cancel was not accepted")
				}
				waitDeferredSignal(t, canceled, "explicit cancellation")
			}
			finish()
			ended := waitForStatus(t, m, job.ID, StatusFailed)
			wantStatus, wantError, writeCount := outcome, "", beforeWrites+1
			if outcome == "shutdown" {
				wantStatus, wantError, writeCount = "interrupted", "服务关闭，任务已中断，请重新选择要执行的操作", beforeWrites
				if err := <-shutdown; err != nil {
					t.Fatal(err)
				}
			}
			if ended.OperationStatus != wantStatus || ended.OperationError != wantError || ended.LastError == "" || !ended.RecoveryPaused {
				t.Errorf("draft failure replaced the %s business outcome: status=%s error=%s", outcome, ended.OperationStatus, ended.OperationError)
			}
			row := credentialPresentationMetadata(t, st, job.ID)
			public := m.PublicAccounts()[0]
			if row["operation_status"] != wantStatus || row["operation_error"] != wantError || row["credential_warning"] != ended.LastError || public.Job.OperationStatus != wantStatus || public.Job.LastError != ended.LastError {
				t.Error("terminal event or projection overwrote the classified operation outcome")
			}
			saved, err := vault.Load(email)
			if err != nil || saved.MailBody != "synthetic-confirmed-body" || vault.writes.Load() != writeCount || m.QueuePosition(job.ID) != 0 {
				t.Error("draft failure changed committed material or restarted work", err)
			}
			assertCredentialPresentationRestartOutcome(t, st, vault, job.ID, wantStatus, wantError)
		})
	}
}

func TestCredentialPresentationReleaseWarningPersistsWithoutRepeatingWrite(t *testing.T) {
	for _, metadataFails := range []bool{false, true} {
		t.Run(map[bool]string{false: "metadata-confirmed", true: "metadata-failed"}[metadataFails], func(t *testing.T) {
			const origin, email = "https://credential-release-presentation.synthetic.invalid", "release-presentation@example.test"
			vault := &sharedWriterCountingVault{MemoryStore: credentials.NewMemoryStore()}
			m, st := sharedVaultWriterManager(t, vault, origin)
			job, err := m.Submit(Spec{Email: email, ImportOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			var repair func()
			m.SetAccountOperationLease(func(email string) (func() error, error) {
				release, err := accountlock.Acquire(origin, email)
				if err != nil {
					return nil, err
				}
				return func() error {
					if err := release(); err != nil {
						return err
					}
					if metadataFails {
						repair = breakAdmissionMetadata(t, st)
					}
					return errors.New("synthetic secret cleanup detail")
				}, nil
			})
			t.Cleanup(func() {
				if repair != nil {
					repair()
				}
			})
			updated, err := m.Update(job.ID, map[string]any{"password": "synthetic-confirmed-new-password"})
			if err != nil || updated == nil || updated.Status != StatusImported || !strings.HasPrefix(updated.LastError, "ACCOUNT_CREDENTIAL_LOCK_RELEASE:") || vault.saves.Load() != 1 {
				t.Fatal("release failure rejected or replayed the confirmed credential save", err)
			}
			if metadataFails {
				if !strings.Contains(updated.LastError, credentialMetadataWarning) {
					t.Error("failed warning persistence did not retain its independent metadata warning")
				}
				repair()
				repair = nil
				if err := m.Shutdown(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			row := credentialPresentationMetadata(t, st, job.ID)
			public := m.PublicAccounts()[0]
			if row["credential_warning"] != updated.LastError || public.Job.LastError != updated.LastError || vault.saves.Load() != 1 {
				t.Error("warning persistence/confirmation repeated the vault write or lost its warning")
			}
			assertCredentialPresentationRestartOutcome(t, st, vault, job.ID, "", "")
		})
	}
}

func TestCredentialWarningResolutionPreservesIndependentEvidence(t *testing.T) {
	const cleanup = "ACCOUNT_CREDENTIAL_LOCK_RELEASE: 账号编辑锁释放未确认；已保存或已受理的结果保留，请核实，不要重复提交"
	const archivedLog = "完整日志尚未安全归档，请检查本地存储；回收操作已受保护"
	for _, tc := range []struct {
		name, warning, password, want string
	}{
		{"legacy-cleanup", "系统凭据保存失败，登录资料仅保留于内存；" + cleanup, "synthetic-confirmed", cleanup},
		{"coded-cleanup", credentialSaveWarning + "；" + cleanup, "synthetic-confirmed", cleanup},
		{"metadata", credentialSaveWarning + "\n" + credentialMetadataWarning, "synthetic-confirmed", credentialMetadataWarning},
		{"archival", archivedLog, "synthetic-confirmed", archivedLog},
		{"unsaved-other-field", credentialSaveWarning, "synthetic-still-unsaved", credentialSaveWarning},
	} {
		t.Run(tc.name, func(t *testing.T) {
			job := &Job{Input: map[string]any{"password": tc.password, "mailRequestBody": "synthetic-confirmed-body"}, LastError: tc.warning, OperationError: "independent business error"}
			clearResolvedCredentialWriteWarning(job, credentials.Credentials{Password: "synthetic-confirmed", MailBody: "synthetic-confirmed-body"})
			if job.LastError != tc.want || job.OperationError != "independent business error" {
				t.Error("credential readback cleared independent or unresolved evidence")
			}
		})
	}
	t.Run("empty-record-method-default", func(t *testing.T) {
		job := &Job{Input: map[string]any{"password": "", "mailRequestMethod": "GET", "mailRequestHeaders": nil}, LastError: credentialSaveWarning}
		clearResolvedCredentialWriteWarning(job, credentials.Credentials{})
		if job.LastError != "" {
			t.Error("a method-only default disguised a confirmed empty credential record as an unsaved draft")
		}
	})
}

func TestCredentialPresentationNullHeadersClearResolvesPriorWarning(t *testing.T) {
	const origin, email = "https://credential-null-headers.synthetic.invalid", "null-headers@example.test"
	vault := &credentialCommitWarningStore{MemoryStore: credentials.NewMemoryStore()}
	m, st := sharedVaultWriterManager(t, vault, origin)
	job, err := m.Submit(Spec{Email: email, ImportOnly: true, Input: map[string]any{"password": "synthetic-password", "mailRequestHeaders": map[string]string{"X-Synthetic": "previous-value"}}})
	if err != nil {
		t.Fatal(err)
	}
	vault.failure = credentials.ErrUnavailable
	if failed, err := m.Update(job.ID, map[string]any{"mailRequestHeaders": nil}); err != nil || failed.LastError != credentialSaveWarning {
		t.Fatal("null headers fixture did not retain its initial failure", err)
	}
	vault.failure = nil
	writes := vault.writes
	updated, err := m.Update(job.ID, map[string]any{"mailRequestHeaders": nil})
	if err != nil {
		t.Fatal(err)
	}
	saved, err := vault.Load(email)
	if err != nil || len(saved.MailHeaders) != 0 || saved.Password != "synthetic-password" || vault.writes != writes+1 {
		t.Fatal("explicit null did not retain its existing header-clear contract", err)
	}
	if updated.LastError != "" || m.PublicAccounts()[0].Job.LastError != "" || credentialPresentationMetadata(t, st, job.ID)["credential_warning"] != "" {
		t.Error("confirmed null header clear retained the earlier failure warning")
	}
}
