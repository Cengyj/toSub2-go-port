package jobs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/logging"
	"tosub2-go-port/internal/protocol"
)

var ErrTOTPRecoveryPending = errors.New("2FA 重置状态尚未确认，已暂停登录、重置和资料导出，请先恢复有效密钥")
var ErrTOTPRecoveryRequired = ErrTOTPRecoveryPending

func resetOperation(operation string) bool { return operation == "reset-2fa" }

func (m *Manager) totpBlockedLocked(email string) bool {
	for _, j := range m.jobs {
		if emailKey(j.Email) == emailKey(email) && (j.TOTPRecoveryPending || j.TOTPCredentialInvalidated) {
			return true
		}
	}
	return false
}

func (m *Manager) checkTOTPActionLocked(email string, localCandidates ...*Job) error {
	if m.totpBlockedLocked(email) {
		return ErrTOTPRecoveryRequired
	}
	for _, j := range m.jobs {
		if emailKey(j.Email) == emailKey(email) && resetOperation(j.Operation) && (containsPending(m.pending, j.ID) || m.active[j.ID] != nil) {
			return ErrTOTPRecoveryRequired
		}
	}
	// Saving an already accepted grant does not authenticate or use a TOTP
	// seed. Keep this local recovery independent of a locked credential vault.
	for _, j := range localCandidates {
		if j == nil || m.store == nil || !safeID(j.ID) {
			continue
		}
		if m.pendingRefreshOperation(j) != "" {
			return nil
		}
		switch j.Operation {
		case "", "login", "full", "resume":
			if checkpointStage(m.store.Root, j.ID) == "tokens_received" {
				return nil
			}
		}
	}
	return m.checkTOTPQuarantineLocked(email)
}

func (m *Manager) checkTOTPQuarantineLocked(email string) error {
	if m.totpBlockedLocked(email) {
		return ErrTOTPRecoveryPending
	}
	if m.vault != nil {
		c, err := credentials.LoadContext(m.rootCtx, m.vault, email)
		if err == nil && c.TOTPResetPending {
			return ErrTOTPRecoveryPending
		}
	}
	return nil
}

// A previous run's journal may be the only copy of a verified new factor.
// A strictly recognized pre-mutation journal carries no changed credential;
// other private evidence must block another factor replacement.
func (m *Manager) checkOtherTOTPResultsLocked(email, currentID string) error {
	if m.store == nil {
		return nil
	}
	for _, j := range m.jobs {
		if j.ID == currentID || emailKey(j.Email) != emailKey(email) {
			continue
		}
		path := filepath.Join(m.store.Root, j.ID, "totp-setup-result.json")
		_, err := os.Lstat(path)
		if err == nil && protocol.ValidateTOTPResetStart(path, email) == nil {
			continue
		}
		if err == nil || !errors.Is(err, os.ErrNotExist) {
			return ErrTOTPRecoveryRequired
		}
	}
	return nil
}

func quarantineTOTP(j *Job) {
	j.TOTPRecoveryPending, j.TOTPCredentialInvalidated = true, true
	j.HasTOTPKey, j.TOTPKnownEnabled = false, false
	j.TOTPSetupSecret, j.TOTPSetupURI = "", ""
	delete(j.Input, "totpSecret")
	delete(j.Input, "totp")
	delete(j.Input, "totpCode")
}

// BeginTOTPReset is the durable acknowledgement required before the protocol
// sends any factor removal. A failed acknowledgement must abort that request.
func (m *Manager) BeginTOTPReset(ctx context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id]
	if !ok {
		return osErrNotFound(id)
	}
	if !resetOperation(j.Operation) || m.closing || ctx.Err() != nil {
		return errors.New("2FA reset operation is no longer active")
	}
	if m.store == nil || m.vault == nil {
		return errors.New("2FA 重置需要可靠的元数据和安全凭据存储")
	}
	if _, memoryOnly := m.vault.(*credentials.MemoryStore); memoryOnly {
		return errors.New("2FA 重置需要持久安全凭据存储，当前仅支持内存保存")
	}
	if err := m.checkTOTPQuarantineLocked(j.Email); err != nil {
		return err
	}
	if err := m.checkOtherTOTPResultsLocked(j.Email, j.ID); err != nil {
		return err
	}
	c, err := credentials.LoadContext(ctx, m.vault, j.Email)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.New("无法读取系统凭据，已停止 2FA 重置")
	}
	c.TOTPResetPending = true
	if err := credentials.SaveContext(ctx, m.vault, j.Email, c); err != nil {
		return errors.New("无法安全保存 2FA 重置意图，已停止后续请求")
	}
	// Mark memory immediately after a write, including on uncertain readback.
	// This prevents a concurrent retry from using the previous secret.
	for _, related := range m.jobs {
		if emailKey(related.Email) != emailKey(j.Email) {
			continue
		}
		quarantineTOTP(related)
		if related.ID != id && related.Status == StatusQueued {
			m.removePendingLocked(related.ID)
			related.Status, related.Error = StatusFailed, ErrTOTPRecoveryPending.Error()
		}
	}
	j.ResetTOTP, j.TOTPResetStatus, j.TOTPResetPhase = true, "disabling", "disable_intent"
	j.UpdatedAt = time.Now().UTC()
	m.persistLocked()
	if m.lastPersistError != nil {
		return errors.New("无法可靠保存 2FA 重置阶段，已停止后续请求")
	}
	readback, err := credentials.LoadContext(ctx, m.vault, j.Email)
	if err != nil || !reflect.DeepEqual(readback, c) {
		return errors.New("2FA 重置凭据读回不一致，已停止后续请求")
	}
	return ctx.Err()
}

