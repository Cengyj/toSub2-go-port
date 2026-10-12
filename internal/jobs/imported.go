package jobs

import (
	"errors"
	"os"
	"strings"

	"tosub2-go-port/internal/credentials"
)

// Validate every recorded requirement before modifying the imported account.
// In particular, an empty proxy from the console must not discard its saved
// proxy, and an unavailable vault must not turn a password login into OTP.
func (m *Manager) prepareImportedActionLocked(j *Job, values map[string]any) (map[string]any, error) {
	resolved := cloneMap(j.Input)
	if resolved == nil {
		resolved = map[string]any{}
	}
	get := func(key string) string { value, _ := resolved[key].(string); return value }
	missing := func() bool {
		return (j.HasPassword && get("password") == "") || (j.HasTOTPKey && get("totpSecret") == "") || (j.ProxyConfigured && get("proxyUrl") == "")
	}
	if missing() && m.vault != nil {
		saved, err := credentials.LoadContext(m.rootCtx, m.vault, j.Email)
		if saved.TOTPResetPending {
			return nil, ErrTOTPRecoveryPending
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, errors.New("无法读取系统凭据，请重新导入或解锁凭据存储后再开始任务")
		}
		for key, value := range map[string]string{"password": saved.Password, "totpSecret": saved.TOTPSecret, "proxyUrl": saved.ProxyURL, "mailApiUrl": saved.MailAPIURL, "mailRequestBody": saved.MailBody, "mailRequestMethod": saved.MailMethod} {
			if get(key) == "" && value != "" {
				resolved[key] = value
			}
		}
		if len(saved.MailHeaders) > 0 && resolved["mailRequestHeaders"] == nil {
			resolved["mailRequestHeaders"] = saved.MailHeaders
		}
	}
	for key, value := range values {
		if key == "proxyUrl" && value == "" {
			continue
		}
		resolved[key] = cloneValue(value)
	}
	var required []string
	for _, field := range []struct {
		needed     bool
		key, label string
	}{
		{j.HasPassword, "password", "密码"}, {j.HasTOTPKey, "totpSecret", "2FA 密钥"}, {j.ProxyConfigured, "proxyUrl", "代理地址"},
	} {
		if field.needed && get(field.key) == "" {
			required = append(required, field.label)
		}
	}
	if len(required) > 0 {
		return nil, errors.New("已记录的" + strings.Join(required, "、") + "无法恢复，请重新导入或填写后再开始任务")
	}
	return resolved, nil
}
