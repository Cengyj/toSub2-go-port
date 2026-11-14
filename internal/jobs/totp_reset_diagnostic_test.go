package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTOTPResetFinalDiagnosticPreservesCauseWithoutReleasingQuarantine(t *testing.T) {
	m, vault, job, peer := resetManagerFixture(t)
	if _, err := m.QueueOperation(job.ID, "reset-2fa", nil); err != nil {
		t.Fatal(err)
	}
	const cause = "旧 2FA 移除接口已确认成功，但后续状态复核未通过"
	m.SetRunner(func(ctx context.Context, active *Job, emit func(JobEvent)) error {
		if err := beginTOTPResetFixture(t, m, ctx, active.ID); err != nil {
			return err
		}
		emit(JobEvent{Stage: "totp_reset_disable", Status: StatusRunning})
		return errors.Join(errors.New(cause), m.UpdateTOTPResetResult(active.ID, map[string]any{
			"reset_totp_requested": true, "reset_totp_status": "unknown", "reset_totp_phase": "recovery_required", "reset_totp_error": cause,
		}, errors.New(cause)))
	})
	m.Start()
	t.Cleanup(func() { _ = m.Shutdown(context.Background()) })
	current := waitForStatus(t, m, job.ID, StatusFailed)
	if !strings.HasPrefix(current.Error, ErrTOTPRecoveryPending.Error()) || !strings.Contains(current.Error, cause) || current.OperationError != current.Error {
		t.Fatal("final operation lost the original failure or quarantine explanation")
	}
	if !current.TOTPRecoveryPending || !current.TOTPCredentialInvalidated || current.HasTOTPKey || current.TOTPResetPhase != "recovery_required" {
		t.Fatal("diagnostic update released an uncertain factor")
	}
	if len(current.LogEntries) == 0 || !strings.Contains(current.LogEntries[len(current.LogEntries)-1].Message, cause) {
		t.Fatal("retained final log lost the root cause")
	}
	for _, id := range []string{job.ID, peer.ID} {
		if _, err := m.LoadCredentials(id); !errors.Is(err, ErrTOTPRecoveryRequired) {
			t.Fatal("diagnostic update enabled credential export")
		}
		if _, err := m.QueueOperation(id, "reset-2fa", nil); !errors.Is(err, ErrTOTPRecoveryRequired) {
			t.Fatal("diagnostic update enabled reset replay")
		}
	}
	credential, err := vault.Load(job.Email)
	if err != nil || !credential.TOTPResetPending {
		t.Fatal("diagnostic update changed protected recovery state")
	}
}

func TestTOTPResetFinalDiagnosticRedactsBeforeLoggingAndPersistence(t *testing.T) {
	m, _, job, _ := resetManagerFixture(t)
	if _, err := m.QueueOperation(job.ID, "reset-2fa", nil); err != nil {
		t.Fatal(err)
	}
	const newSecret = "new-factor-canary-private"
	const safeCause = "new factor persistence failed"
	m.SetRunner(func(ctx context.Context, active *Job, emit func(JobEvent)) error {
		if err := beginTOTPResetFixture(t, m, ctx, active.ID); err != nil {
			return err
		}
		failure := errors.New(safeCause + ": " + newSecret + " fixture-password Authorization: Bearer token-canary")
		if err := m.UpdateTOTPResetResult(active.ID, map[string]any{
			"secret": newSecret, "reset_totp_requested": true, "reset_totp_status": "unknown", "reset_totp_phase": "recovery_required", "reset_totp_error": failure.Error(),
		}, failure); err != nil {
			return err
		}
		return failure
	})
	m.Start()
	t.Cleanup(func() { _ = m.Shutdown(context.Background()) })
	current := waitForStatus(t, m, job.ID, StatusFailed)
	if !strings.Contains(current.Error, safeCause) || !strings.Contains(current.Error, "[REDACTED]") {
		t.Fatal("root cause was discarded instead of being safely redacted")
	}
	metadata, err := os.ReadFile(filepath.Join(m.store.Root, "job-meta.json"))
	if err != nil {
		t.Fatal(err)
	}
	public, _ := json.Marshal(SanitizeJobForPublic(current))
	for _, private := range []string{newSecret, "fixture-password", "token-canary"} {
		if strings.Contains(string(metadata), private) || strings.Contains(string(public), private) || strings.Contains(current.Error, private) {
			t.Fatal("private credential escaped final operation diagnostics")
		}
	}
}

