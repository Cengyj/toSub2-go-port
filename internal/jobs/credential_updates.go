package jobs

import (
	"time"

	"tosub2-go-port/internal/credentials"
)

// UpdateStoredCredentials mirrors a successful protected-store write in the
// process-local input. Later input and retry must not resurrect older passwords
// or the proxy template from before session rotation.
func (m *Manager) UpdateStoredCredentials(id string, c credentials.Credentials) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if j, ok := m.jobs[id]; ok {
		m.mirrorStoredCredentialsLocked(j, c)
		j.UpdatedAt = time.Now().UTC()
		m.persistLocked()
	}
}

// Mirror already confirmed protected material without overwriting fields that
// the operator explicitly edited during this attempt. The caller owns m.mu and
// decides when to commit metadata; this helper never writes the vault.
func (m *Manager) mirrorStoredCredentialsLocked(j *Job, c credentials.Credentials) {
	m.cancelAccountQueryLocked(j.Email)
	if j.Input == nil {
		j.Input = map[string]any{}
	}
	edited := func(key string) bool {
		attempt := m.active[j.ID]
		if attempt == nil {
			return false
		}
		_, present := attempt.credentialEdits[key]
		return present
	}
	for k, v := range map[string]any{"password": c.Password, "totpSecret": c.TOTPSecret, "proxyUrl": c.ProxyURL, "mailApiUrl": c.MailAPIURL, "mailRequestMethod": c.MailMethod, "mailRequestBody": c.MailBody, "mailRequestHeaders": c.MailHeaders} {
		if !edited(k) {
			j.Input[k] = cloneValue(v)
		}
	}
	if !edited("password") {
		j.HasPassword = c.Password != ""
		j.LoginMode = "email_otp"
		if j.HasPassword {
			j.LoginMode = "password"
		}
	}
	if !edited("totpSecret") {
		j.HasTOTPKey = c.TOTPSecret != ""
	}
	if !edited("proxyUrl") {
		j.ProxyConfigured = c.ProxyURL != ""
	}
	if !edited("mailApiUrl") {
		j.MailAPIURL = c.MailAPIURL
	}
	if c.TOTPResetPending || m.totpBlockedLocked(j.Email) {
		quarantineTOTP(j)
	}
}
