package jobs

import (
	"bytes"
	"errors"
	"path/filepath"

	"tosub2-go-port/internal/accountlock"
	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/protocol"
)

// Immutable evidence held only until its recovered metadata is committed.
// It is excluded from public projections and metadata projection caches.
type privateResultCleanup struct {
	password                 *passwordRecoveryEvidence
	totp                     []byte
	totpSecret, totpFactorID string
}

func (m *Manager) cleanupPrivateResultEvidence(j *Job) error {
	if !j.passwordCleanup && !j.totpSetupCleanup {
		return nil
	}
	proof := j.recoveryCleanup
	if proof == nil || m.vault == nil {
		return errors.New("恢复结果清理缺少绑定证据，私有资料已保留")
	}
	acquire := m.accountOperationLease
	if acquire == nil {
		acquire = func(email string) (func() error, error) { return accountlock.Acquire("https://chatgpt.com", email) }
	}
	release, err := acquire(j.Email)
	if err != nil || release == nil {
		if release != nil {
			_ = release()
		}
		return errors.New("恢复结果清理未取得账号操作锁，私有资料已保留")
	}
	defer release()
	current, err := credentials.LoadContext(m.rootCtx, m.vault, j.Email)
	if err != nil {
		return errors.New("恢复结果清理无法核对已提交凭据，私有资料已保留")
	}
	if j.passwordCleanup {
		if proof.password == nil || proof.password.completion == nil || current.Password != proof.password.completion.Password || current.PendingPassword != "" {
			return errors.New("密码凭据在恢复提交后变化，未清理操作证据")
		}
		if err := proof.password.verify(j.Email); err != nil {
			return err
		}
		if err := protocol.RemovePasswordAddEvidence(proof.password.path); err != nil {
			return err
		}
		j.passwordCleanup = false
	}
	if j.totpSetupCleanup && !j.TOTPRecoveryPending && !j.TOTPCredentialInvalidated {
		path := filepath.Join(m.store.Root, j.ID, "totp-setup-result.json")
		latest, err := readRegularJSON(path)
		if err != nil || len(proof.totp) == 0 || !bytes.Equal(latest, proof.totp) || current.TOTPResetPending || current.TOTPSecret != proof.totpSecret || current.TOTPFactorID != proof.totpFactorID {
			return errors.New("2FA 结果或凭据在恢复提交后变化，未清理操作证据")
		}
		if err := protocol.RemovePrivateResult(path); err != nil {
			return err
		}
		j.totpSetupCleanup = false
	}
	if !j.passwordCleanup && !j.totpSetupCleanup {
		j.recoveryCleanup = nil
	}
	return nil
}
