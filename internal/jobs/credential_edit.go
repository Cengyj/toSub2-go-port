package jobs

import (
	"errors"
	"maps"
	"os"
	"strings"

	"tosub2-go-port/internal/accountlock"
	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/privatefile"
)

var credentialInputKeys = []string{"password", "totpSecret", "proxyUrl", "mailApiUrl", "mailRequestMethod", "mailRequestBody", "mailRequestHeaders"}

const (
	credentialLoadWarning      = "ACCOUNT_CREDENTIAL_LOAD_FAILED: 无法读取系统凭据，未覆盖现有凭据"
	credentialSaveWarning      = "ACCOUNT_CREDENTIAL_SAVE_FAILED: 系统凭据保存失败，登录资料仅保留于内存"
	credentialUncertainWarning = "ACCOUNT_CREDENTIAL_SAVE_UNCERTAIN: 系统凭据保存结果待核实，当前登录资料已保留，请核对后再操作"
	credentialClearWarning     = "ACCOUNT_CREDENTIAL_CLEAR_UNCONFIRMED: 系统凭据清除后读回未确认，请核实；未重复清除"
	credentialReadbackWarning  = "ACCOUNT_CREDENTIAL_READBACK_UNCONFIRMED: 系统凭据保存后读回未确认，请保留当前资料并核实；未重复保存"
	credentialMetadataWarning  = "ACCOUNT_CREDENTIAL_STATUS_PERSISTENCE: 凭据编辑的最新状态尚未确认保存到元数据；已保存或已受理的结果保留，不要重复提交"
)

// Only warnings owned by this write boundary may be resolved by its readback.
// Keep unrelated metadata, log archival and ownership-cleanup warnings intact.
func isCredentialWriteWarning(value string) bool {
	code, _, _ := strings.Cut(value, ": ")
	switch code {
	case "ACCOUNT_CREDENTIAL_LOAD_FAILED", "ACCOUNT_CREDENTIAL_SAVE_FAILED", "ACCOUNT_CREDENTIAL_SAVE_UNCERTAIN", "ACCOUNT_CREDENTIAL_CLEAR_UNCONFIRMED", "ACCOUNT_CREDENTIAL_READBACK_UNCONFIRMED", "ACCOUNT_CREDENTIAL_EDIT_PENDING":
		return true
	}
	// Existing v2 metadata predates stable warning codes.
	switch value {
	case "无法读取系统凭据，未覆盖现有凭据", "系统凭据保存失败，登录资料仅保留于内存", "系统凭据保存结果待核实，当前登录资料已保留，请核对后再操作", "系统凭据清除后读回未确认，请核实；未重复清除", "系统凭据保存后读回未确认，请保留当前资料并核实；未重复保存":
		return true
	}
	return false
}

func credentialWarningLines(value string) []string {
	// Older cleanup appended this independent warning to the write warning.
	// Recognize only that exact boundary; ordinary semicolons belong to text.
	value = strings.ReplaceAll(value, "；ACCOUNT_CREDENTIAL_LOCK_RELEASE: ", "\nACCOUNT_CREDENTIAL_LOCK_RELEASE: ")
	return strings.Split(value, "\n")
}

func setCredentialWriteWarning(j *Job, warning string) {
	var retained []string
	for _, line := range credentialWarningLines(j.LastError) {
		if line != "" && !isCredentialWriteWarning(line) {
			retained = append(retained, line)
		}
	}
	if warning != "" {
		retained = append(retained, warning)
	}
	j.LastError = strings.Join(retained, "\n")
}

func appendCredentialWarning(j *Job, warning string) {
	for _, line := range credentialWarningLines(j.LastError) {
		if line == warning {
			return
		}
	}
	if j.LastError != "" {
		j.LastError += "\n"
	}
	j.LastError += warning
}

func (m *Manager) persistCredentialWarningLocked(j *Job) {
	m.persistLocked()
	if m.lastPersistError != nil {
		// The vault outcome and prior admission are already observed. A second
		// metadata failure may only add a warning; it cannot replay either one.
		appendCredentialWarning(j, credentialMetadataWarning)
	}
}

func emptyCredentialRecord(c credentials.Credentials) bool {
	return !c.TOTPResetPending && c.Password == "" && c.TOTPSecret == "" && c.ProxyURL == "" && c.MailAPIURL == "" && c.MailBody == "" && len(c.MailHeaders) == 0 && c.PendingPassword == "" && c.PendingTOTPSecret == "" && c.PendingTOTPURI == "" && c.PendingTOTPFactorID == ""
}

