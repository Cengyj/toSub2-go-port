package protocol

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"

	"tosub2-go-port/internal/authmaterial"
	"tosub2-go-port/internal/resetlog"
	"tosub2-go-port/internal/transport"
)

type totpResetError struct {
	reason     string
	message    string
	uncertain  bool
	cause      error
	httpStatus int
}

func (e *totpResetError) Error() string { return e.message }
func (e *totpResetError) Unwrap() error { return e.cause }
func resetError(message string, uncertain bool) error {
	return &totpResetError{message: message, uncertain: uncertain}
}

func resetStop(reason, message string, uncertain bool) error {
	return &totpResetError{reason: reason, message: message, uncertain: uncertain}
}

// ResetTOTP replaces a verified current factor, or enrolls first when initial
// metadata has not provided a removal target. A definite enrollment conflict
// permits removal only after a fresh read identifies exactly one valid factor.
// Security mutations retry only when the transport proves HTTP was not sent.
// Safe status reads may retry network failures. A durable intent blocks automatic
// restart, including a restart after an ambiguous response or process crash.
func (f *Flow) ResetTOTP(ctx context.Context, resultPath string) (out map[string]any, err error) {
	if resultPath == "" || f.Options.TOTPResetBeforeDisable == nil || f.Options.TOTPResetPersist == nil || f.Options.TOTPEnrollment == nil {
		return nil, errors.New("TOTP_RESET_PERSISTENCE_REQUIRED: private journal and credential persistence callbacks are required")
	}
	if err = ValidateTOTPResetStart(resultPath, f.Options.Email); err != nil {
		return nil, err
	}
	out = map[string]any{
		"version": 1, "email": f.Options.Email, "already_enabled": false, "support_schema": 1,
		"operation_scope": "current_session", "operation_id": randomUUID(),
		"activation_evidence_schema": 2, // Complete, bound business receipt. A reconciled read writes schema 3.
		"mutation_schema":            1, "disable_dispatch": "not_sent", "enroll_dispatch": "not_sent", "activation_dispatch": "not_sent",
		"enroll_attempted": false, "activation_attempted": false,
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
	if err = f.startLoginOperation(ctx); err != nil {
		return nil, err
	}
	out["initial_login_allowed"] = true

	detach, captureErr := f.attachResetCapture(resultPath, out)
	if captureErr != nil {
		return nil, captureErr
	}
	defer detach()
	if f.Options.EventData != nil {
		f.resetLog = &resetDiagnostics{ref: resetlog.OperationRef(str(out["operation_id"]))}
		f.Client.resetAttempt = f.observeResetAttempt
		defer func() { f.Client.resetAttempt = nil }()
	}
	save := func() error { return f.saveTOTPResetJournal(resultPath, out) }
	defer func() {
		if err == nil {
			return
		}
		if verifiedResetActivation(out) && out["credential_persisted"] == true {
			out["reset_totp_status"], out["reset_totp_phase"], out["reset_totp_error"] = "succeeded", "credential_persisted", nil
			if f.Options.LogoutAllDevicesAfterTOTP && str(out["logout_all_devices_attempted_at"]) == "" {
				out["logout_all_devices_status"] = "skipped"
				out["logout_all_devices_error"] = "新 2FA 已可靠完成；退出设备前的本地提交未完成，未发送退出请求"
			}
			if saveErr := save(); saveErr != nil {
				err = errors.Join(err, saveErr)
			}
			return
		}
		out["reset_totp_status"] = "failed"
		if str(out["reset_totp_attempted_at"]) != "" && !resetRequestUnsent(out, "disable") || out["initial_enroll_attempted"] == true && !resetRequestUnsent(out, "initial_enroll") {
			out["reset_totp_status"] = "unknown"
		}
		out["reset_totp_error"] = resetFailureSummary(err)
		var failure *totpResetError
		if errors.As(err, &failure) {
			out["reset_totp_error"] = failure.message
			out["reset_totp_status"] = "failed"
			if failure.uncertain {
				out["reset_totp_status"] = "unknown"
			}
		}
		if out["activation_succeeded"] == true {
			out["reset_totp_status"] = "unknown"
		}
		if (acknowledgedResetRemoval(out) || out["old_factor_removal_confirmed_by_read"] == true) && out["activation_verified"] != true {
			out["reset_totp_error"] = "所选旧因子已确认移除，本次新因子尚未核对。" + str(out["reset_totp_error"])
		}
		if out["reset_totp_phase"] != "initial" && out["reset_totp_phase"] != "manual_review_required" {
			out["reset_totp_last_phase"] = out["reset_totp_phase"]
			out["reset_totp_phase"] = "manual_review_required"
		}
		if f.Options.LogoutAllDevicesAfterTOTP && str(out["logout_all_devices_attempted_at"]) == "" {
			out["logout_all_devices_status"] = "skipped"
			out["logout_all_devices_error"] = "Reset and new credential persistence did not complete; no logout request was sent."
		}
		code := "TOTP_RESET_MANUAL_REVIEW_REQUIRED"
		if out["reset_totp_phase"] == "initial" {
			code = "TOTP_RESET_NOT_STARTED"
			if transport.IsRetryableReadError(err) {
				out["reset_totp_error"] = transport.ReadFailureMessage(err) + "；尚未修改 2FA，可以再次执行"
			}
		}

		// Publish the final diagnosis together with the last reliable phase.
		// A write failure cannot turn in-memory evidence into durable evidence.
		if saveErr := save(); saveErr != nil {
			out["reset_totp_error"] = str(out["reset_totp_error"]) + "；操作证据保存失败，不能继续修改账号"
			err = errors.Join(err, saveErr)
		}
		err = wrapTOTPResetFailure(code, out, err)
	}()
	out["failure_site"] = "web_authorization"
	token, account, e := f.prepareResetWebAuthorization(ctx)
	if e != nil {
		err = e
		return
	}
	out["account_id"] = account
	f.decision("identity", "IDENTITY_VERIFIED", "state_read")
	out["failure_site"] = "initial_state_read"
	info, e := f.readTOTPResetStateWithRetry(ctx, token, account)
	if e != nil {
		err = e
		return
	}
	state := classifyTOTPState(info)
	out["initial_totp_state"], out["initial_totp_summary"] = state.State, state.Summary
	f.emit("totp_reset_state", "running", totpInitialStateMessage(state))
	if state.State != totpStateEnabled && !totpInitialEnrollmentAllowed(info) {
		f.decision("initial_state", "TARGET_UNRESOLVED", "state_read")
		err = resetError("登录已成功，但 2FA 信息存在多个验证器、恢复验证器或异常字段，无法确定可修改对象；未发送移除或设置请求。"+state.Summary, false)
		return
	}
	if state.State == totpStateEnabled {
		if !f.permitsResetTarget(str(state.Factors[0]["id"]), false) {
			f.decision("initial_state", "TARGET_NOT_AUTHORIZED", "state_read")
			err = resetStop("TARGET_NOT_AUTHORIZED", "当前可见因子不匹配本次登录的旧因子绑定；请核对验证器，未发送移除请求", false)
			return
		}
		out["old_totp_factor_id"] = str(state.Factors[0]["id"])
		f.resetNote(resetlog.Detail{Stage: "initial_state", Reason: "TARGET_BOUND", Source: "state_read", Expected: "F1"})
	} else {
		reason := "OPTIONAL_STATE_MISSING"
		if state.State == totpStateDisabled {
			reason = "NO_ORDINARY_TOTP"
		}
		f.decision("initial_state", reason, "state_read")
		out["reset_totp_strategy"] = "enroll_first"
		out["initial_enroll_dispatch"], out["initial_enroll_attempted"] = "not_sent", false
	}
	out["reset_totp_phase"] = "prepared"
	out["failure_site"] = "prepared_journal_write"
	if err = save(); err != nil {
		return
	}
	out["failure_site"] = "operation_readback"
	if _, err = ReadTOTPResetIdentity(resultPath, f.Options.Email); err != nil {
		return
	}
	skipRemoval := false
	if out["reset_totp_strategy"] == "enroll_first" {
		skipRemoval, err = f.tryInitialTOTPEnrollment(ctx, token, account, resultPath, out)
		if err != nil {
			return
		}
	}
	if !skipRemoval {
		if err = f.removePreparedTOTP(ctx, token, account, resultPath, out); err != nil {
			return
		}
	}
	if err = f.continueTOTPReset(ctx, token, account, resultPath, out); err != nil {
		return
	}
	f.emit("totp_reset_complete", "running", "本次激活已有确认依据，密钥已读回；正在确认任务提交")
	if f.Options.LogoutAllDevicesAfterTOTP {
		if f.Options.TOTPResetBeforeLogout != nil {
			err = f.Options.TOTPResetBeforeLogout(ctx)
			if err != nil {
				return
			}
		}
		err = f.logoutAfterTOTP(ctx, token, account, resultPath, out)
	}
	return
}

// ValidateTOTPResetStart permits a fresh operation or a recognized journal
// that stopped before mutation. Unknown/setup records and recovery material
// must never be overwritten by starting a reset in the same output directory.
func ValidateTOTPResetStart(path, email string) error {
	prior, err := ReadTOTPResetJournal(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	blocked := errors.New("TOTP_RESET_MANUAL_REVIEW_REQUIRED: previous private operation state requires review; automatic continuation is blocked")
	if err != nil || TOTPResetJournalIsReadOnly(prior) {
		return blocked
	}
	if err := validateResetMutationEvidence(prior); err != nil {
		return err
	}

	if strings.TrimSpace(email) == "" || !strings.EqualFold(strings.TrimSpace(str(prior["email"])), strings.TrimSpace(email)) {
		return blocked
	}
	if !mutationEvidenceVersion(prior) || prior["operation_scope"] != "current_session" || prior["version"] != float64(1) || prior["reset_totp_requested"] != true || prior["reset_totp_phase"] != "initial" || prior["activation_succeeded"] != false || prior["activation_verified"] != false || prior["old_factor_invalidated"] != false || prior["credential_persisted"] != false {
		return blocked
	}
	for _, key := range []string{"reset_totp_attempted_at", "logout_all_devices_attempted_at"} {
		if prior[key] != nil && prior[key] != "" {
			return blocked
		}
	}
	for _, key := range []string{"secret", "otpauth_uri", "old_totp_factor_id", "new_totp_factor_id", "activated_at", "reset_totp_last_phase", "initial_enroll_attempt_id", "initial_enroll_attempted_at", "initial_enroll_receipt", "initial_enroll_result"} {
		if _, exists := prior[key]; exists {
			return blocked
		}
	}
	return nil
}

// Account-security reads never invoke a challenge solver or replay its response.
func (f *Flow) readTOTPResetPage(ctx context.Context) (*transport.FollowResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	page, err := f.Client.Follow(ctx, f.Options.ChatGPTBase+"/?action=enable&factor=totp", RequestOptions{Referer: f.Options.ChatGPTBase + "/", StopAt: f.isCallback, DisableChallengeReplay: true})
	if errors.Is(err, ErrBrowserChallenge) {
		return page, &totpResetError{message: "TOTP_RESET_CHALLENGE_REQUIRED: 官方要求额外访问验证；未重新登录或重放账号修改请求", cause: err}
	}
	if err == nil && (page == nil || page.Response == nil) {
		return nil, resetError("当前网页未返回可读取的会话页面；尚未发送 2FA 修改请求", false)
	}
	return page, err
}

func (f *Flow) currentTOTPAuthorization(ctx context.Context, pageToken string) (string, string, error) {
	r, err := f.Client.Request(ctx, http.MethodGet, f.Options.ChatGPTBase+"/api/auth/session", RequestOptions{Referer: f.Options.ChatGPTBase + "/", DisableChallengeReplay: true, MaxResponseBytes: 256 << 10})
	if err != nil {
		return "", "", &totpResetError{message: "读取当前账号会话失败：" + transport.ReadFailureMessage(err), cause: err}
	}
	if r == nil {
		return "", "", resetError("当前会话未返回实时账号验证响应；未授权后续修改", false)
	}
	if r.StatusCode == http.StatusUnauthorized {
		return "", "", &totpResetError{message: "当前网页会话尚未登录或已失效（HTTP 401）；尚未发送 2FA 修改请求", cause: expiredSession("WEB_SESSION_LOGIN_REQUIRED")}
	}
	if r.StatusCode < 200 || r.StatusCode >= 300 {
		return "", "", resetError(fmt.Sprintf("当前网页会话验证返回 HTTP %d；未授权后续修改", r.StatusCode), false)
	}
	var session map[string]any
	if authmaterial.ValidateUnambiguousJSON(r.Body) != nil || json.Unmarshal(r.Body, &session) != nil || session == nil {
		return "", "", resetError("当前会话账号结构无法确认；未授权后续修改", false)
	}
	if len(session) == 0 {
		return "", "", &totpResetError{message: "服务器返回未登录的空会话；尚未发送 2FA 修改请求", cause: expiredSession("WEB_SESSION_LOGIN_REQUIRED")}
	}
	email := strings.TrimSpace(str(payload(session["user"])["email"]))
	if email == "" || !strings.EqualFold(email, strings.TrimSpace(f.Options.Email)) {
		return "", "", resetError("Current authenticated web account email is missing or does not match the selected account; no further factor mutation is authorized.", false)
	}
	account := payload(session["account"])
	fromSession := ""
	for _, value := range []any{account["id"], account["account_id"], session["account_id"]} {
		if value == nil {
			continue
		}
		id, valid := value.(string)
		id = strings.TrimSpace(id)
		if !valid || id == "" || fromSession != "" && fromSession != id {
			return "", "", resetError("Current authenticated session account IDs are invalid or inconsistent; no further factor mutation is authorized.", false)
		}
		fromSession = id
	}
	if fromSession == "" {
		return "", "", resetError("Current authenticated web account ID is unavailable; no further factor mutation is authorized.", false)
	}
	if err := f.confirmResetWebIdentity(); err != nil {
		return "", "", err
	}
	// Prefer the current token supplied with the live identity. A page token
	// is compatible fallback data, never an alternative to identity checks.
	token, ok := session["accessToken"].(string)
	if value, present := session["accessToken"]; present && value != nil && !ok {
		return "", "", resetError("当前网页会话的访问令牌结构无法确认；未授权后续修改", false)
	}
	token = strings.TrimSpace(token)
	if token == "" {
		token = strings.TrimSpace(pageToken)
	}
	if token == "" {
		return "", "", resetError("当前网页会话身份已确认，但会话接口和页面均未提供访问令牌；尚未发送 2FA 修改请求", false)
	}
	if claims, e := DecodeJWTClaims(token); e == nil {
		fromToken := strings.TrimSpace(str(payload(claims["https://api.openai.com/auth"])["chatgpt_account_id"]))
		if fromToken != "" && fromToken != fromSession {
			return "", "", resetError("Current web account context is inconsistent; no further factor mutation is authorized.", false)
		}
	}
	return token, fromSession, nil
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
	if read {
		// Status reconciliation owns the GET budget; do not multiply it by
		// the client's connection retry count.
		opt.SingleAttempt = f.resetCapture != nil && endpoint == "/backend-api/accounts/mfa_info"
		opt.MaxResponseBytes = 256 << 10
		opt.Headers.Set("Cache-Control", "no-cache, no-store")
		opt.Headers.Set("Pragma", "no-cache")
	}
	if method == http.MethodPost {
		opt.Origin = f.Options.ChatGPTBase
		opt.JSON = body
	}
	step := resetStepForEndpoint(endpoint)
	if step == "enroll" && f.resetCapture != nil && f.resetCapture.enrollmentStep != "" {
		step = f.resetCapture.enrollmentStep
	}
	if !read && step != "" {
		opt.MaxResponseBytes, opt.CaptureResponse = maxResetReceiptBody, true
		opt.OnResponseHeaders = func(r *transport.Response) error { return f.resetReceipt(step, r, false, nil) }
	}
	r, err := f.Client.Request(ctx, method, f.Options.ChatGPTBase+endpoint, opt)
	if opt.SingleAttempt && isBrowserChallenge(r) {
		err = ErrBrowserChallenge
	}
	if !read && r != nil && step != "" {
		if saveErr := f.resetReceipt(step, r, (r.BodyComplete || err == nil) && !isBrowserChallenge(r) && !errors.Is(err, ErrBrowserChallenge), err); saveErr != nil {
			return nil, &totpResetError{message: "TOTP_RESET_STORAGE_BLOCKED: 已收到的响应仍保留在当前进程；先重试本地保存", uncertain: true, cause: errors.Join(err, saveErr)}
		}
	}
	// Preserve received HTTP status even if the diagnostic body was interrupted.
	if errors.Is(err, ErrBrowserChallenge) {
		return nil, &totpResetError{reason: "CHALLENGE_REQUIRED", message: "账号安全请求需要额外验证；未再次发送修改请求", cause: ErrBrowserChallenge}
	}
	// A 5xx response can follow a committed mutation and remains uncertain.
	if r != nil && (r.StatusCode < 200 || r.StatusCode >= 300) {
		message := fmt.Sprintf("账号安全请求返回 HTTP %d；不会自动重试", r.StatusCode)
		if (step == "disable" || isResetEnrollmentStep(step)) && r.StatusCode == http.StatusUnauthorized && err == nil && isRecentAuthRefusal(r.Body) {
			message = totpRecentAuthMessageForStep(step)
		}
		if read {
			message = fmt.Sprintf("读取 2FA 状态返回 HTTP %d", r.StatusCode)
		}
		return nil, &totpResetError{message: message, uncertain: r.StatusCode >= 500 || r.StatusCode == http.StatusRequestTimeout, cause: err, httpStatus: r.StatusCode}
	}
	// A transport may report a late connection error after the entire response
	// was received. Its complete business body remains usable. An operation
	// evidence write failure must still preserve its local storage barrier.
	if r != nil && r.BodyComplete && !isBrowserChallenge(r) && !errors.Is(err, ErrBrowserChallenge) && !errors.Is(err, ErrResetStorageBlocked) {
		err = nil
	}
	if err != nil || r == nil {
		if read {
			if err != nil {
				return nil, &totpResetError{message: "读取 2FA 状态失败：" + transport.ReadFailureMessage(err), cause: err}
			}
			return nil, resetError("读取 2FA 状态未返回有效响应。", false)
		}
		message := "账号安全请求未获得确认响应；为避免重复修改，已停止自动重试"
		if transport.IsRequestDefinitelyUnsent(err) {
			message = transport.ReadFailureMessage(err) + "；账号安全请求尚未发送"
		}
		return nil, &totpResetError{message: message, uncertain: !transport.IsRequestDefinitelyUnsent(err), cause: err}
	}
	result, valid := decodeTOTPResponse(r.Body)
	if !valid {
		if f.resetLog != nil {
			f.decision(f.resetLog.lastStage, "RESPONSE_INVALID", "transport")
		}
		return nil, resetError("Account-security response could not be verified; it will not be retried.", true)
	}
	if !stateAccountMatches(result, account) {
		if f.resetLog != nil {
			source := "business_receipt"
			if read {
				source = "state_read"
			}
			f.decision(f.resetLog.lastStage, "IDENTITY_UNVERIFIED", source)
		}
		return nil, resetStop("IDENTITY_UNVERIFIED", "回包的账号绑定不一致，未采用该结果，停止后续修改。", false)
	}
	return result, nil
}

// Identity is established in the authenticated session. An optional echoed
// binding may be omitted, but a present null, mistyped or different ID conflicts.
func stateAccountMatches(result map[string]any, account string) bool {
	if id, present := result["account_id"]; present && id != account {
		return false
	}
	value, present := result["account"]
	if !present {
		return true
	}
	obj := payload(value)
	if obj == nil {
		return false
	}
	for _, key := range []string{"id", "account_id"} {
		if id, present := obj[key]; present && id != account {
			return false
		}
	}
	return true
}

// Local completion never dispatches logout. Preserve any earlier attempt and its
// diagnosis; disabling future execution must not rewrite historical evidence.
func skipUnattemptedLocalCompletionLogout(out map[string]any) {
	if out["logout_all_devices_requested"] != true || str(out["logout_all_devices_attempted_at"]) != "" {
		return
	}
	switch str(out["logout_all_devices_status"]) {
	case "", "pending", "skipped":
		out["logout_all_devices_status"] = "skipped"
		out["logout_all_devices_error"] = "本地保存不执行退出设备，仅完成已确认的密钥提交"
	}
}

func (f *Flow) logoutAfterTOTP(ctx context.Context, token, account, path string, result map[string]any) error {
	finish := func(status, message string) error {
		result["logout_all_devices_status"] = status
		result["logout_all_devices_error"] = nil
		if message != "" {
			result["logout_all_devices_error"] = message
		}
		if err := writeTOTPJournal(path, result); err != nil {
			f.emit("totp_logout_save_failed", "failed", "退出设备结果尚未可靠保存；保留当前结果，等待本地保存恢复")
			return err
		}
		label := map[string]string{"succeeded": "服务器已接受退出所有设备请求；其他会话的实际失效时间需受保护接口确认", "failed": "失败", "unknown": "结果待核实", "skipped": "未执行", "not_attempted": "未发送"}[status]
		if message != "" {
			label += "；" + message
		}
		f.emit("totp_logout_"+status, "running", "退出所有设备："+label)
		return nil
	}
	result["logout_all_devices_status"] = "unknown"
	result["logout_all_devices_error"] = "退出所有设备的结果尚未确认"
	result["logout_all_devices_attempted_at"] = nowRFC3339()
	if err := writeTOTPJournal(path, result); err != nil {
		return errors.Join(err, finish("skipped", "退出设备意图无法安全保存，未发送请求"))
	}
	f.checkpoint = nil
	headers := f.accountSecurityHeaders(token, account)
	r, err := f.Client.Request(ctx, http.MethodPost, f.Options.ChatGPTBase+"/backend-api/accounts/logout_all", RequestOptions{Headers: headers, Origin: f.Options.ChatGPTBase, Referer: f.Options.ChatGPTBase + "/", RetryBeforeSend: true, DisableChallengeReplay: true, DisableDefaultHeaders: true})
	if r != nil && r.BodyComplete && !isBrowserChallenge(r) && !errors.Is(err, ErrBrowserChallenge) {
		err = nil
	}
	if r != nil && r.StatusCode >= 500 {
		return finish("unknown", logoutHTTPDiagnostic(r.StatusCode, r.Body)+"；结果待核实，不会自动重试")
	}
	if r != nil && (r.StatusCode < 200 || r.StatusCode >= 300) {
		return finish("failed", logoutHTTPDiagnostic(r.StatusCode, r.Body)+"；不会自动重试")
	}
	if err != nil || r == nil {
		if transport.IsRequestDefinitelyUnsent(err) {
			return finish("not_attempted", transport.ReadFailureMessage(err)+"；退出设备请求尚未发送；不会自动重试")
		}
		message := "退出设备请求未获得确认响应"
		if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
			message = "退出设备请求已取消"
		} else if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			message = "退出设备请求超时"
		} else if errors.Is(err, ErrBrowserChallenge) {
			message = "退出设备收到挑战响应，不能确认完成"
		}
		if r != nil {
			// A received status remains useful even if reading or processing the
			// response failed. Never include the transport's private error text.
			message += fmt.Sprintf("（HTTP %d；响应处理未完成）", r.StatusCode)
		}
		return finish("unknown", message+"；结果待核实，不会自动重试")
	}
	if reason := logoutResponseIssue(r); reason != "" {
		return finish("unknown", fmt.Sprintf("退出设备响应无法确认完成（HTTP %d；%s）；结果待核实，不会自动重试", r.StatusCode, reason))
	}
	return finish("succeeded", "")
}

