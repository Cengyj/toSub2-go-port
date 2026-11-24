package protocol

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf16"

	"tosub2-go-port/internal/transport"
)

type PasswordAddOutcome string

const (
	PasswordAdded         PasswordAddOutcome = "added"
	PasswordRecovered     PasswordAddOutcome = "recovered"
	PasswordAlreadyExists PasswordAddOutcome = "already_exists"
)

// PasswordAddResult contains no password. A freshly verified existing password
// is saved through PasswordVerified, independently of the candidate to add.
type PasswordAddResult struct {
	Outcome PasswordAddOutcome
}

// AddPassword performs the post-login password enrollment flow. Passwords are
// accepted only in memory and the result is written with mode 0600.
func (f *Flow) AddPassword(ctx context.Context, newPassword, resultPath string) (PasswordAddResult, error) {
	if !validNewPassword(newPassword) {
		return PasswordAddResult{}, errors.New("password must be 12-128 chars with upper/lower/digit/symbol")
	}
	prior, err := ReadPasswordAddRecovery(resultPath, f.Options.Email)
	if err != nil {
		return PasswordAddResult{}, err
	}
	if prior != nil {
		if prior.Password != newPassword {
			return PasswordAddResult{}, errors.New("PASSWORD_ADD_CANDIDATE_CONFLICT: 待完成的密码与本次输入不同，保留原证据，未发起远端请求")
		}
		return PasswordAddResult{Outcome: PasswordRecovered}, f.finishPasswordAddedWithOperation(newPassword, resultPath, prior.OperationID, prior.AddedAt)
	}
	if err := f.startLoginOperation(ctx); err != nil {
		return PasswordAddResult{}, err
	}
	defer func() { f.verifiedPassword = "" }()
	if _, err := f.LoginWeb(ctx); err != nil {
		return PasswordAddResult{}, err
	}
	if f.verifiedPassword == newPassword {
		err := f.finishPasswordAdded(newPassword, resultPath)
		if err == nil {
			f.emit("password_recovered", "running", "Current login confirmed the requested password")
		}
		return PasswordAddResult{Outcome: PasswordRecovered}, err
	}
	if f.verifiedPassword != "" {
		f.emit("password_already_exists", "running", "本次登录已验证账号现有密码，已跳过添加密码")
		return PasswordAddResult{Outcome: PasswordAlreadyExists}, nil
	}
	if err := f.addPasswordAttempt(ctx, newPassword, resultPath); err != nil {
		return PasswordAddResult{}, err
	}
	return PasswordAddResult{Outcome: PasswordAdded}, nil
}

