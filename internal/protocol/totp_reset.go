package protocol

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"tosub2-go-port/internal/transport"
)

type totpResetError struct {
	message   string
	uncertain bool
	cause     error
}

const maxTOTPJournalBytes = 1 << 20

func (e *totpResetError) Error() string { return e.message }
func (e *totpResetError) Unwrap() error { return e.cause }
func resetError(message string, uncertain bool) error {
	return &totpResetError{message: message, uncertain: uncertain}
}

// ResetTOTP replaces precisely the factor read from the current web account.
// Security mutations retry only when the transport proves HTTP was not sent.
// Safe status reads may retry network failures. A durable intent blocks automatic
// restart, including a restart after an ambiguous response or process crash.
func (f *Flow) ResetTOTP(ctx context.Context, resultPath string) (out map[string]any, err error) {
	if resultPath == "" || f.Options.TOTPResetBeforeDisable == nil || f.Options.TOTPResetPersist == nil {
		return nil, errors.New("TOTP_RESET_PERSISTENCE_REQUIRED: private journal and credential persistence callbacks are required")
	}
	if err = ValidateTOTPResetStart(resultPath, f.Options.Email); err != nil {
		return nil, err
	}
	out = map[string]any{
		"version": 1, "email": f.Options.Email, "already_enabled": false,
		"activation_mode": "automatic", "activation_succeeded": false, "activation_verified": false,
		"reset_totp_requested": true, "reset_totp_status": "pending", "reset_totp_error": nil,
		"reset_totp_phase": "initial", "reset_totp_attempted_at": nil, "old_factor_invalidated": false,
		"credential_persisted":         false,
		"logout_all_devices_requested": f.Options.LogoutAllDevicesAfterTOTP,
		"logout_all_devices_status":    "not_requested", "logout_all_devices_error": nil, "logout_all_devices_attempted_at": nil,
	}
	if f.Options.LogoutAllDevicesAfterTOTP {
		out["logout_all_devices_status"] = "skipped"
		out["logout_all_devices_error"] = "New 2FA activation and persistence are not complete."
	}
	save := func() error { return writeTOTPJournal(resultPath, out) }
	defer func() {
		if err == nil {
			return
		}
		out["reset_totp_status"] = "failed"
		out["reset_totp_error"] = "Reset state could not be safely persisted or verified; automatic continuation is blocked."
		var failure *totpResetError
		if errors.As(err, &failure) {
			out["reset_totp_error"] = failure.message
			if failure.uncertain {
				out["reset_totp_status"] = "unknown"
			}
		}
		if out["activation_succeeded"] == true {
			out["reset_totp_status"] = "unknown"
		}
		if str(out["reset_totp_attempted_at"]) != "" {
			out["reset_totp_last_phase"] = out["reset_totp_phase"]
			out["reset_totp_phase"] = "recovery_required"
			_ = f.removeTOTPCheckpoint()
		}
		if f.Options.LogoutAllDevicesAfterTOTP {
			out["logout_all_devices_status"] = "skipped"
			out["logout_all_devices_error"] = "Reset and new credential persistence did not complete; no logout request was sent."
		}
		_ = save()
		code := "TOTP_RESET_RECOVERY_REQUIRED"
		if out["reset_totp_phase"] == "initial" {
			code = "TOTP_RESET_NOT_STARTED"
			if transport.IsRetryableReadError(err) {
				out["reset_totp_error"] = transport.ReadFailureMessage(err) + "；尚未修改 2FA，可以再次执行"
				_ = save()
			}
		}
		err = fmt.Errorf("%s: %s", code, out["reset_totp_error"])
	}()
	if err = save(); err != nil {
		return
	}
	if err = f.loadCheckpoint(); err != nil {
		return
	}
	if err = f.prepareEnrollmentSession(ctx); err != nil {
		return
	}
	f.emit("totp_reset_read", "running", "Checking the current account and existing 2FA factor")
	page, e := f.follow(ctx, f.Options.ChatGPTBase+"/?action=enable&factor=totp", f.Options.ChatGPTBase+"/")
	if e != nil {
		err = e
		return
	}
	token := extractAccessToken(page.Response.Text())
	if token == "" {
		err = resetError("Current ChatGPT web access token is unavailable; no factor was removed.", false)
		return
	}
	account, e := f.currentTOTPAccount(ctx, token)
	if e != nil {
		err = e
		return
	}
	request := func(method, endpoint string, body any) (map[string]any, error) {
		return f.totpResetRequest(ctx, token, account, method, endpoint, body)
	}
	const infoEndpoint = "/backend-api/accounts/mfa_info"
	info, e := request(http.MethodGet, infoEndpoint, nil)
	if e != nil {
		err = e
		return
	}
	factors, valid := ordinaryTOTPFactors(info)
	all, _ := payload(info["factors"])["totp"].([]any)
	if !hasTOTP(info) || !valid || len(factors) != 1 || len(all) != 1 {
		err = resetError("Exactly one unambiguous active, non-recovery TOTP factor is required; no factor was removed.", false)
		return
	}
	oldID := str(factors[0]["id"])
	out["old_totp_factor_id"] = oldID
	out["reset_totp_status"] = "disabling"
	out["reset_totp_phase"] = "disable_intent"
	out["reset_totp_attempted_at"] = nowRFC3339()
	// This is a local quarantine, deliberately conservative before dispatch.
	out["old_factor_invalidated"] = true
	if err = save(); err != nil {
		return
	}
	if e = f.Options.TOTPResetBeforeDisable(ctx); e != nil {
		err = resetError("Reset intent could not be persisted; no removal request was sent.", false)
		f.abortTOTPResetBeforeSend(ctx, resultPath, &out, &err)
		return
	}
	if err = f.removeTOTPCheckpoint(); err != nil {
		f.abortTOTPResetBeforeSend(ctx, resultPath, &out, &err)
		return
	}
	f.emit("totp_reset_disable", "running", "Replacing the current 2FA factor once")
	disabled, e := request(http.MethodPost, "/backend-api/accounts/mfa/user/disable_in_house", map[string]any{"factor_id": oldID})
	if e != nil {
		err = e
		if transport.IsRequestDefinitelyUnsent(e) {
			f.abortTOTPResetBeforeSend(ctx, resultPath, &out, &err)
		}
		return
	}
	if disabled["message"] != "Factor disabled" {
		err = resetError("Removal response did not confirm the selected factor; further mutations are blocked.", true)
		return
	}
	info, e = request(http.MethodGet, infoEndpoint, nil)
	if e != nil {
		err = e
		return
	}
	all, valid = payload(info["factors"])["totp"].([]any)
	if !valid || len(all) != 0 {
		err = resetError("Removal could not be verified against the live TOTP list; further mutations are blocked.", true)
		return
	}
	out["reset_totp_status"] = "disabled"
	out["reset_totp_phase"] = "disabled_verified"
	if err = save(); err != nil {
		return
	}
	enrollment, e := request(http.MethodPost, "/backend-api/accounts/mfa/enroll", map[string]any{"factor_type": "totp", "source": "settings"})
	if e != nil {
		err = e
		return
	}
	secret := normalizeTOTPSecret(str(enrollment["secret"]))
	sid := str(enrollment["session_id"])
	factor := payload(enrollment["factor"])
	newID := str(factor["id"])
	uri, uriErr := BuildTOTPURI(f.Options.Email, secret)
	if secret == "" || strings.TrimSpace(sid) == "" || strings.TrimSpace(newID) == "" || newID == oldID || factor["factor_type"] != "totp" || factor["is_recovery"] != false || uriErr != nil {
		err = resetError("New enrollment did not contain a valid, distinct non-recovery TOTP factor; activation was blocked.", true)
		return
	}
	out["secret"], out["otpauth_uri"], out["new_totp_factor_id"] = secret, uri, newID
	out["reset_totp_phase"] = "enrolled"
	if err = save(); err != nil {
		return
	}
	if f.Options.TOTPEnrollment != nil {
		if err = f.Options.TOTPEnrollment(ctx, secret, uri); err != nil {
			return
		}
	}
	code, e := GenerateTOTP(secret, time.Now())
	if e != nil {
		err = e
		return
	}
	out["reset_totp_phase"] = "activation_intent"
	if err = save(); err != nil {
		return
	}
	activation, e := request(http.MethodPost, "/backend-api/accounts/mfa/user/activate_enrollment", map[string]any{"factor_type": "totp", "session_id": sid, "code": code, "source": "settings"})
	if e != nil {
		err = e
		return
	}
	if activation["success"] != true {
		err = resetError("New TOTP activation was not accepted; automatic retry is blocked.", false)
		return
	}
	out["activation_succeeded"] = true
	out["activated_at"] = nowRFC3339()
	if err = save(); err != nil {
		return
	}
	info, e = request(http.MethodGet, infoEndpoint, nil)
	if e != nil {
		err = e
		return
	}
	factors, valid = ordinaryTOTPFactors(info)
	all, _ = payload(info["factors"])["totp"].([]any)
	oldPresent := false
	for _, value := range all {
		if payload(value)["id"] == oldID {
			oldPresent = true
		}
	}
	if !hasTOTP(info) || !valid || len(factors) != 1 || factors[0]["id"] != newID || oldPresent {
		err = resetError("Activation response succeeded but the new live TOTP factor was not verified; recovery is required.", true)
		return
	}
	out["activation_verified"] = true
	out["reset_totp_status"] = "activated"
	out["reset_totp_phase"] = "activated_verified"
	if err = save(); err != nil {
		return
	}
	if e = f.Options.TOTPResetPersist(ctx, secret, uri); e != nil {
		err = resetError("New TOTP is active, but credential persistence was not confirmed; logout is blocked and recovery is required.", true)
		return
	}
	out["credential_persisted"] = true
	out["reset_totp_phase"] = "credential_persisted"
	out["logout_all_devices_error"] = nil
	if f.Options.LogoutAllDevicesAfterTOTP {
		out["logout_all_devices_status"] = "pending"
	}
	if err = save(); err != nil {
		return
	}
	f.emit("totp_reset_complete", "running", "New 2FA activated, verified, and securely saved")
	if f.Options.LogoutAllDevicesAfterTOTP {
		f.logoutAfterTOTP(ctx, token, account, resultPath, out)
	}
	return
}

