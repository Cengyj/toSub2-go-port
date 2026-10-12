package console

import (
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
	ID                 string            `json:"id"`
	Email              string            `json:"email"`
	LoginMode          string            `json:"loginMode"`
	Password           string            `json:"password"`
	TOTPSecret         string            `json:"totpSecret"`
	MailAPIURL         string            `json:"mailApiUrl"`
	MailRequestMethod  string            `json:"mailRequestMethod"`
	MailRequestBody    string            `json:"mailRequestBody"`
	MailRequestHeaders map[string]string `json:"mailRequestHeaders"`
	ProxyURL           string            `json:"proxyUrl"`
	CredentialSource   string            `json:"credentialSource"`
	TOTPStatus         string            `json:"totpStatus"`
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
	c, err := s.jobs.LoadCredentialsContext(r.Context(), id)
	source := "secure-store"
	if err != nil {
		if errors.Is(err, jobs.ErrTOTPRecoveryPending) {
			writeError(w, http.StatusConflict, "2FA 密钥正在修改或恢复未完成，暂不能查看账号资料")
			return
		}
		// A bare-email account intentionally has no credential record. Never
		// substitute process-local inputs when a saved record cannot be read.
		input, _ := s.jobs.InputCopy(id)
		inputHasDetails := false
		for _, key := range []string{"password", "totpSecret", "proxyUrl", "mailApiUrl", "mailRequestBody"} {
			value, _ := input[key].(string)
			inputHasDetails = inputHasDetails || value != ""
		}
		if errors.Is(err, os.ErrNotExist) && !inputHasDetails && !j.HasPassword && j.LoginMode != "password" && !j.HasTOTPKey && !j.TOTPKnownEnabled && !j.ProxyConfigured && j.MailAPIURL == "" {
			c, source = credentials.Credentials{}, "none"
		} else {
			writeError(w, http.StatusConflict, "当前账号资料无法从系统安全凭据存储读取，请核实后重试")
			return
		}
	}
	if c.TOTPResetPending || ((j.HasTOTPKey || j.TOTPKnownEnabled) && c.TOTPSecret == "") || ((j.HasPassword || j.LoginMode == "password") && c.Password == "") {
		writeError(w, http.StatusConflict, "当前账号资料未完整保存，暂不能安全查看，请核实后重试")
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
	writeJSON(w, http.StatusOK, accountInfoResponse{
		ID: j.ID, Email: j.Email, LoginMode: j.LoginMode,
		Password: c.Password, TOTPSecret: c.TOTPSecret, ProxyURL: c.ProxyURL,
		MailAPIURL: c.MailAPIURL, MailRequestMethod: c.MailMethod,
		MailRequestBody: c.MailBody, MailRequestHeaders: headers,
		CredentialSource: source, TOTPStatus: totpStatus,
	})
}