func (f *Flow) addPasswordAttempt(ctx context.Context, newPassword, resultPath string) error {
	var csrf struct {
		Token string `json:"csrfToken"`
	}
	if _, e := f.Client.JSON(ctx, http.MethodGet, f.Options.ChatGPTBase+"/api/auth/csrf", nil, RequestOptions{Referer: f.Options.ChatGPTBase + "/"}, &csrf); e != nil {
		return e
	}
	if csrf.Token == "" {
		return errors.New("ADD_PASSWORD_CSRF_MISSING")
	}
	q := url.Values{"connection": {"password"}, "login_hint": {f.Options.Email}, "reauth": {"password"}, "post_login_add_password": {"true"}, "max_age": {"0"}, "ext-oai-did": {f.deviceID}}
	var signin struct {
		URL string `json:"url"`
	}
	if e := f.beforeEmailOTP(ctx); e != nil {
		return e
	}
	if _, e := f.Client.JSON(ctx, http.MethodPost, f.Options.ChatGPTBase+"/api/auth/signin/openai?"+q.Encode(), nil, RequestOptions{Origin: f.Options.ChatGPTBase, Referer: f.Options.ChatGPTBase + "/", Form: url.Values{"callbackUrl": {f.Options.ChatGPTBase + "/"}, "csrfToken": {csrf.Token}, "json": {"true"}}}, &signin); e != nil {
		return e
	}
	if signin.URL == "" {
		return errors.New("ADD_PASSWORD_AUTHORIZE_URL_MISSING")
	}
	authRequestedAt := time.Now().UTC()
	page, e := f.follow(ctx, signin.URL, f.Options.ChatGPTBase+"/")
	if e != nil {
		return e
	}
	ref := page.FinalURL
	if pathOf(ref) == "/email-verification" {
		f.emailOTPRequested(authRequestedAt)
		validated, e := f.verifyEmail(ctx, ref)
		if e != nil {
			return e
		}
		ref = continuation(validated)
		if ref == "" {
			ref = f.Options.AuthBase + "/reset-password/new-password"
		}
		if f.isLoginURL(ref) {
			return expiredSession("ADD_PASSWORD_LOGIN_REQUIRED")
		}
		if pathOf(ref) != "/reset-password/new-password" {
			return errors.New("ADD_PASSWORD_FLOW_INVALID: expected new-password page")
		}
		continued, followErr := f.follow(ctx, ref, page.FinalURL)
		if followErr != nil {
			return followErr
		}
		if f.isLoginURL(continued.FinalURL) {
			return expiredSession("ADD_PASSWORD_LOGIN_REQUIRED")
		}
		if pathOf(continued.FinalURL) != "/reset-password/new-password" {
			return errors.New("ADD_PASSWORD_FLOW_INVALID: expected new-password page")
		}
		ref = continued.FinalURL
	} else if f.isLoginURL(ref) {
		return expiredSession("ADD_PASSWORD_LOGIN_REQUIRED")
	} else if pathOf(ref) != "/reset-password/new-password" {
		return errors.New("ADD_PASSWORD_PAGE_INVALID: expected new-password or email verification page")
	}

	h, e := f.sentinel(ctx, "password_reset")
	if e != nil {
		return e
	}
	var data authPayload
	intent := PasswordAddRecovery{Version: 1, OperationID: randomUUID(), Email: f.Options.Email, Password: newPassword, State: "may_have_been_sent"}
	if e = ctx.Err(); e != nil {
		return e
	}
	if e = writePasswordAddIntent(resultPath, intent); e != nil {
		return e
	}
	response, e := f.Client.JSON(ctx, http.MethodPost, f.Options.AuthBase+"/api/accounts/password/add", map[string]any{"password": newPassword}, RequestOptions{Headers: h, Origin: f.Options.AuthBase, Referer: ref, DisableChallengeReplay: true}, &data)
	if e != nil {
		if transport.IsRequestDefinitelyUnsent(e) {
			intent.State = "not_sent"
		} else if response != nil && response.StatusCode >= 400 && response.StatusCode < 500 && response.StatusCode != http.StatusRequestTimeout {
			intent.State = "rejected"
		}
		if intent.State != "may_have_been_sent" {
			return errors.Join(e, writePasswordAddIntent(resultPath, intent))
		}
		return e
	}
	// A next-page hint does not override an explicit business rejection. Keep
	// the existing successful continuation/external_url shapes and JSON-false
	// error values, but never publish a rejected password as confirmed.
	if authenticationReceiptRejected(data) {
		intent.State = "rejected"
		return errors.Join(errors.New("ADD_PASSWORD_RESULT_REJECTED: password addition was refused"), writePasswordAddIntent(resultPath, intent))
	}
	if continuation(data) == "" && pageType(data) != "external_url" {
		return errors.New("ADD_PASSWORD_RESULT_INVALID")
	}
	intent.State, intent.AddedAt = "succeeded", nowRFC3339()
	// Both records describe the same confirmed operation. If one local write
	// fails, the other can still protect a local-only completion after restart.
	journalErr := writePasswordAddIntent(resultPath, intent)
	return errors.Join(journalErr, f.finishPasswordAddedWithOperation(newPassword, resultPath, intent.OperationID, intent.AddedAt))
}

func (f *Flow) finishPasswordAdded(newPassword, resultPath string) error {
	return f.finishPasswordAddedWithOperation(newPassword, resultPath, "", nowRFC3339())
}

func (f *Flow) finishPasswordAddedWithOperation(newPassword, resultPath, operationID, addedAt string) error {
	if resultPath != "" {
		value := map[string]any{"version": 1, "email": f.Options.Email, "password": newPassword, "added_at": addedAt}
		if operationID != "" {
			value["operation_id"] = operationID
		}
		if err := WritePrivateJSON(resultPath, value); err != nil {
			return err
		}
	}
	f.recordProgress("email_verified", nil)
	return nil
}

func validNewPassword(password string) bool {
	n := len(utf16.Encode([]rune(password)))
	symbol := strings.IndexFunc(password, func(r rune) bool { return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') }) >= 0
	return n >= 12 && n <= 128 && strings.ContainsAny(password, "abcdefghijklmnopqrstuvwxyz") && strings.ContainsAny(password, "ABCDEFGHIJKLMNOPQRSTUVWXYZ") && strings.ContainsAny(password, "0123456789") && symbol
}