func credentialInputMatchesRecord(input map[string]any, saved credentials.Credentials) bool {
	fields := map[string]string{"password": saved.Password, "totpSecret": saved.TOTPSecret, "proxyUrl": saved.ProxyURL, "mailApiUrl": saved.MailAPIURL, "mailRequestMethod": saved.MailMethod, "mailRequestBody": saved.MailBody}
	for key, expected := range fields {
		if key == "mailRequestMethod" && emptyCredentialRecord(saved) {
			// A method alone never creates a protected record. Explicitly
			// clearing its last material field leaves that default harmless.
			continue
		}
		if value, present := input[key]; present {
			text, ok := value.(string)
			if !ok || text != expected {
				return false
			}
		}
	}
	if value, present := input["mailRequestHeaders"]; present {
		var headers map[string]string
		switch value := value.(type) {
		case nil:
			// The compatibility input API already treats explicit null as a
			// header clear; readback must apply the same presence semantics.
		case map[string]string:
			headers = value
		case map[string]any:
			headers = make(map[string]string, len(value))
			for key, raw := range value {
				text, ok := raw.(string)
				if !ok {
					return false
				}
				headers[key] = text
			}
		default:
			return false
		}
		return maps.Equal(headers, saved.MailHeaders)
	}
	return true
}

func clearResolvedCredentialWriteWarning(j *Job, saved credentials.Credentials) {
	// Saving a different field must not claim an earlier failed draft is saved.
	if credentialInputMatchesRecord(j.Input, saved) {
		setCredentialWriteWarning(j, "")
	}
}

func hasCredentialDelta(values map[string]any) bool {
	for _, key := range credentialInputKeys {
		if _, ok := values[key]; ok {
			return true
		}
	}
	return false
}

// Call before mutating metadata or input, and hold through read/merge/write and
// readback. Account acquisition is nonblocking; a peer writer is never waited
// on while the manager mutex is held. A local query can lend its still-held OS
// lease until the edit finishes, then observes its canceled generation.
func (m *Manager) acquireCredentialEditLeaseLocked(email string, delta map[string]any) (func() error, error) {
	if m.vault == nil || !hasCredentialDelta(delta) {
		return nil, nil
	}
	if err := m.rootCtx.Err(); err != nil {
		return nil, err
	}
	if lease := m.accountQueries[emailKey(email)]; lease != nil {
		if release := lease.tryBorrowAccountLock(); release != nil {
			return release, nil
		}
		return nil, accountlock.ErrBusy
	}
	acquire := m.accountOperationLease
	if acquire == nil {
		acquire = func(email string) (func() error, error) { return accountlock.Acquire("https://chatgpt.com", email) }
	}
	release, err := acquire(email)
	if err != nil || release == nil {
		if release != nil {
			_ = release()
		}
		if errors.Is(err, accountlock.ErrBusy) {
			return nil, accountlock.ErrBusy
		}
		return nil, errors.New("ACCOUNT_CREDENTIAL_LOCK_UNAVAILABLE: 无法取得账号凭据编辑锁，未修改账号资料")
	}
	return release, nil
}

// Cleanup does not undo a confirmed credential write or task admission. Keep
// that result and report cleanup independently, including in the returned copy.
// Never retry the write merely because closing its ownership handle failed.
func (m *Manager) finishCredentialEditLocked(release func() error, result *Job) {
	if release == nil || release() == nil {
		return
	}
	const warning = "ACCOUNT_CREDENTIAL_LOCK_RELEASE: 账号编辑锁释放未确认；已保存或已受理的结果保留，请核实，不要重复提交"
	if result != nil {
		if current := m.jobs[result.ID]; current != nil {
			appendCredentialWarning(current, warning)
			m.persistCredentialWarningLocked(current)
			result.LastError = current.LastError
		}
	}
	if m.logger != nil {
		m.logger.Error("account credential edit lock cleanup failed")
	}
}

// The caller owns the edit lease, unless the current runner still owns the
// account. In that case only its attempt-local draft changes; cleanup owns the
// eventual protected write and must reacquire the OS lease first.
func (m *Manager) saveSecretsLocked(j *Job, delta map[string]any) {
	if j == nil || m.vault == nil || !hasCredentialDelta(delta) {
		return
	}
	beforeWarning := j.LastError
	defer func() {
		if j.LastError != beforeWarning {
			m.persistCredentialWarningLocked(j)
		}
	}()
	if m.active[j.ID] != nil {
		m.deferCredentialEditLocked(j, delta)
		return
	}
	_ = m.writeCredentialDeltaLocked(j, delta)
}