// Keep the original logout endpoint's compatibility rule: a completed 2xx
// response succeeds unless it contains a recognized refusal or challenge.
// This is endpoint-specific; factor activation still requires its own evidence.
// Never retain response text, field names, URLs or arbitrary header values.
func logoutResponseIssue(r *transport.Response) string {
	if isBrowserChallenge(r) {
		return "挑战响应"
	}
	body := strings.TrimSpace(r.Text())
	lower := strings.ToLower(body)
	if strings.Contains(strings.ToLower(r.Header.Get("Content-Type")), "text/html") || strings.HasPrefix(lower, "<!doctype") || strings.HasPrefix(lower, "<html") || strings.HasPrefix(lower, "<script") {
		return "HTML 响应，不是退出完成凭据"
	}
	// The original endpoint accepts empty, scalar and plain-text responses.
	// Only container-shaped bodies carry optional success/error fields.
	if !strings.HasPrefix(body, "{") && !strings.HasPrefix(body, "[") {
		return ""
	}
	if !json.Valid(r.Body) {
		return "无效 JSON，缺少退出完成凭据"
	}
	// Retain duplicate-field/depth rejection and lossless numbers; neither
	// requires the success:true object that this endpoint does not mandate.
	if authmaterial.ValidateUnambiguousJSON(r.Body) != nil {
		return "歧义 JSON 或结构超限，未采用响应字段"
	}
	decoder := json.NewDecoder(strings.NewReader(body))
	decoder.UseNumber()
	var decoded any
	if decoder.Decode(&decoded) != nil {
		return "JSON 解析失败，缺少退出完成凭据"
	}
	receipt, object := decoded.(map[string]any)
	if !object {
		return ""
	}
	if jsonTruthy(receipt["error"]) {
		if receipt["success"] == true {
			return "成功与错误字段矛盾"
		}
		return "响应包含错误字段，未确认退出完成"
	}
	if receipt["success"] == false {
		return "success=false，未确认退出完成"
	}
	return ""
}

// Match JavaScript Boolean(payload.error), including false, zero and empty
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
	case json.Number:
		// JSON.parse uses IEEE-754 numbers: underflow becomes zero, overflow
		// becomes a nonzero infinity. Preserve that truthiness for error only;
		// other numeric fields never require this conversion.
		number, err := value.Float64()
		return err != nil || number != 0
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
	label := "退出所有设备请求被拒绝"
	if status >= 500 {
		label = "退出所有设备请求返回服务端异常"
	}
	detail := fmt.Sprintf("%s（HTTP %d", label, status)
	if code != "" {
		detail += "；错误标识 " + code
	}
	return detail + "）"
}
