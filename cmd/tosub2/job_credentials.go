package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/jobs"
)

func prepareOperationCredentials(ctx context.Context, manager *jobs.Manager, credStore credentials.Store, job *jobs.Job, op string, vals map[string]any) (credentials.Credentials, error) {
	savedCredentials, credentialErr := credentials.LoadContext(ctx, credStore, job.Email)
	if credentialErr != nil && !errors.Is(credentialErr, os.ErrNotExist) {
		return credentials.Credentials{}, fmt.Errorf("secure credentials could not be loaded: %w", credentialErr)
	}
	// Workflow steps reacquire the OS account lease independently. A second
	// process can complete a reset between steps; never reuse the manager's
	// mirrored seed or save it over that newer credential during re-login.
	if op == "relogin" && job.TOTPWorkflowStatus == "running" {
		expectedSeed, _ := vals["totpSecret"].(string)
		// The runner receives a re-login clone. The manager retains the real
		// workflow source; a later setup must not inherit an old reset receipt.
		workflow, exists := manager.Get(job.ID)
		resetMismatch := exists && workflow.ResetTOTP && (job.TOTPResetOperationID == "" || savedCredentials.TOTPResetOperationID != job.TOTPResetOperationID)
		if !exists || credentialErr != nil || savedCredentials.TOTPResetPending || savedCredentials.PendingTOTPSecret != "" || savedCredentials.TOTPSecret == "" || resetMismatch || expectedSeed != "" && expectedSeed != savedCredentials.TOTPSecret {
			return credentials.Credentials{}, errors.New("TOTP_WORKFLOW_CREDENTIAL_CHANGED: 当前凭据与本次 2FA 流程不一致，已停止自动重登和上传；未覆盖已保存凭据")
		}
	}
	if savedCredentials.TOTPResetPending {
		return credentials.Credentials{}, errors.New("TOTP_RECOVERY_REQUIRED: 2FA 重置仍有待处理进度，已停止自动使用旧密钥")
	}
	if credStore != nil {
		if saved, e := savedCredentials, credentialErr; e == nil {
			if _, ok := vals["password"]; !ok && saved.Password != "" {
				vals["password"] = saved.Password
			}
			if _, ok := vals["totpSecret"]; !ok && saved.TOTPSecret != "" {
				vals["totpSecret"] = saved.TOTPSecret
			}
			if _, ok := vals["proxyUrl"]; !ok && saved.ProxyURL != "" {
				vals["proxyUrl"] = saved.ProxyURL
			}
			if _, ok := vals["mailApiUrl"]; !ok && saved.MailAPIURL != "" {
				vals["mailApiUrl"] = saved.MailAPIURL
			}
			if _, ok := vals["mailRequestMethod"]; !ok && saved.MailMethod != "" {
				vals["mailRequestMethod"] = saved.MailMethod
			}
			if _, ok := vals["mailRequestBody"]; !ok && saved.MailBody != "" {
				vals["mailRequestBody"] = saved.MailBody
			}
			if _, ok := vals["mailRequestHeaders"]; !ok && saved.MailHeaders != nil {
				vals["mailRequestHeaders"] = saved.MailHeaders
			}
		}
	}
	return savedCredentials, nil
}

func operationCredentialProtector(ctx context.Context, credStore credentials.Store, email string, verifyReadback bool, accepted func(credentials.Credentials)) func(credentials.Credentials, bool, bool) error {
	return func(value credentials.Credentials, clearPendingTOTP, clearPendingPassword bool) error {
		// JSON-backed vaults omit an empty header map. Compare the canonical
		// value on readback so an empty map is not mistaken for a lost secret.
		if len(value.MailHeaders) == 0 {
			value.MailHeaders = nil
		}
		old, e := credentials.LoadContext(ctx, credStore, email)
		if e != nil && !errors.Is(e, os.ErrNotExist) {
			return e
		}
		value.TOTPResetPending = old.TOTPResetPending
		value.TOTPResetOperationID = old.TOTPResetOperationID
		// An unrelated password/proxy update may omit the binding, but a new
		// seed must never inherit the previous seed's factor ID.
		if value.TOTPSecret == "" {
			value.TOTPFactorID = ""
		} else if value.TOTPFactorID == "" && value.TOTPSecret == old.TOTPSecret {
			value.TOTPFactorID = old.TOTPFactorID
		}
		if !clearPendingTOTP && value.PendingTOTPSecret == "" {
			value.PendingTOTPSecret, value.PendingTOTPURI, value.PendingTOTPFactorID = old.PendingTOTPSecret, old.PendingTOTPURI, old.PendingTOTPFactorID
		}
		if value.PendingTOTPSecret == "" {
			value.PendingTOTPFactorID = ""
		} else if value.PendingTOTPFactorID == "" && value.PendingTOTPSecret == old.PendingTOTPSecret {
			value.PendingTOTPFactorID = old.PendingTOTPFactorID
		}
		if !clearPendingPassword && value.PendingPassword == "" {
			value.PendingPassword = old.PendingPassword
		}
		if e = credentials.SaveContext(ctx, credStore, email, value); e != nil {
			return e
		}
		if verifyReadback {
			readback, readErr := credentials.LoadContext(ctx, credStore, email)
			if readErr != nil || !credentials.EqualReadback(value, readback) {
				return errors.New("TOTP_SETUP_PERSISTENCE_REQUIRED: 2FA 凭据保存后读回不一致，未报告设置完成")
			}
		}
		accepted(value)
		return nil
	}
}

// Inputs may replace the saved seed. Its binding is usable only for an exact
// material match, never merely because both values belong to the same account.
func credentialTOTPFactorID(saved credentials.Credentials, candidate string) string {
	if candidate == "" {
		return ""
	}
	if candidate == saved.PendingTOTPSecret {
		return saved.PendingTOTPFactorID
	}
	if candidate == saved.TOTPSecret {
		return saved.TOTPFactorID
	}
	return ""
}
