package jobs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/protocol"
)

// Completion consumes only a confirmed local operation. It never authorizes
// login, factor inspection, replay, or loading a previous Cookie session.
func (m *Manager) CanCompleteTOTPReset(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.canCompleteTOTPResetLocked(id, true)
}

func (m *Manager) TOTPResetCompletionPending(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.totpResetCompletionPendingLocked(m.jobs[id])
}

func (m *Manager) totpResetCompletionPendingLocked(j *Job) bool {
	if j == nil || m.store == nil || !safeID(j.ID) || j.TOTPResetCommitted {
		return false
	}
	_, err := protocol.ReadTOTPResetCompletion(filepath.Join(m.store.Root, j.ID, "totp-setup-result.json"), j.Email)
	return err == nil
}

func (m *Manager) requireTOTPCompletionStorageLocked(id string) error {
	if m.store == nil || m.vault == nil || !safeID(id) {
		return errors.New("完成本地保存需要持久任务记录和安全凭据存储")
	}
	if _, memory := m.vault.(*credentials.MemoryStore); memory {
		return errors.New("完成本地保存需要持久安全凭据存储")
	}
	return nil
}

func (m *Manager) canCompleteTOTPResetLocked(id string, allowOwn bool) error {
	j := m.jobs[id]
	if j == nil {
		return osErrNotFound(id)
	}
	if m.closing || m.rootCtx.Err() != nil {
		return errors.New("服务正在关闭，暂不能完成本地保存")
	}
	if err := m.requireTOTPCompletionStorageLocked(id); err != nil {
		return err
	}
	if j.TOTPResetCommitted {
		return errors.New("本次 2FA 已完成本地保存")
	}
	key := emailKey(j.Email)
	if m.deleting[key] || m.importing[key] || m.accountQueries[key] != nil {
		return errors.New("同账号正在执行其他操作")
	}
	for _, related := range m.jobs {
		if emailKey(related.Email) == key && related.ID != id && (related.TOTPRecoveryPending || related.TOTPCredentialInvalidated) && related.TOTPResetOperationID != j.TOTPResetOperationID {
			return errors.New("同账号存在其他未确认的 2FA 操作，未覆盖凭据")
		}
		if emailKey(related.Email) != key || (!containsPending(m.pending, related.ID) && m.active[related.ID] == nil) {
			continue
		}
		if allowOwn && related.ID == id && related.Operation == "complete-2fa" {
			continue
		}
		return errors.New("同账号已有任务排队或运行")
	}
	if err := m.checkOtherTOTPResultsLocked(j.Email, id); err != nil {
		return err
	}
	result, err := protocol.ReadTOTPResetCompletion(filepath.Join(m.store.Root, id, "totp-setup-result.json"), j.Email)
	if err != nil {
		return errors.New("没有完整有效的本次激活证据；未知结果须人工处理")
	}
	saved, err := credentials.LoadContext(m.rootCtx, m.vault, j.Email)
	if err != nil || !totpCompletionMatchesVault(result, saved) {
		return errors.New("激活证据与安全凭据的操作或密钥不一致，未覆盖凭据")
	}
	if j.TOTPResetOperationID == "" || j.TOTPResetOperationID != resultString(result, "operation_id") {
		return errors.New("激活证据不属于本次任务操作")
	}
	return nil
}

func totpCompletionMatchesVault(result map[string]any, c credentials.Credentials) bool {
	operation, secret := resultString(result, "operation_id"), resultString(result, "secret")
	if operation == "" || secret == "" || c.TOTPResetOperationID != operation {
		return false
	}
	if c.TOTPResetPending {
		return c.PendingTOTPSecret == secret && c.PendingTOTPURI == resultString(result, "otpauth_uri")
	}
	return c.TOTPSecret == secret && c.PendingTOTPSecret == "" && c.PendingTOTPURI == ""
}

func validateTOTPCompletionValues(values map[string]any) error {
	for key := range values {
		if key != "totpIntentId" {
			return errors.New("完成本地保存不接受登录资料或后续操作选项")
		}
	}
	return ValidateTOTPIntent(values)
}

func (m *Manager) queueTOTPCompletionLocked(id string, values map[string]any) (*Job, error) {
	if err := validateTOTPCompletionValues(values); err != nil {
		return nil, err
	}
	if err := m.canCompleteTOTPResetLocked(id, false); err != nil {
		return nil, err
	}
	j := m.jobs[id]
	before := cloneJob(j)
	recordTOTPIntent(j, values)
	j.Operation, j.LastOperationType = "complete-2fa", "complete-2fa"
	beginOperationState(j)
	j.ResetTOTP = true
	initializeTOTPWorkflow(j, nil)
	j.LogoutAllDevicesAfterTOTP, j.ReloginAfterTOTP, j.UploadAfterTOTP = false, false, false
	for _, key := range []string{"resetTotp", "logoutAllDevicesAfterTotp", "reloginAfterTotp", "uploadAfterTotp", "sub2apiConfig", "cpaConfig", "uploadTarget", "totpReauthenticate"} {
		delete(j.Input, key)
	}
	j.Status, j.Prompt = StatusQueued, "正在完成已确认 2FA 的本地保存；不会登录或连接远端"
	j.LastOperationAt = time.Now().UTC()
	j.QueuedAt, j.UpdatedAt = j.LastOperationAt, j.LastOperationAt
	j.Error, j.LastError, j.FinishedAt = "", "", nil
	if err := m.commitAdmissionLocked(j, before, JobEvent{JobID: id, Status: StatusQueued, Stage: "queued", Prompt: j.Prompt, At: j.UpdatedAt}); err != nil {
		return nil, err
	}
	return cloneJob(j), nil
}