func (m *Manager) writeCredentialDeltaLocked(j *Job, delta map[string]any) error {
	if m.vault == nil || j == nil {
		return nil
	}
	c := credentials.Credentials{}
	if v, ok := delta["password"].(string); ok {
		c.Password = v
	}
	if v, ok := delta["totpSecret"].(string); ok {
		c.TOTPSecret = v
	}
	if v, ok := delta["proxyUrl"].(string); ok {
		c.ProxyURL = v
	}
	if v, ok := delta["mailApiUrl"].(string); ok {
		c.MailAPIURL = v
	}
	if v, ok := delta["mailRequestMethod"].(string); ok {
		c.MailMethod = v
	}
	if v, ok := delta["mailRequestBody"].(string); ok {
		c.MailBody = v
	}
	if v, ok := delta["mailRequestHeaders"].(map[string]string); ok {
		c.MailHeaders = v
	}
	if v, ok := delta["mailRequestHeaders"].(map[string]any); ok {
		c.MailHeaders = map[string]string{}
		for k, x := range v {
			if q, ok := x.(string); ok {
				c.MailHeaders[k] = q
			}
		}
	}
	if !hasCredentialDelta(delta) {
		return nil
	}
	old, loadErr := credentials.LoadContext(m.rootCtx, m.vault, j.Email)
	if loadErr != nil && !errors.Is(loadErr, os.ErrNotExist) {
		setCredentialWriteWarning(j, credentialLoadWarning)
		if m.logger != nil {
			m.logger.Error("secure credential load failed", "error", safeError(loadErr))
		}
		return errors.New(credentialLoadWarning)
	}
	if loadErr == nil {
		c.TOTPResetPending = old.TOTPResetPending
		c.TOTPResetOperationID = old.TOTPResetOperationID
		c.PendingTOTPSecret, c.PendingTOTPURI = old.PendingTOTPSecret, old.PendingTOTPURI
		c.PendingTOTPFactorID = old.PendingTOTPFactorID
		c.PendingPassword = old.PendingPassword
		if _, present := delta["password"]; !present {
			c.Password = old.Password
		}
		if _, present := delta["totpSecret"]; !present {
			c.TOTPSecret = old.TOTPSecret
		}
		if old.TOTPResetPending || m.totpBlockedLocked(j.Email) {
			c.TOTPSecret = old.TOTPSecret
			c.TOTPResetPending = true
			quarantineTOTP(j)
		}
		// A factor binding belongs to an exact saved seed. Input cannot supply
		// a new binding, and replacing/clearing a seed must discard the old ID.
		if c.TOTPSecret != "" && c.TOTPSecret == old.TOTPSecret {
			c.TOTPFactorID = old.TOTPFactorID
		}
		if _, present := delta["proxyUrl"]; !present {
			c.ProxyURL = old.ProxyURL
		}
		if _, present := delta["mailApiUrl"]; !present {
			c.MailAPIURL = old.MailAPIURL
		}
		if _, present := delta["mailRequestMethod"]; !present {
			c.MailMethod = old.MailMethod
		}
		if _, present := delta["mailRequestBody"]; !present {
			c.MailBody = old.MailBody
		}
		if _, present := delta["mailRequestHeaders"]; !present {
			c.MailHeaders = old.MailHeaders
		}
	}
	email := j.Email
	// The React console includes proxyUrl:"" for new direct-connect jobs.
	// Do not create a keychain entry and encryption key for an entirely empty
	// credential record; a previously saved account still needs its explicit
	// clears applied without retaining the old password or proxy.
	empty := emptyCredentialRecord(c)
	if empty && errors.Is(loadErr, os.ErrNotExist) {
		clearResolvedCredentialWriteWarning(j, c)
		return nil
	}
	var saveErr error
	if empty {
		saveErr = credentials.DeleteContext(m.rootCtx, m.vault, email)
		if errors.Is(saveErr, os.ErrNotExist) {
			saveErr = nil
		}
	} else {
		saveErr = credentials.SaveContext(m.rootCtx, m.vault, email, c)
	}
	if err := saveErr; err != nil {
		warning := credentialSaveWarning
		if errors.Is(err, privatefile.ErrDurabilityUncertain) {
			warning = credentialUncertainWarning
		}
		setCredentialWriteWarning(j, warning)
		if m.logger == nil {
			return errors.New(warning)
		}
		// Do not downgrade to plaintext or put the credential material in the
		// job response. The failure is observable only as a safe operational log.
		m.logger.Error("secure credential save failed", "email", redactEmail(email), "error", safeError(err))
		return errors.New(warning)
	}
	readback, readErr := credentials.LoadContext(m.rootCtx, m.vault, email)
	if empty {
		if errors.Is(readErr, os.ErrNotExist) {
			clearResolvedCredentialWriteWarning(j, c)
			return nil
		}
		setCredentialWriteWarning(j, credentialClearWarning)
		return errors.New(credentialClearWarning)
	}
	if readErr != nil || !credentials.EqualReadback(c, readback) {
		setCredentialWriteWarning(j, credentialReadbackWarning)
		return errors.New(credentialReadbackWarning)
	}
	clearResolvedCredentialWriteWarning(j, readback)
	return nil
}