// CommitTOTPReset acknowledges only the new, verified credential that the
// runner has already saved. It does not perform remote account operations.
func (m *Manager) CommitTOTPReset(ctx context.Context, id string, expected credentials.Credentials) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id]
	if !ok {
		return osErrNotFound(id)
	}
	if !j.ResetTOTP || expected.TOTPResetPending || expected.TOTPSecret == "" || m.vault == nil || m.store == nil {
		return errors.New("2FA 新密钥尚未可靠保存")
	}
	readback, err := credentials.LoadContext(ctx, m.vault, j.Email)
	if err != nil || !reflect.DeepEqual(readback, expected) {
		return errors.New("2FA 新密钥安全存储读回不一致")
	}
	for _, related := range m.jobs {
		if emailKey(related.Email) != emailKey(j.Email) {
			continue
		}
		related.TOTPRecoveryPending, related.TOTPCredentialInvalidated = false, false
		related.HasTOTPKey, related.TOTPKnownEnabled = true, true
		if related.Input == nil {
			related.Input = map[string]any{}
		}
		related.Input["totpSecret"] = expected.TOTPSecret
	}
	j.TOTPResetStatus, j.TOTPResetPhase, j.TOTPResetError = "activated", "credential_persisted", ""
	m.persistLocked()
	if m.lastPersistError != nil {
		for _, related := range m.jobs {
			if emailKey(related.Email) == emailKey(j.Email) {
				quarantineTOTP(related)
			}
		}
		return errors.New("新密钥已保存，但重置元数据保存失败，恢复资料已保留")
	}
	return ctx.Err()
}

func resultBool(result map[string]any, key string) bool     { v, _ := result[key].(bool); return v }
func resultString(result map[string]any, key string) string { v, _ := result[key].(string); return v }

func applyTOTPResetResult(j *Job, result map[string]any, operationErr error) {
	// Redact while private input still identifies nested integration secrets.
	// Retaining a raw error here would expose it once workflow completion
	// removes the temporary upload configuration.
	known := append(jobKnownSecrets(j), resultString(result, "secret"), resultString(result, "otpauth_uri"))
	committed := j.TOTPResetPhase == "credential_persisted" && !j.TOTPCredentialInvalidated && !j.TOTPRecoveryPending
	if resultBool(result, "reset_totp_requested") {
		j.ResetTOTP = true
	}
	if j.ResetTOTP && !committed {
		if status := resultString(result, "reset_totp_status"); status != "" {
			switch status {
			case "pending", "disabling", "disabled", "activated", "failed", "unknown":
				j.TOTPResetStatus = status
			}
		}
		if phase := resultString(result, "reset_totp_phase"); eventIdentifier.MatchString(phase) {
			j.TOTPResetPhase = phase
		}
		if resultBool(result, "old_factor_invalidated") || resultString(result, "reset_totp_attempted_at") != "" {
			quarantineTOTP(j)
		}
		if j.TOTPRecoveryPending || j.TOTPCredentialInvalidated {
			if j.TOTPResetStatus == "pending" || j.TOTPResetStatus == "disabling" || j.TOTPResetStatus == "disabled" || j.TOTPResetStatus == "" {
				j.TOTPResetStatus = "unknown"
			}
			j.TOTPResetError = "2FA 重置未完整完成，请手动核实并恢复有效密钥"
		} else if operationErr != nil {
			j.TOTPResetStatus = "failed"
		}
		if text := resultString(result, "reset_totp_error"); text != "" {
			j.TOTPResetError = logging.RedactKnown(text, known...)
		} else if operationErr != nil {
			j.TOTPResetError = logging.RedactKnown(safeError(operationErr), known...)
		}
	}
	if resultBool(result, "logout_all_devices_requested") {
		j.LogoutAllDevicesAfterTOTP = true
	}
	if status := resultString(result, "logout_all_devices_status"); status != "" {
		switch status {
		case "not_requested", "pending", "succeeded", "failed", "unknown", "not_attempted", "skipped":
			j.LogoutAllDevicesStatus = status
		}
	}
	if j.LogoutAllDevicesAfterTOTP && j.LogoutAllDevicesStatus == "pending" {
		j.LogoutAllDevicesStatus = "not_attempted"
		if strings.TrimSpace(resultString(result, "logout_all_devices_attempted_at")) != "" {
			j.LogoutAllDevicesStatus = "unknown"
		}
	}
	j.LogoutAllDevicesError = logging.RedactKnown(resultString(result, "logout_all_devices_error"), known...)
}

// UpdateTOTPResetResult records public outcome fields only. Secret-bearing
// recovery evidence remains in the private result file until commit succeeds.
func (m *Manager) UpdateTOTPResetResult(id string, result map[string]any, operationErr error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id]
	if !ok {
		return osErrNotFound(id)
	}
	applyTOTPResetResult(j, result, operationErr)
	j.UpdatedAt = time.Now().UTC()
	m.persistLocked()
	return m.lastPersistError
}
