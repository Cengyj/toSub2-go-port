package console

import (
	"context"
	"errors"
	"net/http"
	"os"

	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/jobs"
)

// accountInfoResponse deliberately excludes pending changes and OAuth tokens.
// This private response is requested explicitly by the account information card,
// rather than included in the public account list or its polling responses.
type accountInfoResponse struct {
	ID                    string            `json:"id"`
	Email                 string            `json:"email"`
	LoginMode             string            `json:"loginMode"`
	Password              string            `json:"password"`
	TOTPSecret            string            `json:"totpSecret"`
	MailAPIURL            string            `json:"mailApiUrl"`
	MailRequestMethod     string            `json:"mailRequestMethod"`
	MailRequestBody       string            `json:"mailRequestBody"`
	MailRequestHeaders    map[string]string `json:"mailRequestHeaders"`
	ProxyURL              string            `json:"proxyUrl"`
	CredentialSource      string            `json:"credentialSource"`
	TOTPStatus            string            `json:"totpStatus"`
	ManualExpiresAt       string            `json:"manualExpiresAt"`
	SubscriptionExpiresAt string            `json:"subscriptionExpiresAt"`
	ExpiresAt             string            `json:"expiresAt"`
	ExpiresAtSource       string            `json:"expiresAtSource"`
}

func (s *Server) accountInfo(w http.ResponseWriter, r *http.Request, id string) {
	w.Header().Set("Cache-Control", "no-store")
	j, exists := s.jobs.Get(id)
	if !exists {
		writeError(w, http.StatusNotFound, "job not found")
		return
	}
	if totpRecoveryBlocked(j) || (j.ResetTOTP && (j.Status == jobs.StatusQueued || isActiveStatus(j.Status))) {
		writeError(w, http.StatusConflict, "2FA 密钥正在修改或恢复未完成，暂不能查看账号资料")
		return
	}
	c, source, err := s.loadCurrentProfile(r.Context(), j)
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	totpStatus := "none"
	if c.TOTPSecret != "" {
		totpStatus = "saved"
	}
	headers := c.MailHeaders
	if headers == nil {
		headers = map[string]string{}
	}
	expiresAt, expiresAtSource := jobs.EffectiveExpiry(j)
	writeJSON(w, http.StatusOK, accountInfoResponse{
		ID: j.ID, Email: j.Email, LoginMode: j.LoginMode,
		Password: c.Password, TOTPSecret: c.TOTPSecret, ProxyURL: c.ProxyURL,
		MailAPIURL: c.MailAPIURL, MailRequestMethod: c.MailMethod,
		MailRequestBody: c.MailBody, MailRequestHeaders: headers,
		CredentialSource: source, TOTPStatus: totpStatus,
		ManualExpiresAt: j.ManualExpiresAt, SubscriptionExpiresAt: j.SubscriptionExpiresAt,
		ExpiresAt: expiresAt, ExpiresAtSource: expiresAtSource,
	})
}

// Information cards and source export share one saved-credential boundary.
// In-flight input and pending candidates are never substitutes for a missing
// or unreadable committed credential record.
func (s *Server) loadCurrentProfile(ctx context.Context, j *jobs.Job) (credentials.Credentials, string, error) {
	c, err := s.jobs.LoadCredentialsContext(ctx, j.ID)
	source := "secure-store"
	if err != nil {
		if errors.Is(err, jobs.ErrTOTPRecoveryPending) || errors.Is(err, jobs.ErrTOTPSetupVerificationPending) {
			return credentials.Credentials{}, "", errors.New("2FA 密钥正在修改或恢复未完成，暂不能查看或导出账号资料")
		}
		input, _ := s.jobs.InputCopy(j.ID)
		inputHasDetails := false
		for _, key := range []string{"password", "totpSecret", "proxyUrl", "mailApiUrl", "mailRequestBody"} {
			value, _ := input[key].(string)
			inputHasDetails = inputHasDetails || value != ""
		}
		if errors.Is(err, os.ErrNotExist) && !inputHasDetails && !j.HasPassword && j.LoginMode != "password" && !j.HasTOTPKey && !j.TOTPKnownEnabled && !j.ProxyConfigured && j.MailAPIURL == "" {
			c, source = credentials.Credentials{}, "none"
		} else {
			return credentials.Credentials{}, "", errors.New("当前账号资料无法从安全凭据存储读取，请核实后重试")
		}
	}
	if c.TOTPResetPending || ((j.HasTOTPKey || j.TOTPKnownEnabled) && c.TOTPSecret == "") || ((j.HasPassword || j.LoginMode == "password") && c.Password == "") {
		return credentials.Credentials{}, "", errors.New("当前账号资料未完整保存，暂不能安全查看或导出，请核实后重试")
	}
	return c, source, nil
}
