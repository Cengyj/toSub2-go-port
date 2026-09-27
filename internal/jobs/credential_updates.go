package jobs

import "tosub2-go-port/internal/credentials"

// UpdateStoredCredentials mirrors a successful protected-store write in the
// process-local input. Later input and retry must not resurrect older passwords
// or the proxy template from before session rotation.
func (m *Manager) UpdateStoredCredentials(id string, c credentials.Credentials) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if j, ok := m.jobs[id]; ok {
		if j.Input == nil {
			j.Input = map[string]any{}
		}
		for k, v := range map[string]any{"password": c.Password, "totpSecret": c.TOTPSecret, "proxyUrl": c.ProxyURL, "mailApiUrl": c.MailAPIURL, "mailRequestMethod": c.MailMethod, "mailRequestBody": c.MailBody, "mailRequestHeaders": c.MailHeaders} {
			j.Input[k] = cloneValue(v)
		}
		j.HasPassword = c.Password != ""
		j.LoginMode = "email_otp"
		if j.HasPassword {
			j.LoginMode = "password"
		}
		j.HasTOTPKey = c.TOTPSecret != ""
		j.ProxyConfigured = c.ProxyURL != ""
		j.MailAPIURL = c.MailAPIURL
		m.persistLocked()
	}
}