// ValidateTOTPResetStart permits a fresh operation or a recognized journal
// that stopped before mutation. Unknown/setup records and recovery material
// must never be overwritten by starting a reset in the same output directory.
func ValidateTOTPResetStart(path, email string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	blocked := errors.New("TOTP_RECOVERY_REQUIRED: previous private operation state requires review; automatic continuation is blocked")
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxTOTPJournalBytes {
		return blocked
	}
	file, err := os.Open(path)
	if err != nil {
		return blocked
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) || !opened.Mode().IsRegular() {
		return blocked
	}
	// Limit the handle read as well: the file could grow after the size check.
	data, err := io.ReadAll(io.LimitReader(file, maxTOTPJournalBytes+1))
	var prior map[string]any
	if err != nil || len(data) > maxTOTPJournalBytes || json.Unmarshal(data, &prior) != nil || prior == nil {
		return blocked
	}
	if strings.TrimSpace(email) == "" || !strings.EqualFold(strings.TrimSpace(str(prior["email"])), strings.TrimSpace(email)) {
		return blocked
	}
	if prior["version"] != float64(1) || prior["reset_totp_requested"] != true || prior["reset_totp_phase"] != "initial" || prior["activation_succeeded"] != false || prior["activation_verified"] != false || prior["old_factor_invalidated"] != false || prior["credential_persisted"] != false {
		return blocked
	}
	for _, key := range []string{"reset_totp_attempted_at", "logout_all_devices_attempted_at"} {
		if prior[key] != nil && prior[key] != "" {
			return blocked
		}
	}
	for _, key := range []string{"secret", "otpauth_uri", "old_totp_factor_id", "new_totp_factor_id", "activated_at", "reset_totp_last_phase"} {
		if _, exists := prior[key]; exists {
			return blocked
		}
	}
	return nil
}
func writeTOTPJournal(path string, result map[string]any) error {
	if err := WritePrivateJSON(path, result); err != nil {
		return err
	}
	return syncTOTPDirectory(filepath.Dir(path))
}
func syncTOTPDirectory(dir string) error {
	// Windows cannot fsync directory handles through os.File. The file itself
	// has been flushed before atomic replacement on all supported platforms.
	if runtime.GOOS == "windows" {
		return nil
	}
	file, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer file.Close()
	return file.Sync()
}
func (f *Flow) removeTOTPCheckpoint() error {
	if f.Options.CheckpointPath != "" {
		if err := RemoveCheckpoint(f.Options.CheckpointPath); err != nil {
			return err
		}
		if err := syncTOTPDirectory(filepath.Dir(f.Options.CheckpointPath)); err != nil {
			return err
		}
	}
	f.checkpoint = nil
	return nil
}
func (f *Flow) currentTOTPAccount(ctx context.Context, token string) (string, error) {
	var fromSession, fromToken string
	r, err := f.Client.Request(ctx, http.MethodGet, f.Options.ChatGPTBase+"/api/auth/session", RequestOptions{Referer: f.Options.ChatGPTBase + "/", DisableChallengeReplay: true})
	if err != nil {
		return "", resetError("读取当前账号会话失败："+transport.ReadFailureMessage(err), false)
	}
	if err == nil && r != nil && r.StatusCode >= 200 && r.StatusCode < 300 {
		var session map[string]any
		if json.Unmarshal(r.Body, &session) == nil {
			account := payload(session["account"])
			fromSession = str(account["id"])
			if fromSession == "" {
				fromSession = str(account["account_id"])
			}
			if fromSession == "" {
				fromSession = str(session["account_id"])
			}
			if email := str(payload(session["user"])["email"]); email != "" && !strings.EqualFold(strings.TrimSpace(email), strings.TrimSpace(f.Options.Email)) {
				return "", resetError("Current authenticated web account email does not match the selected account; no factor was removed.", false)
			}
		}
	}
	if claims, e := DecodeJWTClaims(token); e == nil {
		fromToken = str(payload(claims["https://api.openai.com/auth"])["chatgpt_account_id"])
	}
	fromSession, fromToken = strings.TrimSpace(fromSession), strings.TrimSpace(fromToken)
	if fromSession != "" && fromToken != "" && fromSession != fromToken {
		return "", resetError("Current web account context is inconsistent; no factor was removed.", false)
	}
	if fromSession != "" {
		return fromSession, nil
	}
	if fromToken != "" {
		return fromToken, nil
	}
	return "", resetError("Current authenticated web account ID is unavailable; no factor was removed.", false)
}
func ordinaryTOTPFactors(info map[string]any) ([]map[string]any, bool) {
	all, ok := payload(info["factors"])["totp"].([]any)
	if !ok {
		return nil, false
	}
	out := []map[string]any{}
	for _, value := range all {
		factor := payload(value)
		if factor["factor_type"] == "totp" && factor["is_recovery"] == false && strings.TrimSpace(str(factor["id"])) != "" {
			out = append(out, factor)
		}
	}
	return out, true
}
func (f *Flow) accountSecurityHeaders(token, account string) http.Header {
	h := http.Header{}
	h.Set("Authorization", "Bearer "+token)
	h.Set("Accept", "*/*")
	h.Set("User-Agent", f.Client.UserAgent)
	h.Set("Accept-Language", f.Client.AcceptLanguage)
	h.Set("Sec-Fetch-Site", "same-origin")
	h.Set("Sec-Fetch-Mode", "cors")
	h.Set("Sec-Fetch-Dest", "empty")
	h.Set("Priority", "u=1, i")
	device := f.Client.Jar.Value("oai-did", f.Options.ChatGPTBase+"/")
	if device == "" {
		device = f.deviceID
	}
	h.Set("Oai-Did", device)
	locale := strings.Split(strings.Split(f.Client.AcceptLanguage, ",")[0], ";")[0]
	if locale == "" {
		locale = "en-US"
	}
	h.Set("Oai-Language", locale)
	h.Set("Originator", "web")
	h.Set("Chatgpt-Account-Id", account)
	return h
}
func (f *Flow) totpResetRequest(ctx context.Context, token, account, method, endpoint string, body any) (map[string]any, error) {
	read := method == http.MethodGet || method == http.MethodHead
	opt := RequestOptions{Headers: f.accountSecurityHeaders(token, account), Referer: f.Options.ChatGPTBase + "/", RetryBeforeSend: !read, DisableChallengeReplay: true, DisableDefaultHeaders: true}
	if method == http.MethodPost {
		opt.Origin = f.Options.ChatGPTBase
		opt.JSON = body
	}
	r, err := f.Client.Request(ctx, method, f.Options.ChatGPTBase+endpoint, opt)
	// Rejection headers are conclusive even if their diagnostic body was
	// interrupted. Do not turn an HTTP refusal into an unknown network result.
	if r != nil && (r.StatusCode < 200 || r.StatusCode >= 300) {
		return nil, resetError(fmt.Sprintf("账号安全请求返回 HTTP %d；不会自动重试", r.StatusCode), r.StatusCode >= 500)
	}
	if err != nil || r == nil {
		if read {
			if err != nil {
				return nil, resetError("读取 2FA 状态失败："+transport.ReadFailureMessage(err), false)
			}
			return nil, resetError("读取 2FA 状态未返回有效响应。", false)
		}
		message := "账号安全请求未获得确认响应；为避免重复修改，已停止自动重试"
		if transport.IsRequestDefinitelyUnsent(err) {
			message = transport.ReadFailureMessage(err) + "；账号安全请求尚未发送"
		}
		return nil, &totpResetError{message: message, uncertain: !transport.IsRequestDefinitelyUnsent(err), cause: err}
	}
	var result map[string]any
	if json.Unmarshal(r.Body, &result) != nil || result == nil {
		return nil, resetError("Account-security response could not be verified; it will not be retried.", true)
	}
	return result, nil
}
func (f *Flow) logoutAfterTOTP(ctx context.Context, token, account, path string, result map[string]any) {
	finish := func(status, message string) {
		result["logout_all_devices_status"] = status
		result["logout_all_devices_error"] = nil
		if message != "" {
			result["logout_all_devices_error"] = message
		}
		_ = writeTOTPJournal(path, result)
		label := map[string]string{"succeeded": "已完成", "failed": "失败", "unknown": "结果待核实", "skipped": "未执行", "not_attempted": "未发送"}[status]
		if message != "" {
			label += "；" + message
		}
		f.emit("totp_logout_"+status, "running", "退出所有设备："+label)
	}
	result["logout_all_devices_status"] = "unknown"
	result["logout_all_devices_error"] = "退出所有设备的结果尚未确认"
	result["logout_all_devices_attempted_at"] = nowRFC3339()
	if err := writeTOTPJournal(path, result); err != nil {
		finish("skipped", "退出设备意图无法安全保存，未发送请求")
		return
	}
	if err := f.removeTOTPCheckpoint(); err != nil {
		finish("skipped", "保存的会话无法失效，未发送退出设备请求")
		return
	}
	headers := f.accountSecurityHeaders(token, account)
	r, err := f.Client.Request(ctx, http.MethodPost, f.Options.ChatGPTBase+"/backend-api/accounts/logout_all", RequestOptions{Headers: headers, Origin: f.Options.ChatGPTBase, Referer: f.Options.ChatGPTBase + "/", RetryBeforeSend: true, DisableChallengeReplay: true, DisableDefaultHeaders: true})
	if r != nil && r.StatusCode >= 500 {
		finish("unknown", logoutHTTPDiagnostic(r.StatusCode, r.Body)+"；结果待核实，不会自动重试")
		return
	}
	if r != nil && (r.StatusCode < 200 || r.StatusCode >= 300) {
		finish("failed", logoutHTTPDiagnostic(r.StatusCode, r.Body)+"；不会自动重试")
		return
	}
	if err != nil || r == nil {
		if transport.IsRequestDefinitelyUnsent(err) {
			finish("not_attempted", transport.ReadFailureMessage(err)+"；退出设备请求尚未发送，可重新执行")
			return
		}
		message := "退出设备请求未获得确认响应，结果待核实；不会自动重试"
		if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
			message = "退出设备请求已取消，结果待核实；不会自动重试"
		} else if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			message = "退出设备请求超时，结果待核实；不会自动重试"
		}
		finish("unknown", message)
		return
	}
	body := strings.TrimSpace(r.Text())
	lower := strings.ToLower(body)
	ambiguous := strings.Contains(strings.ToLower(r.Header.Get("Content-Type")), "text/html") || strings.HasPrefix(lower, "<!doctype") || strings.HasPrefix(lower, "<html") || strings.HasPrefix(lower, "<script") || isBrowserChallenge(r)
	if strings.HasPrefix(body, "{") || strings.HasPrefix(body, "[") {
		var data any
		if json.Unmarshal(r.Body, &data) != nil {
			ambiguous = true
		} else if m, ok := data.(map[string]any); ok {
			ambiguous = ambiguous || m["success"] == false || jsonTruthy(m["error"])
		}
	}
	if ambiguous {
		finish("unknown", "退出设备响应无法确认完成，结果待核实；不会自动重试")
		return
	}
	finish("succeeded", "")
}

