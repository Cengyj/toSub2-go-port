package jobs

import (
	"errors"
	"os"

	"tosub2-go-port/internal/accountlock"
	"tosub2-go-port/internal/credentials"
)

const credentialEditPendingWarning = "ACCOUNT_CREDENTIAL_EDIT_PENDING: 新资料已保留，等待当前会话结束后保存凭据；尚未确认持久保存"

var errDeferredCredentialSave = errors.New("ACCOUNT_CREDENTIAL_EDIT_INCOMPLETE: 会话结束后的凭据编辑未可靠完成，已保留当前资料并停止自动重启；请核对后再操作")

// An active runner alone owns the credential read/modify/write sequence. Keep
// later edits as an explicit delta until its transport and OS lease are gone.
// Passwords submitted at an authentication prompt are filtered by inputLocked;
// they remain candidates for the protocol's verified-password hook.
func (m *Manager) deferCredentialEditLocked(j *Job, delta map[string]any) {
	attempt := m.active[j.ID]
	if attempt == nil {
		return
	}
	for _, key := range credentialInputKeys {
		if value, present := delta[key]; present {
			if attempt.credentialEdits == nil {
				attempt.credentialEdits = make(map[string]any)
			}
			attempt.credentialEdits[key] = cloneValue(value)
		}
	}
	if len(attempt.credentialEdits) > 0 {
		setCredentialWriteWarning(j, credentialEditPendingWarning)
	}
}

// Call only after the runner returns, while it remains the manager's active
// attempt. The OS lease is reacquired because another process could have won
// the gap after runner cleanup. Failure keeps the process-local draft and does
// not queue another authentication attempt.
func (m *Manager) applyDeferredCredentialEditsLocked(j *Job, attempt *executionAttempt) error {
	if attempt == nil || len(attempt.credentialEdits) == 0 {
		return nil
	}
	fail := func(message string) error {
		if j.Input == nil {
			j.Input = make(map[string]any)
		}
		for key, value := range attempt.credentialEdits {
			j.Input[key] = cloneValue(value)
		}
		setCredentialWriteWarning(j, "ACCOUNT_CREDENTIAL_EDIT_PENDING: "+message+"；未确认保存的草稿仅保留于本次进程，未自动重启")
		return errDeferredCredentialSave
	}
	if m.vault == nil {
		return fail("安全凭据存储不可用，尚未保存编辑")
	}
	release, err := m.acquireCredentialEditLeaseLocked(j.Email, attempt.credentialEdits)
	if err != nil {
		if errors.Is(err, accountlock.ErrBusy) {
			return fail("另一操作仍持有该账号，尚未保存编辑")
		}
		return fail("无法取得账号凭据编辑锁，尚未保存编辑")
	}
	// A runner may have ended with a newly protected recovery journal or vault
	// quarantine. Its successful/unknown security result outranks a later draft.
	if err = m.checkTOTPQuarantineLocked(j.Email, false); err == nil {
		err = m.writeCredentialDeltaLocked(j, attempt.credentialEdits)
	} else {
		err = errors.New("2FA 修改仍有待处理证据，未覆盖安全凭据")
	}
	if err == nil {
		// A peer may have completed an independent operation before this
		// cleanup acquired the lease. Mirror the merged record now so the
		// next runner cannot overwrite its unedited fields with stale Input.
		var saved credentials.Credentials
		saved, err = credentials.LoadContext(m.rootCtx, m.vault, j.Email)
		if errors.Is(err, os.ErrNotExist) {
			// Explicitly clearing the final field deletes the empty record.
			err = nil
		}
		if err == nil {
			// This readback confirms the complete merged record. The draft
			// shield only protects against callbacks from the previous runner.
			draft := attempt.credentialEdits
			attempt.credentialEdits = nil
			m.mirrorStoredCredentialsLocked(j, saved)
			attempt.credentialEdits = draft
			clearResolvedCredentialWriteWarning(j, saved)
		} else {
			err = errors.New("凭据已经保存，但当前记录读取失败；未重新提交保存")
		}
	}
	var releaseErr error
	if release != nil {
		releaseErr = release()
	}
	if err != nil {
		return fail(err.Error())
	}
	if releaseErr != nil {
		return fail("凭据已经保存，但账号锁释放未确认")
	}
	attempt.credentialEdits = nil
	return nil
}
