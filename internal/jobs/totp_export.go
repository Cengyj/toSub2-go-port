package jobs

import (
	"errors"
	"os"

	"tosub2-go-port/internal/credentials"
)

var ErrTOTPExportUnavailable = errors.New("无法确认账号安全凭据状态，已停止资料导出和上传，请核实安全凭据存储后重试")

// CheckTOTPExport checks the live account boundary before exposing a saved
// authorization. It only reads recovery evidence; a web download or upload
// cannot finish a reset, commit credentials, or consume a retained journal.
func (m *Manager) CheckTOTPExport(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id]
	if !ok {
		return osErrNotFound(id)
	}
	return m.checkTOTPExportLocked(j.Email)
}

// CheckTOTPWorkflowExport retains every read-only recovery boundary while
// allowing a completed reset's own active workflow to upload its new grant.
// Callers must hold the cross-process account lease before using this gate.
func (m *Manager) CheckTOTPWorkflowExport(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id]
	if !ok {
		return osErrNotFound(id)
	}
	if err := m.checkTOTPExportVaultLocked(j.Email); err != nil {
		return err
	}
	ownUpload := j.Operation == "reset-2fa" && j.ResetTOTP && m.active[id] != nil && !containsPending(m.pending, id) && !m.closing && m.rootCtx.Err() == nil &&
		j.TOTPWorkflowStatus == "running" && j.TOTPWorkflowStep == "upload" && j.UploadAfterTOTP && j.ReloginAfterTOTP &&
		j.TOTPReloginStatus == "succeeded" && j.TOTPUploadStatus == "running" && j.ResultSaved && j.HasTOTPKey &&
		j.TOTPResetPhase == "credential_persisted" && j.TOTPActivationVerified && j.TOTPResetCommitted &&
		(!j.LogoutAllDevicesAfterTOTP || j.LogoutAllDevicesStatus == "succeeded")
	exceptID := ""
	if ownUpload {
		exceptID = id
	}
	if m.totpExportPendingExceptLocked(j.Email, exceptID) {
		return ErrTOTPRecoveryPending
	}

	if ownUpload && (!m.totpResetCommittedLocked(j)) {
		return ErrTOTPRecoveryPending
	}
	return nil
}

// CheckTOTPExportEmail also covers manually supplied upload payloads which
// have no selected job IDs. Unknown external accounts need no local record.
func (m *Manager) CheckTOTPExportEmail(email string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.checkTOTPExportLocked(email)
}

func (m *Manager) checkTOTPExportLocked(email string) error {
	if m.totpExportPendingLocked(email) {
		return ErrTOTPRecoveryPending
	}
	return m.checkTOTPExportVaultLocked(email)
}

func (m *Manager) checkTOTPExportVaultLocked(email string) error {
	if m.vault != nil {
		current, err := credentials.LoadContext(m.rootCtx, m.vault, email)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return ErrTOTPExportUnavailable
		}
		if current.TOTPResetPending {
			return ErrTOTPRecoveryPending
		}
	}
	return nil
}

// TOTPExportPending projects local account state for the UI without opening
// the credential store. Actual exports must still call CheckTOTPExport.
func (m *Manager) TOTPExportPending(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	j := m.jobs[id]
	if j == nil {
		return false
	}
	return m.totpExportPendingLocked(j.Email)
}

func (m *Manager) totpExportPendingLocked(email string) bool {
	return m.totpExportPendingExceptLocked(email, "")
}

func (m *Manager) totpExportPendingExceptLocked(email, exceptID string) bool {
	if m.totpBlockedLocked(email) || m.unresolvedTOTPJournalLocked(email) {
		return true
	}
	for _, related := range m.jobs {
		if related.ID != exceptID && emailKey(related.Email) == emailKey(email) && resetOperation(related.Operation) && (containsPending(m.pending, related.ID) || m.active[related.ID] != nil) {
			return true
		}
	}
	return false
}
