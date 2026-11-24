package jobs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"reflect"

	"tosub2-go-port/internal/accountlock"
	"tosub2-go-port/internal/privatefile"
	"tosub2-go-port/internal/privatestate"
	"tosub2-go-port/internal/protocol"
)

// archiveCompletedTOTPResetLocked permits an explicitly new reset intent to
// replace only its own fully committed, session-free journal. The admission
// caller holds the normal account lease until metadata commits and rolls the
// original file back if admission fails. Unknown and legacy records stay put.
func (m *Manager) archiveCompletedTOTPResetLocked(j *Job, values map[string]any, accountOwned bool) (rollback func() error, release func() error, err error) {
	if m.store == nil || !safeID(j.ID) {
		return nil, nil, nil
	}
	path := filepath.Join(m.store.Root, j.ID, "totp-setup-result.json")
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	}
	if protocol.ValidateTOTPResetStart(path, j.Email) == nil {
		return nil, nil, nil
	}
	intent, _ := values["totpIntentId"].(string)
	if !totpIntentPattern.MatchString(intent) || intent == j.TOTPIntentID {
		return nil, nil, errors.New("再次重置 2FA 需要新的明确请求标识")
	}
	if accountOwned {
		// The surrounding credential edit already retains this lease through
		// metadata admission and rollback. Do not recursively acquire it.
		release = func() error { return nil }
	} else {
		acquire := m.accountOperationLease
		if acquire == nil {
			acquire = func(email string) (func() error, error) { return accountlock.Acquire("https://chatgpt.com", email) }
		}
		release, err = acquire(j.Email)
	}
	if err != nil {
		if release != nil {
			_ = release()
		}
		return nil, nil, err
	}
	if release == nil {
		return nil, nil, errors.New("本地归档没有取得有效的普通账号操作锁")
	}
	fail := func(e error) (func() error, func() error, error) { _ = release(); return nil, nil, e }
	result, readErr := protocol.ReadTOTPResetCompletion(path, j.Email)
	if readErr != nil || !m.totpResetCommittedLocked(j) || protocol.TOTPResetJournalIsReadOnly(result) {
		return fail(errors.New("之前的 2FA 证据尚未完成或含旧会话资料，不能自动替换"))
	}
	operation := resultString(result, "operation_id")
	digest := sha256.Sum256([]byte(operation))
	archive := filepath.Join(filepath.Dir(path), "totp-completed-"+hex.EncodeToString(digest[:])+".json")
	archiveExists := false
	if info, statErr := os.Lstat(archive); statErr == nil {
		if !info.Mode().IsRegular() {
			return fail(errors.New("本次 2FA 已完成证据的归档路径不可用"))
		}
		archiveExists = true
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return fail(statErr)
	}
	original, err := os.ReadFile(path)
	if err != nil {
		return fail(err)
	}
	// Protected journals authenticate their path. Re-encrypt at the archive
	// destination and read it back before removing the active-path evidence.
	if !archiveExists {
		if err := privatestate.WriteJSON(m.rootCtx, archive, "totp-reset", j.Email, operation, result); err != nil {
			return fail(err)
		}
	}
	archived, err := protocol.ReadTOTPResetCompletion(archive, j.Email)
	if err != nil || !reflect.DeepEqual(archived, result) {
		return fail(errors.New("已完成 2FA 证据归档读回不一致，原记录未移除"))
	}
	rollback = func() error {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			return errors.New("原操作路径已变化，未覆盖新证据；完成记录仍在本地归档")
		}
		if err := privatefile.Write(context.Background(), path, original); err != nil {
			return err
		}
		return privatefile.Remove(context.Background(), archive)
	}
	if err := privatefile.Remove(m.rootCtx, path); err != nil {
		if _, statErr := os.Lstat(path); errors.Is(statErr, os.ErrNotExist) {
			err = errors.Join(err, rollback())
		}
		return fail(err)
	}
	return rollback, release, nil
}

// SetAccountOperationLease installs the ordinary account lock during startup
// so queries, local recovery and archival use the same origin as remote operations.
func (m *Manager) SetAccountOperationLease(acquire func(string) (func() error, error)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.accountOperationLease = acquire
}