func TestTOTPRecoveryDiagnosticFallsBackWithoutDuplicatingGenericWarning(t *testing.T) {
	job := &Job{TOTPResetError: "2FA 重置未完整完成，请手动核实并恢复有效密钥"}
	if got := totpRecoveryDiagnostic(job, ErrTOTPRecoveryPending); got != ErrTOTPRecoveryPending.Error() {
		t.Fatal("generic recovery warning duplicated itself")
	}
	if got := totpRecoveryDiagnostic(job, errors.New("读取 2FA 状态失败：HTTP 503")); !strings.Contains(got, "HTTP 503") {
		t.Fatal("unavailable persisted cause hid the operation failure")
	}
}

func TestTOTPDefiniteRefusalDiagnosticPreservesReasonWithoutOfferingReauthentication(t *testing.T) {
	job := &Job{TOTPRecoveryPending: true, TOTPCredentialInvalidated: true, TOTPResetError: "TOTP_RECENT_AUTH_REQUIRED: 官方拒绝了本次停用请求，要求重新验证身份"}
	got := totpRecoveryDiagnostic(job, errors.New("HTTP 401"))
	if !strings.Contains(got, "官方拒绝了本次停用请求") || strings.Contains(got, "状态尚未确认") {
		t.Fatal("definite refusal diagnosis lost the reason or claimed unknown state")
	}
	if !job.TOTPRecoveryPending || !job.TOTPCredentialInvalidated {
		t.Fatal("read-only diagnosis released recovery protection")
	}
}

func TestTOTPRecoveryDiagnosticRedactsKnownValuesBeforeTruncation(t *testing.T) {
	secret := strings.Repeat("credential-canary-", 6)
	job := &Job{Input: map[string]any{"password": secret}}
	got := totpRecoveryDiagnostic(job, errors.New(strings.Repeat("x", 440)+" "+secret))
	if strings.Contains(got, "credential") || len([]rune(got)) > 512 {
		t.Fatal("truncation exposed a partial known credential or exceeded the public bound")
	}
}

func TestTOTPResetRestoredDiagnosticPreservesPersistedRootCause(t *testing.T) {
	m, vault, job, _ := resetManagerFixture(t)
	if _, err := m.QueueOperation(job.ID, "reset-2fa", nil); err != nil {
		t.Fatal(err)
	}
	const cause = "移除请求结果未知，当前会话内只读核实超时"
	m.SetRunner(func(ctx context.Context, active *Job, emit func(JobEvent)) error {
		if err := beginTOTPResetFixture(t, m, ctx, active.ID); err != nil {
			return err
		}
		emit(JobEvent{Stage: "totp_reset_disable", Status: StatusRunning})
		failure := errors.New(cause)
		return errors.Join(failure, m.UpdateTOTPResetResult(active.ID, map[string]any{
			"reset_totp_requested": true, "reset_totp_status": "unknown", "reset_totp_phase": "recovery_required", "reset_totp_error": cause,
		}, failure))
	})
	m.Start()
	waitForStatus(t, m, job.ID, StatusFailed)
	if err := m.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	restored := NewManager(t.Context(), 1, m.store, nil)
	restored.SetCredentialStore(vault)
	t.Cleanup(func() { _ = restored.Shutdown(context.Background()) })
	if err := restored.Restore(t.Context()); err != nil {
		t.Fatal(err)
	}
	current, ok := restored.Get(job.ID)
	if !ok || !strings.Contains(current.Error, cause) || current.OperationError != current.Error || strings.Count(current.Error, ErrTOTPRecoveryPending.Error()) != 1 {
		t.Fatal("restart hid or duplicated the persisted failure diagnostic")
	}
	if current.Status != StatusFailed || !current.TOTPRecoveryPending || !current.TOTPCredentialInvalidated || len(restored.pending) != 0 {
		t.Fatal("diagnostic restoration released quarantine or queued another mutation")
	}
	if len(current.LogEntries) == 0 || !strings.Contains(current.LogEntries[len(current.LogEntries)-1].Message, cause) {
		t.Fatal("restart lost the retained failure log")
	}
	if _, err := restored.LoadCredentials(job.ID); !errors.Is(err, ErrTOTPRecoveryRequired) {
		t.Fatal("diagnostic restoration enabled credential export")
	}
	if _, err := restored.QueueOperation(job.ID, "reset-2fa", nil); !errors.Is(err, ErrTOTPRecoveryRequired) {
		t.Fatal("diagnostic restoration enabled reset replay")
	}
}