func (m *Manager) validateTOTPResetCommitLocked(ctx context.Context, j *Job, operationID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if j == nil || operationID == "" || j.TOTPResetOperationID != operationID || m.vault == nil || m.store == nil {
		return errors.New("2FA 本地操作身份尚未确认")
	}
	result, err := protocol.ReadTOTPResetCompletion(filepath.Join(m.store.Root, j.ID, "totp-setup-result.json"), j.Email)
	if err != nil || resultString(result, "operation_id") != operationID {
		return errors.New("2FA 激活与本地保存证据不完整")
	}
	c, err := credentials.LoadContext(ctx, m.vault, j.Email)
	if err != nil || c.TOTPResetPending || !totpCompletionMatchesVault(result, c) {
		return errors.New("2FA 完成证据与安全凭据不一致")
	}
	return nil
}

// MarkTOTPResetCommitted is the final durable local commit. Journal retention
// is evidence, never a Cookie/session handoff prerequisite.
func (m *Manager) MarkTOTPResetCommitted(ctx context.Context, id, operationID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	j := m.jobs[id]
	if j == nil {
		return osErrNotFound(id)
	}
	if !j.ResetTOTP {
		return errors.New("当前任务没有待提交的 2FA 操作")
	}
	if err := m.validateTOTPResetCommitLocked(ctx, j, operationID); err != nil {
		return err
	}
	c, err := credentials.LoadContext(ctx, m.vault, j.Email)
	if err != nil {
		return err
	}
	before := map[string]*Job{}
	for _, related := range m.jobs {
		if emailKey(related.Email) != emailKey(j.Email) {
			continue
		}
		if related.TOTPResetOperationID != operationID {
			if !m.totpResetHistoricallyCommittedLocked(related) || related.TOTPRecoveryPending || related.TOTPCredentialInvalidated {
				continue
			}
			before[related.ID] = cloneJob(related)
			if related.Input == nil {
				related.Input = map[string]any{}
			}
			// Keep the historical operation proof, but use the current vault seed
			// if this row later starts an ordinary login in the same process.
			related.Input["totpSecret"] = c.TOTPSecret
			delete(related.Input, "totp")
			delete(related.Input, "totpCode")
			continue
		}
		before[related.ID] = cloneJob(related)
		related.TOTPRecoveryPending, related.TOTPCredentialInvalidated = false, false
		related.HasTOTPKey, related.TOTPKnownEnabled = true, true
		if related.Input == nil {
			related.Input = map[string]any{}
		}
		related.Input["totpSecret"] = c.TOTPSecret
	}
	if _, ok := before[id]; !ok {
		before[id] = cloneJob(j)
	}
	j.TOTPResetCommitted, j.TOTPActivationVerified = true, true
	j.TOTPRecoveryPending, j.TOTPCredentialInvalidated = false, false
	j.HasTOTPKey, j.TOTPKnownEnabled = true, true
	j.TOTPResetStatus, j.TOTPResetPhase, j.TOTPResetError = "activated", "credential_persisted", ""
	for key, receipt := range j.TOTPIntentReceipts {
		if receipt.ResetIntentID == j.TOTPIntentID {
			receipt.Completed, receipt.OperationID = true, operationID
			j.TOTPIntentReceipts[key] = receipt
		}
	}
	j.UpdatedAt = time.Now().UTC()
	m.persistLocked()
	if m.lastPersistError != nil {
		for key, prior := range before {
			*m.jobs[key] = *prior
		}
		return m.lastPersistError
	}
	return nil
}

func (m *Manager) totpResetCommittedLocked(j *Job) bool {
	return j != nil && j.TOTPResetCommitted && j.TOTPActivationVerified && j.TOTPResetPhase == "credential_persisted" && m.validateTOTPResetCommitLocked(m.rootCtx, j, j.TOTPResetOperationID) == nil
}

// Unknown private records continue to block new operations, even if old public
// metadata did not preserve the quarantine. Nothing is resumed from them.
func (m *Manager) unresolvedTOTPJournalLocked(email string) bool {
	if m.store == nil {
		return false
	}
	for _, j := range m.jobs {
		if emailKey(j.Email) != emailKey(email) || !safeID(j.ID) {
			continue
		}
		p := filepath.Join(m.store.Root, j.ID, "totp-setup-result.json")
		if _, err := os.Lstat(p); errors.Is(err, os.ErrNotExist) {
			continue
		}
		if m.totpResetHistoricallyCommittedLocked(j) {
			continue
		}
		if protocol.ValidateTOTPResetStart(p, email) != nil {
			return true
		}
	}
	return false
}

// Historical completion remains true when a later operation changes the vault.
// Current commits and workflow continuation still require exact vault readback.
func (m *Manager) totpResetHistoricallyCommittedLocked(j *Job) bool {
	if j == nil || !j.TOTPResetCommitted || !j.TOTPActivationVerified || j.TOTPResetPhase != "credential_persisted" || j.TOTPResetOperationID == "" || m.store == nil || !safeID(j.ID) {
		return false
	}
	result, err := protocol.ReadTOTPResetCompletion(filepath.Join(m.store.Root, j.ID, "totp-setup-result.json"), j.Email)
	return err == nil && resultString(result, "operation_id") == j.TOTPResetOperationID
}
