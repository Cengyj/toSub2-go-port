package jobs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"time"

	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/protocol"
)

// AbortTOTPResetBeforeSend restores a quarantined credential only after the
// protocol proves removal was never dispatched and writes a safe initial
// journal. The expected credential is captured before beginning quarantine.
func (m *Manager) AbortTOTPResetBeforeSend(ctx context.Context, id string, expected credentials.Credentials) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id]
	if !ok {
		return osErrNotFound(id)
	}
	if !resetOperation(j.Operation) || j.TOTPResetPhase != "initial" && j.TOTPResetPhase != "disable_intent" || expected.TOTPResetPending || m.vault == nil || m.store == nil || ctx.Err() != nil {
		return errors.New("无法确认 2FA 重置前的安全凭据，恢复隔离已保留")
	}
	if j.TOTPResetPhase == "initial" && m.totpBlockedLocked(j.Email) {
		// This attempt never established its own account-wide quarantine.
		// Existing flags therefore belong to independent recovery state.
		return ErrTOTPRecoveryRequired
	}
	path := filepath.Join(m.store.Root, id, "totp-setup-result.json")
	if _, err := os.Lstat(path); err != nil {
		return ErrTOTPRecoveryRequired
	}
	if protocol.ValidateTOTPResetStart(path, j.Email) != nil {
		return ErrTOTPRecoveryRequired
	}
	// A safe journal for this request cannot release independent recovery
	// evidence retained by another record of the same account. Recheck before
	// changing either the shared vault or any account-wide quarantine flags.
	if err := m.checkOtherTOTPResultsLocked(j.Email, j.ID); err != nil {
		return err
	}
	current, err := credentials.LoadContext(ctx, m.vault, j.Email)
	quarantined := expected
	quarantined.TOTPResetPending = true
	if err != nil || !reflect.DeepEqual(current, quarantined) && !reflect.DeepEqual(current, expected) {
		return errors.New("2FA 原凭据读回不一致，恢复隔离已保留")
	}
	if err := credentials.SaveContext(ctx, m.vault, j.Email, expected); err != nil {
		return errors.New("无法恢复 2FA 原凭据，恢复隔离已保留")
	}
	readback, err := credentials.LoadContext(ctx, m.vault, j.Email)
	if err != nil || !reflect.DeepEqual(readback, expected) {
		return errors.New("2FA 原凭据恢复后校验失败，恢复隔离已保留")
	}
	for _, related := range m.jobs {
		if emailKey(related.Email) != emailKey(j.Email) {
			continue
		}
		related.TOTPRecoveryPending, related.TOTPCredentialInvalidated = false, false
		related.HasTOTPKey, related.TOTPKnownEnabled = expected.TOTPSecret != "", true
		if related.Input == nil {
			related.Input = map[string]any{}
		}
		if expected.TOTPSecret != "" {
			related.Input["totpSecret"] = expected.TOTPSecret
		}
	}
	j.ResetTOTP, j.TOTPResetStatus, j.TOTPResetPhase = true, "failed", "initial"
	j.TOTPResetError = "尚未发送 2FA 修改请求，可以再次执行"
	j.UpdatedAt = time.Now().UTC()
	m.persistLocked()
	if m.lastPersistError != nil {
		for _, related := range m.jobs {
			if emailKey(related.Email) == emailKey(j.Email) {
				quarantineTOTP(related)
			}
		}
		// Preserve a durable lock if metadata restoration fails. A subsequent
		// restart must not mistake partially persisted state for a clean abort.
		_ = credentials.SaveContext(ctx, m.vault, j.Email, quarantined)
		return errors.New("2FA 原凭据已恢复，但元数据保存失败，恢复隔离已保留")
	}
	return ctx.Err()
}
