package protocol

import (
	"context"
)

func (f *Flow) prepareResetWebAuthorization(ctx context.Context) (string, string, error) {
	if f.resetCapture == nil || !canFinishInitialResetLogin(f.resetCapture.out) {
		return "", "", resetError("Current reset has no unmodified initial operation", false)
	}
	if err := f.loginForTOTPReset(ctx); err != nil {
		return "", "", err
	}
	f.emit("totp_reset_identity", "running", "网页登录流程已完成，正在核对当前账号身份")
	page, err := f.readTOTPResetPage(ctx)
	if err != nil {
		return "", "", err
	}
	if f.isLoginURL(page.FinalURL) {
		return "", "", expiredSession("TOTP_LOGIN_REQUIRED")
	}
	token, account, err := f.currentTOTPAuthorization(ctx, extractAccessToken(page.Response.Text()))
	if err != nil {
		return "", "", err
	}
	f.resetCapture.out["login_completed"] = true
	f.emit("totp_login_complete", "running", "账号登录成功，当前账号身份已核对")
	f.emit("totp_reset_read", "running", "登录已成功，正在读取当前账号的 2FA 设置")
	return token, account, nil
}

func (f *Flow) loginForTOTPReset(ctx context.Context) error {
	_, err := f.LoginWeb(ctx)
	if err != nil {
		return &totpResetError{message: "首次网页登录未完成；尚未修改 2FA", cause: err}
	}
	return f.confirmResetWebIdentity()
}

// Close the first-login allowance durably before a subsequent page, factor
// read or token error can leave another initial preparation record behind.
func (f *Flow) confirmResetWebIdentity() error {
	if f.resetCapture == nil || f.resetCapture.out["initial_login_allowed"] != true {
		return nil
	}
	f.resetCapture.out["initial_login_allowed"] = false

	return f.saveTOTPResetJournal(f.resetCapture.path, f.resetCapture.out)
}

// Only this Flow's pre-mutation evidence can authorize its initial login.
func canFinishInitialResetLogin(out map[string]any) bool {
	if out["operation_scope"] != "current_session" || !mutationEvidenceVersion(out) || validateResetMutationEvidence(out) != nil || out["reset_totp_phase"] != "initial" {
		return false
	}
	for _, step := range []string{"disable", "enroll", "activation"} {
		if out[step+"_dispatch"] != "not_sent" {
			return false
		}
	}
	return out["old_factor_invalidated"] == false && out["enroll_attempted"] == false && out["activation_attempted"] == false
}
