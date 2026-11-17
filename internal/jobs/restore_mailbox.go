package jobs

import (
	"context"
	"errors"
	"os"

	"tosub2-go-port/internal/accountlock"
	"tosub2-go-port/internal/credentials"
)

// A mailbox migration replaces the complete credential value. Keep the normal
// account lease through readback, and reread under it so another process's
// completed password, factor or mailbox update cannot be replaced by a snapshot
// inspected earlier during discovery. Failure retains the legacy input in memory.
// loaded reports a fresh protected snapshot even when migration is deferred;
// callers must retain its quarantine flags if the earlier record was absent.
func migrateLegacyMailbox(ctx context.Context, vault credentials.Store, email string, legacy credentials.Credentials, acquire func(string) (func() error, error)) (value credentials.Credentials, loaded bool, err error) {
	if err := ctx.Err(); err != nil {
		return legacy, false, err
	}
	if acquire == nil {
		acquire = func(email string) (func() error, error) { return accountlock.Acquire("https://chatgpt.com", email) }
	}
	release, err := acquire(email)
	if release != nil {
		defer release()
	}
	if err != nil {
		return legacy, false, err
	}
	if release == nil {
		return legacy, false, errors.New("旧邮箱迁移没有取得有效的普通账号操作锁")
	}
	current, err := credentials.LoadContext(ctx, vault, email)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return legacy, false, err
	}
	loaded = err == nil
	if current.MailAPIURL != "" {
		return current, loaded, nil
	}
	current.MailAPIURL, current.MailMethod, current.MailBody = legacy.MailAPIURL, legacy.MailMethod, legacy.MailBody
	if current.TOTPResetPending {
		return current, loaded, errors.New("账号存在待完成的 2FA 恢复，旧邮箱资料仅在本次进程内保留")
	}
	err = saveRecoveredCredentials(ctx, vault, email, current)
	return current, loaded || err == nil, err
}