// Match the original Boolean(payload.error), including false, zero and empty
// strings. JSON objects and arrays are truthy even when they are empty.
func jsonTruthy(value any) bool {
	switch value := value.(type) {
	case nil:
		return false
	case bool:
		return value
	case string:
		return value != ""
	case float64:
		return value != 0
	default:
		return true
	}
}

// Only fixed, recognized error identifiers enter public status or logs. Remote
// messages and HTML can echo credentials and must never be retained here.
func logoutHTTPDiagnostic(status int, body []byte) string {
	code := ""
	if len(body) <= 64<<10 {
		code = newHTTPError(status, body).Code
	}
	if code == "" && len(body) <= 64<<10 {
		var data map[string]any
		if json.Unmarshal(body, &data) == nil {
			nested := payload(data["error"])
			for _, candidate := range []string{str(nested["code"]), str(data["code"]), str(nested["type"]), str(data["type"]), str(data["error"])} {
				switch candidate {
				case "invalid_request", "invalid_request_error", "bad_request", "invalid_header", "invalid_headers", "duplicate_content_length", "invalid_content_length", "unauthorized", "unauthenticated", "invalid_token", "expired_token", "csrf_failed", "csrf_token_invalid", "invalid_csrf_token", "too_many_requests", "rate_limit_exceeded":
					code = candidate
				}
				if code != "" {
					break
				}
			}
		}
	}
	detail := fmt.Sprintf("退出所有设备请求被拒绝（HTTP %d", status)
	if code != "" {
		detail += "；错误标识 " + code
	}
	return detail + "）"
}
