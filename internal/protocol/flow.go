package protocol

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf16"

	"tosub2-go-port/internal/transport"
)

const DefaultCodexClientID = "app_EMoamEEZ73f0CkXaXp7hrann"
const DefaultCodexRedirectURI = "http://localhost:1455/auth/callback"

// FlowOptions contains secrets only in memory. SaveCheckpoint writes cookies and
// OAuth state to the private checkpoint, never to job metadata or events.
type FlowOptions struct {
	AuthorizationMethod string
	// CheckpointRestoreManaged means the caller has already decided whether
	// to Restore. A checkpoint output path alone must not authorize reading it.
	CheckpointRestoreManaged                                  bool
	OutputMode, SessionOutputPath                             string
	WebOnly                                                   bool
	ChatGPTBase, AuthBase, Email, Password, TOTPSecret, Phone string
	ClientID, RedirectURI, CheckpointPath, OutputPath         string
	WorkspaceID                                               string
	AccountName                                               string
	Concurrency, Priority                                     int
	RateMultiplier                                            float64
	Input                                                     func(context.Context, string, string) (string, error)
	Event                                                     func(stage, status, prompt string)
	// EventData, when configured, receives every event in place of Event.
	// Data contains only explicitly public state, never codes or credentials.
	EventData      func(stage, status, prompt string, data map[string]any)
	Sentinel       func(context.Context, string, string, string) (http.Header, error)
	SaveCheckpoint func(Checkpoint) error
	// Profile may contain an authorized account's name and birthdate. When
	// absent, incomplete accounts receive the legacy generated adult profile.
	Profile        map[string]any
	AuthStep       func(requirement string, automatic bool)
	TOTPEnrollment func(context.Context, string, string) error
	// Reset callbacks must durably quarantine the old credential before removal,
	// and save/read back the replacement only after live activation verification.
	TOTPResetBeforeDisable func(context.Context) error
	// Abort is local persistence only. The caller invokes it only after proving
	// that no factor-removal request was sent and restoring the initial journal.
	TOTPResetAbortBeforeSend  func(context.Context) error
	TOTPResetPersist          func(context.Context, string, string) error
	LogoutAllDevicesAfterTOTP bool
	// TOTPVerified commits a candidate seed only after the server accepts a
	// code generated from it. Manually entered codes cannot prove that seed.
	TOTPVerified func(context.Context, string) error
	// PasswordVerified commits the exact password accepted by the server,
	// including manual input after a rejected stored candidate.
	PasswordVerified func(context.Context, string) error
	// EmailOTPRequested preserves the original request time for mail polling;
	// page navigation and browser challenges can take longer than a poll window.
	EmailOTPRequested func(time.Time)
	// BeforeEmailOTP snapshots existing mailbox candidates before a request
	// can send a new OTP. It is not called when resuming an outstanding OTP.
	BeforeEmailOTP func(context.Context) error
}

type Flow struct {
	Client     *Client
	Options    FlowOptions
	checkpoint *Checkpoint
	web        map[string]any
	deviceID   string
	// Retained only in memory; an accepted password can resolve an interrupted
	// password-add result after the rest of that login has completed.
	verifiedPassword string
}
type FlowResult struct {
	Web    map[string]any
	OAuth  map[string]any
	Export Sub2APIOAuthExport
}
type authPayload map[string]any

func NewFlow(client *Client, options FlowOptions) *Flow {
	if client == nil {
		client = NewClient(nil, nil, "")
	}
	if options.ChatGPTBase == "" {
		options.ChatGPTBase = "https://chatgpt.com"
	}
	if options.AuthBase == "" {
		options.AuthBase = "https://auth.openai.com"
	}
	options.ChatGPTBase = strings.TrimRight(options.ChatGPTBase, "/")
	options.AuthBase = strings.TrimRight(options.AuthBase, "/")
	if options.ClientID == "" {
		options.ClientID = DefaultCodexClientID
	}
	if options.RedirectURI == "" {
		options.RedirectURI = DefaultCodexRedirectURI
	}
	_ = ApplyAuthorizationMethod(&options)
	return &Flow{Client: client, Options: options}
}

func (f *Flow) emit(stage, status, prompt string) {
	f.emitData(stage, status, prompt, nil)
}
func (f *Flow) emitData(stage, status, prompt string, data map[string]any) {
	if f.Options.EventData != nil {
		f.Options.EventData(stage, status, prompt, data)
		return
	}
	if f.Options.Event != nil {
		f.Options.Event(stage, status, prompt)
	}
}
func (f *Flow) ask(ctx context.Context, stage, prompt string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	f.emit(stage, "awaiting_input", prompt)
	if f.Options.Input == nil {
		return "", fmt.Errorf("INPUT_REQUIRED: %s", stage)
	}
	v, err := f.Options.Input(ctx, stage, prompt)
	if err != nil {
		return "", err
	}
	// Passwords are opaque credentials: leading/trailing whitespace can be
	// intentional. Codes and phone numbers keep the legacy trimmed input.
	if stage != "password" {
		v = strings.TrimSpace(v)
	}
	if strings.EqualFold(v, "q") {
		return "", errors.New("flow stopped by user")
	}
	f.emit(stage, "running", "")
	return v, nil
}
func (f *Flow) sentinel(ctx context.Context, flow string) (http.Header, error) {
	if f.Options.Sentinel == nil {
		return nil, fmt.Errorf("AUTH_RUNTIME_REQUIRED: dynamic Sentinel token for %s requires the Go authentication runtime", flow)
	}
	h, e := f.Options.Sentinel(ctx, f.Options.AuthBase, f.deviceID, flow)
	if e != nil {
		return nil, e
	}
	if h.Get("Openai-Sentinel-Token") == "" {
		return nil, errors.New("SENTINEL_TOKEN_MISSING: authentication runtime returned no token")
	}
	return h, nil
}
func (f *Flow) save(stage string, oauth map[string]any) error {
	if oauth != nil {
		oauth = payloadCopy(oauth)
		oauth["clientId"], oauth["redirectUri"] = f.Options.ClientID, f.Options.RedirectURI
	}
	c := Checkpoint{Version: 1, Stage: stage, AuthorizationMethod: f.Options.AuthorizationMethod, Email: f.Options.Email, ChatGPTBase: f.Options.ChatGPTBase, AuthBase: f.Options.AuthBase, Cookies: f.Client.Jar.Cookies(), Web: f.web, OAuth: oauth, UpdatedAt: nowRFC3339()}
	if f.checkpoint != nil {
		c.Warning = f.checkpoint.Warning
		c.Extra = make(map[string]any, len(f.checkpoint.Extra))
		for key, value := range f.checkpoint.Extra {
			c.Extra[key] = value
		}
	}
	if c.Cookies == nil {
		c.Cookies = []transport.Cookie{}
	}
	f.checkpoint = &c
	if f.Options.SaveCheckpoint != nil {
		return f.Options.SaveCheckpoint(c)
	}
	if f.Options.CheckpointPath != "" {
		return WriteCheckpoint(f.Options.CheckpointPath, c)
	}
	return nil
}
func (f *Flow) Restore(c Checkpoint) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if err := f.validateAuthorizationCheckpoint(c); err != nil {
		return err
	}
	if f.Options.Email != "" && !strings.EqualFold(c.Email, f.Options.Email) {
		return errors.New("CHECKPOINT_INVALID: email mismatch")
	}
	if c.AuthBase != "" && strings.TrimRight(c.AuthBase, "/") != f.Options.AuthBase {
		return errors.New("CHECKPOINT_INVALID: auth origin mismatch")
	}
	if c.ChatGPTBase != "" && strings.TrimRight(c.ChatGPTBase, "/") != f.Options.ChatGPTBase {
		return errors.New("CHECKPOINT_INVALID: ChatGPT origin mismatch")
	}
	f.Options.Email = c.Email
	f.Client.Jar.Merge(c.Cookies)
	f.checkpoint = &c
	f.web = c.Web
	f.deviceID = str(c.Web["deviceId"])
	return nil
}

// Run resumes a valid checkpoint or logs in, authorizes Codex and creates the
// legacy-compatible export. Invalid OAuth state is rejected before token exchange.
func (f *Flow) Run(ctx context.Context) (FlowResult, error) {
	if err := ApplyAuthorizationMethod(&f.Options); err != nil {
		return FlowResult{}, err
	}
	if f.Options.AuthorizationMethod == AuthorizationMethodBPS && f.Options.WebOnly {
		return FlowResult{}, errors.New("BPS authorization does not support ChatGPT web-only output")
	}
	if f.Options.WebOnly {
		// The legacy web-only command starts a fresh web login even when a
		// checkpoint was supplied. Exporting restored cookies without a single
		// server request would falsely report an expired session as logged in.
		if err := f.Client.ResetSession(ctx); err != nil {
			return FlowResult{}, err
		}
		f.checkpoint, f.web = nil, nil
		f.verifiedPassword = ""
	}
	if !f.Options.CheckpointRestoreManaged && !f.Options.WebOnly && f.checkpoint == nil && f.Options.CheckpointPath != "" {
		if c, e := ReadCheckpoint(f.Options.CheckpointPath); e == nil {
			if e := f.Restore(c); e != nil {
				return FlowResult{}, e
			}
		} else if !errors.Is(e, os.ErrNotExist) {
			return FlowResult{}, e
		}
	}
	if f.Options.Email == "" {
		return FlowResult{}, errors.New("email is required")
	}
	var oauth map[string]any
	var tok TokenSet
	var err error
	tokensReceived := f.checkpoint != nil && f.checkpoint.Stage == tokenCheckpointStage
	if tokensReceived {
		oauth, tok, err = f.restoreTokenResponse(*f.checkpoint)
		if err != nil {
			return FlowResult{}, err
		}
	}
	for attempt := 0; !tokensReceived; attempt++ {
		if f.Options.AuthorizationMethod == AuthorizationMethodBPS {
			oauth, err = f.RunBPSOAuth(ctx)
		} else if f.checkpoint != nil && !strings.HasPrefix(f.checkpoint.Stage, "web_") {
			oauth, err = f.ResumeOAuth(ctx, *f.checkpoint)
		} else {
			f.web, err = f.LoginWeb(ctx)
			if err == nil && f.Options.WebOnly {
				return f.sessionResult(nil)
			}
			if err == nil {
				oauth, err = f.RunOAuth(ctx)
			}
		}
		if err == nil {
			break
		}
		if attempt != 0 || ctx.Err() != nil || !isExpiredSession(err) {
			return FlowResult{}, err
		}
		if err = f.resetExpiredSession(ctx); err != nil {
			return FlowResult{}, err
		}
	}
	if err = ctx.Err(); err != nil {
		return FlowResult{}, err
	}
	if f.Options.OutputMode == "session" {
		return f.sessionResult(oauth)
	}
	if f.Options.OutputMode == "both" {
		if _, e := f.sessionResult(oauth); e != nil {
			return FlowResult{}, e
		}
	}
	if !tokensReceived {
		cb, e := ParseOAuthCallback(str(oauth["callbackUrl"]), str(oauth["state"]))
		if e != nil {
			return FlowResult{}, e
		}
		tok, err = ExchangeOAuthCode(ctx, f.Client, f.Options.AuthBase, f.Options.ClientID, cb.Code, str(oauth["codeVerifier"]), f.Options.RedirectURI)
		if err != nil {
			return FlowResult{}, err
		}
		// Authorization codes are one-use. Preserve an accepted grant before
		// export construction or file writes can fail, including cancellation
		// racing with the successful response. Never redeem that code again.
		if err = f.saveTokenResponse(oauth, tok); err != nil {
			return FlowResult{}, err
		}
	}
	if err = ctx.Err(); err != nil {
		return FlowResult{}, err
	}
	ex, err := BuildSub2APIOAuthExport(tok, f.Options.ClientID, f.Options.Email, f.Options.AccountName, f.Options.Concurrency, f.Options.Priority, f.Options.RateMultiplier)
	if err != nil {
		return FlowResult{}, err
	}
	if f.Options.OutputPath != "" {
		if err = WritePrivateJSON(f.Options.OutputPath, ex); err != nil {
			return FlowResult{}, err
		}
	}
	if f.Options.CheckpointPath != "" {
		if err = RemoveCheckpoint(f.Options.CheckpointPath); err != nil {
			return FlowResult{}, err
		}
	}
	f.emit("completed", "succeeded", "")
	return FlowResult{Web: f.web, OAuth: oauth, Export: ex}, nil
}
func (f *Flow) auth(ctx context.Context, path string, body any, referer, flow string) (authPayload, error) {
	if referer == "" {
		referer = f.Options.AuthBase + "/"
	}
	var h http.Header
	var err error
	if flow != "" {
		h, err = f.sentinel(ctx, flow)
		if err != nil {
			return nil, err
		}
	}
	var out authPayload
	_, err = f.Client.JSON(ctx, http.MethodPost, f.Options.AuthBase+path, body, RequestOptions{Headers: h, Origin: f.Options.AuthBase, Referer: referer}, &out)
	return out, err
}
func (f *Flow) follow(ctx context.Context, raw, referer string) (*transport.FollowResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if f.isCallback(raw) {
		return &transport.FollowResult{FinalURL: raw}, nil
	}
	return f.Client.Follow(ctx, raw, RequestOptions{Referer: referer, StopAt: f.isCallback})
}
func (f *Flow) continueFlow(ctx context.Context, p authPayload) (string, error) {
	raw := continuation(p)
	if raw == "" {
		return "", errors.New("AUTH_CONTINUE_MISSING: no continuation URL")
	}
	r, e := f.follow(ctx, raw, "")
	if e != nil {
		return "", e
	}
	if f.isLoginURL(r.FinalURL) {
		return "", expiredSession("AUTH_LOGIN_REQUIRED")
	}
	return r.FinalURL, nil
}
func (f *Flow) isCallback(raw string) bool {
	u, e := url.Parse(raw)
	v, ve := url.Parse(f.Options.RedirectURI)
	return e == nil && ve == nil && u.Scheme == v.Scheme && u.Host == v.Host && u.Path == v.Path
}

func (f *Flow) LoginWeb(ctx context.Context) (map[string]any, error) {
	var pageURL, loginMethod string
	var authRequestedAt time.Time
	var authenticated authPayload
	if f.checkpoint != nil && strings.HasPrefix(f.checkpoint.Stage, "web_") {
		pageURL = str(f.checkpoint.OAuth["pageURL"])
		loginMethod = str(f.checkpoint.OAuth["loginMethod"])
		authenticated = payload(f.checkpoint.OAuth["payload"])
		f.deviceID = str(f.checkpoint.Web["deviceId"])
	} else {
		f.emit("login", "running", "")
		if _, e := f.follow(ctx, f.Options.ChatGPTBase+"/", f.Options.ChatGPTBase+"/"); e != nil {
			return nil, e
		}
		if _, e := f.Client.JSON(ctx, "GET", f.Options.ChatGPTBase+"/api/auth/providers", nil, RequestOptions{Referer: f.Options.ChatGPTBase + "/"}, nil); e != nil {
			return nil, e
		}
		var csrf struct {
			Token string `json:"csrfToken"`
		}
		if _, e := f.Client.JSON(ctx, "GET", f.Options.ChatGPTBase+"/api/auth/csrf", nil, RequestOptions{Referer: f.Options.ChatGPTBase + "/"}, &csrf); e != nil {
			return nil, e
		}
		if csrf.Token == "" || !f.Client.Jar.Has("__Host-next-auth.csrf-token") {
			return nil, errors.New("CSRF_MISSING: login CSRF token or cookie missing")
		}
		f.ensureDeviceID()
		f.web = map[string]any{"deviceId": f.deviceID, "authSessionLoggingId": randomUUID()}
		q := url.Values{"prompt": {"login"}, "ext-oai-did": {f.deviceID}, "auth_session_logging_id": {str(f.web["authSessionLoggingId"])}, "screen_hint": {"login_or_signup"}, "login_hint": {f.Options.Email}}
		var signin struct {
			URL string `json:"url"`
		}
		if e := f.beforeEmailOTP(ctx); e != nil {
			return nil, e
		}
		_, e := f.Client.JSON(ctx, "POST", f.Options.ChatGPTBase+"/api/auth/signin/openai?"+q.Encode(), nil, RequestOptions{Origin: f.Options.ChatGPTBase, Referer: f.Options.ChatGPTBase + "/", Form: url.Values{"callbackUrl": {f.Options.ChatGPTBase + "/"}, "csrfToken": {csrf.Token}, "json": {"true"}}}, &signin)
		if e != nil {
			return nil, e
		}
		if signin.URL == "" {
			return nil, errors.New("AUTHORIZE_URL_MISSING")
		}
		authRequestedAt = time.Now().UTC()
		page, e := f.follow(ctx, signin.URL, f.Options.ChatGPTBase+"/")
		if e != nil {
			return nil, e
		}
		pageURL = page.FinalURL
	}
	if authenticated == nil {
		switch pathOf(pageURL) {
		case "/log-in/password":
			loginMethod = "password"
			if e := f.save("web_password", map[string]any{"pageURL": pageURL, "loginMethod": loginMethod}); e != nil {
				return nil, e
			}
			var e error
			authenticated, e = f.verifyPassword(ctx, pageURL)
			if e != nil {
				return nil, e
			}
		case "/email-verification":
			loginMethod = "email_otp"
			if authRequestedAt.IsZero() {
				authRequestedAt, _ = time.Parse(time.RFC3339Nano, str(f.web["emailOtpRequestedAt"]))
			}
			if !authRequestedAt.IsZero() {
				f.emailOTPRequested(authRequestedAt)
			}
			if e := f.save("web_email_otp", map[string]any{"pageURL": pageURL, "loginMethod": loginMethod}); e != nil {
				return nil, e
			}
			var e error
			authenticated, e = f.verifyEmail(ctx, pageURL)
			if e != nil {
				return nil, e
			}
		default:
			if f.isCompletedWebLogin(pageURL) {
				loginMethod = "existing_session"
				authenticated = authPayload{"continue_url": pageURL}
			} else {
				return nil, fmt.Errorf("UNEXPECTED_LOGIN_PAGE: %s", pathOf(pageURL))
			}
		}
		if e := f.save("web_continue", map[string]any{"pageURL": pageURL, "loginMethod": loginMethod, "payload": authenticated}); e != nil {
			return nil, e
		}
	}
	mfaRequired := pageType(authenticated) == "mfa_challenge" || strings.HasPrefix(pathOf(continuation(authenticated)), "/mfa-challenge/")
	previouslyVerified, _ := f.web["mfaVerified"].(bool)
	if mfaRequired {
		if e := f.save("web_mfa", map[string]any{"pageURL": pageURL, "loginMethod": loginMethod, "payload": authenticated}); e != nil {
			return nil, e
		}
		var e error
		authenticated, e = f.verifyMFA(ctx, authenticated, pageURL)
		if e != nil {
			return nil, e
		}
		if f.web == nil {
			f.web = map[string]any{"deviceId": f.deviceID}
		}
		f.web["mfaVerified"] = true
		if e = f.save("web_continue", map[string]any{"pageURL": pageURL, "loginMethod": loginMethod, "payload": authenticated}); e != nil {
			return nil, e
		}
	}
	if pageType(authenticated) == "about_you" || pathOf(continuation(authenticated)) == "/about-you" {
		profile := f.Options.Profile
		if profile == nil {
			profile = generatedProfile()
		}

		ref := continuation(authenticated)
		if ref == "" {
			ref = f.Options.AuthBase + "/about-you"
		}
		var e error
		authenticated, e = f.auth(ctx, "/api/accounts/create_account", profile, ref, "oauth_create_account")
		if e != nil {
			return nil, e
		}
		if pageType(authenticated) == "about_you" || pathOf(continuation(authenticated)) == "/about-you" {
			return nil, errors.New("PROFILE_SECURITY_CHECK_REQUIRED: profile submission did not advance")
		}
		callback, _ := url.Parse(continuation(authenticated))
		chat, _ := url.Parse(f.Options.ChatGPTBase)
		if pageType(authenticated) != "external_url" || !sameOrigin(callback, chat) || callback.Path != "/api/auth/callback/openai" {
			return nil, errors.New("PROFILE_RESULT_INVALID: expected ChatGPT callback")
		}
		if e = f.save("web_continue", map[string]any{"pageURL": pageURL, "loginMethod": loginMethod, "payload": authenticated}); e != nil {
			return nil, e
		}
	}
	if pageType(authenticated) == "workspace" || pathOf(continuation(authenticated)) == "/workspace" {
		id := f.workspace(authenticated)
		if id == "" && len(slice(payload(authenticated["oai-client-auth-session"])["workspaces"])) > 0 {
			return nil, errors.New("WORKSPACE_UNAVAILABLE: no matching workspace in authenticated session")
		}
		if id != "" {
			var e error
			ref := continuation(authenticated)
			if ref == "" {
				ref = f.Options.AuthBase + "/workspace"
			}
			authenticated, e = f.auth(ctx, "/api/accounts/workspace/select", map[string]any{"workspace_id": id}, ref, "")
			if e != nil {
				return nil, e
			}
			if e = f.save("web_continue", map[string]any{"pageURL": pageURL, "loginMethod": loginMethod, "payload": authenticated}); e != nil {
				return nil, e
			}
		}
	}
	callback, e := f.continueFlow(ctx, authenticated)
	if e != nil {
		return nil, e
	}
	if f.web == nil {
		f.web = map[string]any{"deviceId": f.deviceID}
	}
	f.web["callbackUrl"] = callback
	f.web["emailVerified"] = true
	f.web["loginMethod"] = loginMethod
	f.web["mfaVerified"] = mfaRequired || previouslyVerified
	if e := f.save("email_verified", nil); e != nil {
		return nil, e
	}
	return f.web, nil
}

func (f *Flow) isCompletedWebLogin(raw string) bool {
	u, err := url.Parse(raw)
	base, _ := url.Parse(f.Options.ChatGPTBase)
	return err == nil && sameOrigin(u, base) && u.Path == "/" && f.Client.Jar.Value("__Secure-next-auth.session-token", raw) != ""
}
func (f *Flow) verifyPassword(ctx context.Context, ref string) (authPayload, error) {
	password := f.Options.Password
	prompt := "登录服务要求账号密码，但此任务没有可用密码。请输入账号密码，或取消后重新导入完整账号资料（q 退出）"
	if f.Options.AuthStep != nil {
		f.Options.AuthStep("password", password != "")
	}
	for {
		if password == "" {
			var e error
			password, e = f.ask(ctx, "password", prompt)
			if e != nil {
				return nil, e
			}
		}
		if password == "" {
			// The legacy prompt treats empty input as cancellation, before
			// creating a Sentinel browser session or submitting a credential.
			return nil, errors.New("flow stopped before password validation")
		}
		submittedPassword := password
		p, e := f.auth(ctx, "/api/accounts/password/verify", map[string]any{"password": submittedPassword}, ref, "password_verify")
		password = ""
		if e == nil {
			if f.Options.PasswordVerified != nil {
				if err := f.Options.PasswordVerified(ctx, submittedPassword); err != nil {
					return nil, err
				}
			}
			f.verifiedPassword = submittedPassword
			return p, nil
		}
		if !rejected(e, "password") {
			return nil, e
		}
		prompt = "登录服务拒绝了已提交的账号密码。请输入正确的账号密码，或取消后更新导入资料（q 退出）"
	}
}

var digits6 = regexp.MustCompile(`^\d{6}$`)
var phoneDigits = regexp.MustCompile(`^\d{4,8}$`)
var phoneE164 = regexp.MustCompile(`^\+[1-9]\d{6,14}$`)

func (f *Flow) verifyEmail(ctx context.Context, ref string) (authPayload, error) {
	for {
		code, e := f.ask(ctx, "email_otp", "Email OTP (r=resend, q=quit)")
		if e != nil {
			return nil, e
		}
		if strings.EqualFold(code, "r") {
			if e := f.beforeEmailOTP(ctx); e != nil {
				return nil, e
			}
			requestedAt := time.Now().UTC()
			_, e = f.auth(ctx, "/api/accounts/email-otp/resend", map[string]any{}, ref, "")
			if e != nil {
				return nil, e
			}
			f.emailOTPRequested(requestedAt)
			if f.checkpoint != nil && (f.checkpoint.Stage == "web_email_otp" || strings.HasPrefix(f.checkpoint.Stage, "bps_")) {
				if e = f.save(f.checkpoint.Stage, f.checkpoint.OAuth); e != nil {
					return nil, e
				}
			}
			continue
		}
		if !digits6.MatchString(code) {
			continue
		}
		p, e := f.auth(ctx, "/api/accounts/email-otp/validate", map[string]any{"code": code}, ref, "email_otp_validate")
		if e == nil {
			return p, nil
		}
		if !rejected(e, "otp") {
			return nil, e
		}
		f.emit("email_otp_rejected", "running", "Email verification code rejected; enter a new code or resend")
	}
}
func (f *Flow) beforeEmailOTP(ctx context.Context) error {
	if f.Options.BeforeEmailOTP != nil {
		return f.Options.BeforeEmailOTP(ctx)
	}
	return ctx.Err()
}
func (f *Flow) emailOTPRequested(at time.Time) {
	if f.web == nil {
		f.web = map[string]any{}
	}
	f.web["emailOtpRequestedAt"] = at.UTC().Format(time.RFC3339Nano)
	f.emit("email_otp_requested", "running", "")
	if f.Options.EmailOTPRequested != nil {
		f.Options.EmailOTPRequested(at)
	}
}
func (f *Flow) verifyMFA(ctx context.Context, p authPayload, ref string) (authPayload, error) {
	session := payload(p["oai-client-auth-session"])
	var id string
	for _, key := range []string{"mfa_challenge_factors", "mfa_factors"} {
		for _, v := range slice(session[key]) {
			a := payload(v)
			if str(a["factor_type"]) == "totp" && str(a["id"]) != "" {
				id = str(a["id"])
				break
			}
		}
		if id != "" {
			break
		}
	}
	if id == "" {
		return nil, errors.New("MFA_FACTOR_MISSING: TOTP factor ID missing")
	}
	if _, e := f.auth(ctx, "/api/accounts/mfa/issue_challenge", map[string]any{"type": "totp", "id": id, "force_fresh_challenge": false}, ref, ""); e != nil {
		return nil, e
	}
	var code string
	var err error
	automatic := false
	prompt := "2FA OTP (6 digits, q=quit)"
	generatedSecret := f.Options.TOTPSecret
	if f.Options.AuthStep != nil {
		f.Options.AuthStep("mfa", f.Options.TOTPSecret != "")
	}
	if f.Options.TOTPSecret != "" {
		code, err = GenerateTOTP(f.Options.TOTPSecret, time.Now())
		if err != nil {
			return nil, err
		}
		automatic = true
	}
	for {
		if code == "" {
			automatic = false
			code, err = f.ask(ctx, "totp", prompt)
			if err != nil {
				return nil, err
			}
		}
		if !digits6.MatchString(code) {
			code = ""
			automatic = false
			continue
		}
		challengeURL := continuation(p)
		if challengeURL == "" {
			challengeURL = f.Options.AuthBase + "/mfa-challenge/" + url.PathEscape(id)
		}
		flowName := "password_verify"
		if f.Options.AuthorizationMethod == AuthorizationMethodBPS {
			flowName = ""
		}
		out, e := f.auth(ctx, "/api/accounts/mfa/verify", map[string]any{"type": "totp", "id": id, "code": code}, challengeURL, flowName)
		code = ""
		if e == nil {
			if automatic && f.Options.TOTPVerified != nil {
				if err := f.Options.TOTPVerified(ctx, generatedSecret); err != nil {
					return nil, err
				}
			}
			return out, nil
		}
		if !rejected(e, "totp") {
			return nil, e
		}
		message := "2FA verification code rejected; enter a current code"
		prompt = "2FA 验证码被拒绝，请输入当前有效的 6 位验证码（q 退出）"
		if automatic {
			message = "Code generated from the configured 2FA key was rejected; enter a current code manually"
			prompt = "使用保存的 2FA 密钥生成的验证码被拒绝，请手动输入验证器当前显示的 6 位验证码（q 退出）"
		}
		automatic = false
		f.emit("mfa_code_rejected", "running", message)
	}
}

func (f *Flow) RunOAuth(ctx context.Context) (map[string]any, error) {
	f.emit("oauth", "running", "")
	pkce, e := NewPKCE()
	if e != nil {
		return nil, e
	}
	raw, e := AuthorizationURL(f.Options.AuthBase, f.Options.ClientID, f.Options.RedirectURI, pkce)
	if e != nil {
		return nil, e
	}
	page, e := f.follow(ctx, raw, "")
	if e != nil {
		return nil, e
	}
	oauth := map[string]any{"codeVerifier": pkce.CodeVerifier, "state": pkce.State}
	if f.isCallback(page.FinalURL) {
		return f.finishCallback(page.FinalURL, oauth)
	}
	id := ExtractSessionID(page.Response.Text())
	if id == "" {
		if f.isLoginURL(page.FinalURL) {
			return nil, expiredSession("CODEX_AUTH_LOGIN_REQUIRED")
		}
		return nil, errors.New("SESSION_SELECTION_INVALID: account session missing; restart authorization")
	}
	current, e := f.auth(ctx, "/api/accounts/session/select", map[string]any{"session_id": id}, f.Options.AuthBase+"/choose-an-account", "")
	if e != nil {
		return nil, e
	}
	oauth["sessionId"] = id
	oauth["addPhoneUrl"] = continuation(current)
	if needsPhone(current) {
		if str(oauth["addPhoneUrl"]) == "" {
			oauth["addPhoneUrl"] = f.Options.AuthBase + "/add-phone"
		}
		if e = f.save("phone_required", oauth); e != nil {
			return nil, e
		}
		current, e = f.bindPhone(ctx, oauth, false)
		if e != nil {
			return nil, e
		}
	} else if len(slice(payload(current["oai-client-auth-session"])["workspaces"])) == 0 && !f.isCallback(continuation(current)) {
		return nil, errors.New("SESSION_SELECTION_INVALID: expected phone verification, workspace or OAuth callback")
	}
	oauth["current"] = current
	if e = f.save("oauth_continue", oauth); e != nil {
		return nil, e
	}
	return f.finishOAuth(ctx, current, oauth)
}
func (f *Flow) ResumeOAuth(ctx context.Context, c Checkpoint) (map[string]any, error) {
	if c.Stage == tokenCheckpointStage {
		oauth, _, err := f.restoreTokenResponse(c)
		return oauth, err
	}
	if c.Stage == "email_verified" {
		return f.RunOAuth(ctx)
	}
	o := c.OAuth
	if str(o["codeVerifier"]) == "" || str(o["state"]) == "" {
		return nil, errors.New("CHECKPOINT_INVALID: missing OAuth PKCE state")
	}
	switch c.Stage {
	case "callback_ready":
		return f.finishCallback(str(o["callbackUrl"]), o)
	case "oauth_continue":
		return f.finishOAuth(ctx, payload(o["current"]), o)
	case "phone_required", "phone_otp":
		if pathOf(str(o["addPhoneUrl"])) != "/add-phone" {
			return nil, errors.New("CHECKPOINT_INVALID: expected add-phone URL")
		}
		p, e := f.bindPhone(ctx, o, c.Stage == "phone_otp")
		if e != nil {
			return nil, e
		}
		o["current"] = p
		delete(o, "phone")
		delete(o, "phoneReferer")
		if e = f.save("oauth_continue", o); e != nil {
			return nil, e
		}
		return f.finishOAuth(ctx, p, o)
	default:
		return nil, fmt.Errorf("CHECKPOINT_INVALID: unsupported stage %s", c.Stage)
	}
}
func (f *Flow) finishOAuth(ctx context.Context, current authPayload, o map[string]any) (map[string]any, error) {
	if f.isCallback(continuation(current)) {
		return f.finishCallback(continuation(current), o)
	}
	id := f.workspace(current)
	if id == "" && len(slice(payload(current["oai-client-auth-session"])["workspaces"])) > 0 {
		return nil, errors.New("WORKSPACE_UNAVAILABLE: no matching workspace in authenticated session")
	}
	if id != "" {
		var e error
		ref := continuation(current)
		if ref == "" {
			ref = f.Options.AuthBase + "/sign-in-with-chatgpt/codex/consent"
		}
		current, e = f.auth(ctx, "/api/accounts/workspace/select", map[string]any{"workspace_id": id}, ref, "")
		if e != nil {
			return nil, e
		}
		o["workspaceId"] = id
		o["current"] = current
		if e = f.save("oauth_continue", o); e != nil {
			return nil, e
		}
	}
	callback, e := f.continueFlow(ctx, current)
	if e != nil {
		return nil, e
	}
	return f.finishCallback(callback, o)
}
func (f *Flow) finishCallback(callback string, o map[string]any) (map[string]any, error) {
	if !f.isCallback(callback) {
		return nil, errors.New("OAUTH_CALLBACK_INVALID: unexpected redirect URI")
	}
	if _, e := ParseOAuthCallback(callback, str(o["state"])); e != nil {
		return nil, e
	}
	out := map[string]any{"callbackUrl": callback, "codeVerifier": o["codeVerifier"], "state": o["state"], "workspaceId": o["workspaceId"]}
	if e := f.save("callback_ready", out); e != nil {
		return nil, e
	}
	return out, nil
}
func (f *Flow) workspace(p authPayload) string {
	ws := slice(payload(p["oai-client-auth-session"])["workspaces"])
	var first string
	for _, v := range ws {
		w := payload(v)
		id := str(w["id"])
		if id == "" {
			continue
		}
		if f.Options.WorkspaceID != "" {
			if id == f.Options.WorkspaceID {
				return id
			}
			continue
		}
		if first == "" {
			first = id
		}
		if str(w["kind"]) == "organization" {
			return id
		}
	}
	return first
}
func (f *Flow) bindPhone(ctx context.Context, o map[string]any, resume bool) (authPayload, error) {
	raw := str(o["addPhoneUrl"])
	page, e := f.follow(ctx, raw, raw)
	if e != nil {
		return nil, e
	}
	if f.isLoginURL(page.FinalURL) {
		return nil, expiredSession("PHONE_LOGIN_REQUIRED")
	}
	ref := page.FinalURL
	phone := f.Options.Phone
	phoneRef := ""
	if resume {
		savedPhone, savedRef := str(o["phone"]), str(o["phoneReferer"])
		if phoneE164.MatchString(savedPhone) && savedRef != "" {
			phone, phoneRef = savedPhone, savedRef
			f.phoneEvent("phone_resumed", phone, "", false)
		}
	}
	if o["phoneRetryRequired"] == true {
		// A saved temporary error requires an explicit decision after restart.
		// In particular, a configured phone must not replay an uncertain send.
		stage := "phone_send_deferred"
		if phoneRef == "" {
			phone = ""
		} else {
			stage = "sms_otp_deferred"
		}
		f.phoneDeferredEvent(stage, str(o["phone"]), "Phone request paused; retry manually when ready, change number or cancel")
	}
	for {
		if phoneRef == "" {
			if phone == "" {
				phone, e = f.ask(ctx, "phone", "Phone number, E.164 format (q=quit)")
				if e != nil {
					return nil, e
				}
				if strings.EqualFold(phone, "p") {
					return nil, errors.New("flow stopped by user")
				}
			}
			if !phoneE164.MatchString(phone) {
				phone = ""
				continue
			}
			sent, err := f.sendPhone(ctx, phone, ref)
			if err != nil {
				if !retryPhoneInput(ctx, err) {
					f.phoneEvent("phone_rejected", "", phoneFailureMessage(err, "Phone number was not accepted; choose another number"), errors.Is(err, ErrBrowserChallenge))
					return nil, err
				}
				if deferredPhoneFailure(err) {
					// Do not discard the leased number or automatically repeat a
					// possibly accepted write after an uncertain request outcome.
					o["phone"] = phone
					o["phoneRetryRequired"] = true
					delete(o, "phoneReferer")
					if e = f.save("phone_required", o); e != nil {
						return nil, e
					}
					f.phoneDeferredEvent("phone_send_deferred", phone, phoneFailureMessage(err, "Phone request did not complete successfully; retry manually, change number or cancel"))
				} else {
					f.phoneEvent("phone_rejected", "", phoneFailureMessage(err, "Phone number was not accepted; choose another number"), false)
				}
				phone = ""
				continue
			}
			phoneRef = continuation(sent)
			if phoneRef == "" {
				phoneRef = f.Options.AuthBase + "/phone-verification"
			}
			o["phone"] = phone
			o["phoneReferer"] = phoneRef
			delete(o, "phoneRetryRequired")
			f.phoneEvent("phone_sent", phone, "", false)
			if e = f.save("phone_otp", o); e != nil {
				return nil, e
			}
		}
		code, err := f.ask(ctx, "sms_otp", "Phone OTP (r=resend, p=change phone, q=quit)")
		if err != nil {
			return nil, err
		}
		switch strings.ToLower(code) {
		case "p":
			phone = ""
			phoneRef = ""
			delete(o, "phone")
			delete(o, "phoneReferer")
			delete(o, "phoneRetryRequired")
			f.phoneEvent("phone_changed", "", "", false)
			if e = f.save("phone_required", o); e != nil {
				return nil, e
			}
			continue
		case "r":
			if _, e = f.sendPhone(ctx, phone, ref); e != nil {
				if deferredPhoneFailure(e) && retryPhoneInput(ctx, e) {
					o["phoneRetryRequired"] = true
					if err := f.save("phone_otp", o); err != nil {
						return nil, err
					}
					f.phoneDeferredEvent(phoneDeferredStage(e, "sms_resend_deferred", "sms_resend_failed"), phone, phoneFailureMessage(e, "SMS resend did not complete successfully; enter a code, retry manually or change number"))
				} else {
					f.phoneEvent("sms_resend_failed", phone, phoneFailureMessage(e, "SMS resend was not accepted; enter a code, retry or change number"), errors.Is(e, ErrBrowserChallenge))
				}
				if !retryPhoneInput(ctx, e) {
					return nil, e
				}
			} else {
				delete(o, "phoneRetryRequired")
				if e = f.save("phone_otp", o); e != nil {
					return nil, e
				}
				f.phoneEvent("sms_resent", phone, "", false)
			}
			continue
		}
		if !phoneDigits.MatchString(code) {
			continue
		}
		p, err := f.auth(ctx, "/api/accounts/phone-otp/validate", map[string]any{"code": code}, phoneRef, "")
		if err == nil {
			delete(o, "phoneRetryRequired")
			f.phoneEvent("phone_verified", phone, "", false)
			return p, nil
		}
		if deferredPhoneFailure(err) && retryPhoneInput(ctx, err) {
			o["phoneRetryRequired"] = true
			if e = f.save("phone_otp", o); e != nil {
				return nil, e
			}
			f.phoneDeferredEvent(phoneDeferredStage(err, "sms_otp_deferred", "sms_otp_rejected"), phone, phoneFailureMessage(err, "SMS verification did not complete successfully; enter a code manually, resend or change number"))
		} else {
			f.phoneEvent("sms_otp_rejected", phone, phoneFailureMessage(err, "SMS code rejected; enter another code, resend or change number"), errors.Is(err, ErrBrowserChallenge))
		}
		if isExpiredSession(err) {
			return nil, err
		}
		if !retryPhoneInput(ctx, err) {
			return nil, err
		}
	}
}
func (f *Flow) phoneEvent(stage, phone, message string, securityCheck bool) {
	data := map[string]any{"currentPhone": phone, "phoneError": message, "securityCheckRequired": securityCheck}
	switch stage {
	case "phone_sent", "sms_resent", "phone_verified", "phone_changed", "phone_rejected":
		data["phoneRetryRequired"] = false
	}
	f.emitData(stage, "running", message, data)
}
func (f *Flow) phoneDeferredEvent(stage, phone, message string) {
	f.emitData(stage, "running", message, map[string]any{"currentPhone": phone, "phoneError": message, "securityCheckRequired": false, "phoneRetryRequired": true})
}

// Known number/code rejections retain their existing input handling. An
// unclassified HTTP response (including 400/409/422) does not prove a number
// was rejected or a write was ignored, so pause the automatic provider too.
func deferredPhoneFailure(err error) bool {
	var h *HTTPError
	if !errors.As(err, &h) {
		return true
	}
	if rejected(err, "otp") || rejected(err, "phone") {
		return false
	}
	if h.StatusCode == 400 || h.StatusCode == 409 || h.StatusCode == 422 {
		switch h.Code {
		case "invalid_phone", "phone_number_invalid", "invalid_channel":
			return false
		case "invalid_state":
			return h.StatusCode != 400
		}
	}
	return true
}

// Keep the existing structured rejection stages for ordinary 400/409/422
// responses, while adding the explicit pause flag when their outcome is unknown.
func phoneDeferredStage(err error, deferred, legacy string) string {
	var h *HTTPError
	if errors.As(err, &h) && (h.StatusCode == 400 || h.StatusCode == 409 || h.StatusCode == 422) {
		return legacy
	}
	return deferred
}
func phoneFailureMessage(err error, fallback string) string {
	if errors.Is(err, ErrBrowserChallenge) {
		return "Security check could not complete; retry authorization"
	}
	var h *HTTPError
	if errors.As(err, &h) {
		if h.StatusCode == http.StatusTooManyRequests {
			return "Phone requests are temporarily rate limited; retry later"
		}
		if h.Code == "invalid_auth_step" || h.Code == "invalid_authorization_step" {
			return "Phone authorization step is no longer valid; restart authorization"
		}
	}
	return fallback
}
func (f *Flow) sendPhone(ctx context.Context, phone, ref string) (authPayload, error) {
	p, e := f.auth(ctx, "/api/accounts/add-phone/send", map[string]any{"phone_number": phone, "channel": "sms"}, ref, "")
	var h *HTTPError
	if e != nil && errors.As(e, &h) && (h.StatusCode == 400 || h.StatusCode == 409) && (h.phoneWithoutChannel || h.Code == "invalid_channel" || h.Code == "invalid_state" || h.Code == "session_expired" || h.Code == "session_invalid" || h.Code == "invalid_session") {
		return f.auth(ctx, "/api/accounts/add-phone/send", map[string]any{"phone_number": phone}, ref, "")
	}
	return p, e
}

func retryPhoneInput(ctx context.Context, err error) bool {
	// The transport has its own request deadline. Only the task context proves
	// that the job ended; a request timeout can follow an accepted phone write.
	if err == nil || ctx.Err() != nil || errors.Is(err, ErrBrowserChallenge) ||
		errors.Is(err, ErrSessionExpired) {
		return false
	}
	// Joined errors may contain both a provider status and evidence that the
	// bound session can no longer be used. The hard boundary takes precedence.
	var probe *transport.ProfileProbeError
	if errors.As(err, &probe) || phoneSessionUnavailable(err) {
		return false
	}
	var h *HTTPError
	if errors.As(err, &h) {
		// Explicit authorization loss stays terminal regardless of the status
		// used by an intermediary. HTTP 400 invalid_state retains its legacy
		// number-input exception; SMS validation still checks expiry separately.
		switch h.Code {
		case "invalid_auth_step", "invalid_authorization_step", "session_expired", "session_invalid", "invalid_session", "login_required":
			return false
		case "invalid_state":
			if h.StatusCode != http.StatusBadRequest {
				return false
			}
		}
		if h.StatusCode == http.StatusProxyAuthRequired || h.Code == "" && h.sessionExpiredText {
			return false
		}
		return !isExpiredSession(err) || h.StatusCode == 400 && h.Code == "invalid_state"
	}
	return true
}

// Local configuration/session errors must not be turned into a prompt for a new
// number. Inspect only closed transport diagnostics and exact local markers,
// never remote response text or broad matches for words such as TLS/proxy.
func phoneSessionUnavailable(err error) bool {
	if err == nil {
		return false
	}
	if diagnostic, ok := err.(*transport.TransportError); ok {
		switch diagnostic.Error() {
		case "request canceled", "request timed out", "DNS lookup failed", "connection refused",
			"network is unreachable", "connection closed before HTTP headers",
			"TLS response body read failed", "TLS transport request failed before HTTP headers":
			return false
		default:
			return true
		}
	}
	message := err.Error()
	for _, prefix := range []string{
		"PROXY_RISK_CONTROL:", "PROXY_CONNECTION_RETRY:",
		"LEGACY_RUNTIME_", "GO_JS_RUNTIME:", "AUTH_RUNTIME_REQUIRED:",
		"SENTINEL_", "CHECKPOINT_SESSION_",
	} {
		if strings.HasPrefix(message, prefix) {
			return true
		}
	}
	switch message {
	case "invalid or unsupported proxy configuration", "requested TLS profile is not bundled",
		"TLS transport initialization failed", "TLS certificate validation failed",
		"HTTPS proxy certificate validation failed", "TLS handshake or protocol failed",
		"HTTPS proxy TLS handshake failed", "proxy authentication rejected (HTTP 407)",
		"proxy connection refused", "proxy DNS lookup failed", "proxy network is unreachable",
		"proxy connection timed out", "proxy connection failed", "proxy CONNECT request failed",
		"proxy connectivity attempts exhausted", "native proxy connectivity attempts exhausted",
		"invalid TLS request", "invalid HTTP request URL",
		"nil protocol client", "nil transport client":
		return true
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, cause := range joined.Unwrap() {
			if phoneSessionUnavailable(cause) {
				return true
			}
		}
		return false
	}
	return phoneSessionUnavailable(errors.Unwrap(err))
}

func continuation(p authPayload) string {
	if v := str(p["continue_url"]); v != "" {
		return v
	}
	return str(payload(payload(p["page"])["payload"])["url"])
}
func pageType(p authPayload) string { return str(payload(p["page"])["type"]) }
func needsPhone(p authPayload) bool {
	return pageType(p) == "add_phone" || pathOf(continuation(p)) == "/add-phone"
}
func pathOf(raw string) string {
	u, e := url.Parse(raw)
	if e != nil {
		return ""
	}
	return u.Path
}
func str(v any) string { s, _ := v.(string); return s }
func payload(v any) authPayload {
	switch p := v.(type) {
	case authPayload:
		return p
	case map[string]any:
		return authPayload(p)
	default:
		return nil
	}
}
func slice(v any) []any { a, _ := v.([]any); return a }
func rejected(err error, kind string) bool {
	var e *HTTPError
	if !errors.As(err, &e) {
		return false
	}
	// A provider can reject a credential/code with 403 as well as 400. Only
	// recognized rejection codes keep the current authentication step open;
	// permission loss, challenges, rate limiting and server errors remain errors.
	if e.StatusCode != 400 && e.StatusCode != 401 && e.StatusCode != 403 && e.StatusCode != 422 {
		return false
	}
	switch kind {
	case "password":
		return e.Code == "invalid_password" || e.Code == "incorrect_password" || e.Code == "wrong_password"
	case "otp":
		return e.Code == "wrong_email_otp_code" || e.Code == "invalid_email_code" || e.Code == "invalid_otp" || e.Code == "invalid_code" || e.Code == "incorrect_code" || e.Code == "wrong_code"
	case "totp":
		return e.Code == "invalid_totp" || e.Code == "invalid_otp" || e.Code == "invalid_code" || e.Code == "incorrect_code" || e.Code == "wrong_code"
	case "phone":
		return e.Code == "invalid_phone" || e.Code == "phone_number_invalid"
	}
	return false
}

var sessionPatterns = []*regexp.Regexp{regexp.MustCompile(`"session_id"\s*:\s*"(us_[^"]+)"`), regexp.MustCompile(`session_id\\",\\"(us_[^"\\]+)`), regexp.MustCompile(`name="session_id"[^>]+value="(us_[^"]+)"`), regexp.MustCompile(`value="(us_[^"]+)"[^>]+name="session_id"`), regexp.MustCompile(`\b(us_[A-Za-z0-9_-]{10,})\b`)}

func ExtractSessionID(text string) string {
	text = html.UnescapeString(text)
	for _, re := range sessionPatterns {
		if m := re.FindStringSubmatch(text); len(m) > 1 {
			return m[1]
		}
	}
	return ""
}

func generatedProfile() map[string]any {
	first := []string{"Alex", "Avery", "Blake", "Cameron", "Casey", "Drew", "Emerson", "Hayden", "Jamie", "Jordan", "Logan", "Morgan", "Parker", "Quinn", "Reese", "Taylor"}
	last := []string{"Adams", "Baker", "Brooks", "Carter", "Clark", "Collins", "Cooper", "Evans", "Foster", "Gray", "Hall", "Hayes", "Morgan", "Perry", "Reed", "Walker"}
	now := time.Now().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	latest := today.AddDate(-20, 0, 0)
	earliest := today.AddDate(-51, 0, 1)
	span := int(latest.Sub(earliest).Hours() / 24)
	return map[string]any{"name": first[profileRandom(len(first))] + " " + last[profileRandom(len(last))], "birthdate": earliest.AddDate(0, 0, profileRandom(span+1)).Format("2006-01-02")}
}
func profileRandom(n int) int {
	v, err := rand.Int(rand.Reader, big.NewInt(int64(n)))
	if err != nil {
		return 0
	}
	return int(v.Int64())
}

// DefaultOutputPath keeps the legacy per-job output filename.
func DefaultOutputPath(dir string) string { return filepath.Join(dir, "sub2api-import-oauth.json") }

// HTTPError exposes only a validated provider error code; arbitrary response
// bodies (which can echo passwords, codes or tokens) must never enter logs.
type HTTPError struct {
	StatusCode int
	Code       string
	// Derive legacy control-flow decisions before discarding remote text.
	// These flags cannot echo server messages, request credentials or tokens.
	phoneWithoutChannel bool
	sessionExpiredText  bool
}

func (e *HTTPError) Error() string {
	if e.Code == "" {
		return fmt.Sprintf("HTTP %d", e.StatusCode)
	}
	return fmt.Sprintf("HTTP %d: %s", e.StatusCode, e.Code)
}
func newHTTPError(status int, b []byte) *HTTPError {
	candidates := providerErrorCandidates(b)
	failure := &HTTPError{StatusCode: status}
	for _, candidate := range candidates {
		failure.phoneWithoutChannel = failure.phoneWithoutChannel || legacyPhoneRetryText.MatchString(candidate)
		failure.sessionExpiredText = failure.sessionExpiredText || legacyExpiredSessionText.MatchString(candidate)
	}
	classified := func(code string) *HTTPError {
		failure.Code = code
		return failure
	}
	for _, candidate := range candidates {
		s := strings.ToLower(candidate)
		for _, code := range []string{"account_deactivated", "account_deleted", "account_suspended", "account_disabled", "user_deactivated", "user_deleted", "user_suspended", "user_disabled", "access_denied", "permission_denied", "forbidden", "wrong_email_otp_code", "invalid_authorization_step", "invalid_auth_step", "invalid_password", "incorrect_password", "wrong_password", "invalid_email_code", "invalid_otp", "invalid_totp", "invalid_code", "incorrect_code", "wrong_code", "invalid_phone", "phone_number_invalid", "invalid_channel", "registration_disallowed", "session_expired", "session_invalid", "invalid_session", "invalid_state", "login_required"} {
			if strings.Contains(strings.NewReplacer(" ", "_", "-", "_").Replace(s), code) {
				return classified(code)
			}
		}
		if match := permanentAccountPattern.FindStringSubmatch(s); len(match) == 2 {
			return classified("account_" + match[1])
		}
		if strings.Contains(s, "do not have an account because it has been deleted") || strings.Contains(s, "account has been deleted or deactivated") {
			return classified("account_deleted")
		}
		for _, rule := range safeRejectionPatterns {
			if rule.re.MatchString(s) {
				return classified(rule.code)
			}
		}
	}
	return failure
}

func providerErrorCandidates(b []byte) []string {
	var body any
	if json.Unmarshal(b, &body) != nil {
		// Some legacy auth endpoints return a short plain-text rejection.
		// Never classify HTML challenge documents by incidental script text.
		message := strings.TrimSpace(string(b))
		if len(message) <= 4096 && !strings.ContainsAny(message, "<>") {
			return []string{message}
		}
		return nil
	}
	switch value := body.(type) {
	case string:
		return []string{value}
	case map[string]any:
		if nested, ok := value["error"].(map[string]any); ok {
			// Explicit provider codes take precedence over descriptive text.
			return []string{str(nested["code"]), str(value["code"]), str(nested["message"]), str(value["message"]), str(value["error_description"])}
		}
		return []string{str(value["error"]), str(value["code"]), str(value["message"]), str(value["error_description"])}
	default:
		return nil
	}
}

// protocol-login.mjs:2180-2193 used these message-level signals for bounded
// checkpoint recovery and add-phone/send's single channel-less compatibility
// retry. Retain their meaning without retaining the raw response in errors.
var legacyPhoneRetryText = regexp.MustCompile("(?i)channel|invalid_state|no longer valid|session")
var legacyExpiredSessionText = regexp.MustCompile("(?i)no longer valid|session.+(?:invalid|expired)|expired.+session")

var safeRejectionPatterns = []struct {
	re   *regexp.Regexp
	code string
}{
	{regexp.MustCompile(`(?:invalid|incorrect|wrong)[_\s-]*password|password[_\s-]*(?:invalid|incorrect|wrong)`), "invalid_password"},
	{regexp.MustCompile(`(?:invalid|incorrect|wrong)[_\s-]*(?:email[_\s-]*)?(?:totp|otp|code)|(?:totp|otp|code)[_\s-]*(?:invalid|incorrect|wrong)`), "invalid_code"},
	{regexp.MustCompile(`(?:unexpected|invalid|unsupported)[_\s-]+channel|channel[_\s-]+(?:field[_\s-]+)?(?:unexpected|invalid|unsupported)`), "invalid_channel"},
}

var permanentAccountPattern = regexp.MustCompile(`\b(?:(?:your|this|the)\s+)?(?:openai\s+|chatgpt\s+)?(?:account|user)\s+(?:has been|is|was)\s+(?:permanently\s+)?(deactivated|deleted|suspended|disabled)\b`)

// SetupTOTP enrolls and activates TOTP for the currently authenticated ChatGPT
// web session. The returned result is compatible with chatgpt-totp-setup.json.
func (f *Flow) loadCheckpoint() error {
	if f.Options.CheckpointRestoreManaged || f.checkpoint != nil || f.Options.CheckpointPath == "" {
		return nil
	}
	c, err := ReadCheckpoint(f.Options.CheckpointPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	return f.Restore(c)
}

func (f *Flow) SetupTOTP(ctx context.Context, resultPath string) (map[string]any, error) {
	if err := f.loadCheckpoint(); err != nil {
		return nil, err
	}
	mayRecover := f.checkpoint != nil || f.canReuseLogin()
	for attempt := 0; ; attempt++ {
		changed := false
		var out map[string]any
		err := f.prepareEnrollmentSession(ctx)
		if err == nil {
			out, err = f.setupTOTPAttempt(ctx, resultPath, &changed)
		}
		if err == nil || attempt != 0 || !mayRecover || changed || ctx.Err() != nil || !isExpiredSession(err) {
			return out, err
		}
		if err = f.resetExpiredSession(ctx); err != nil {
			return nil, err
		}
	}
}

func (f *Flow) setupTOTPAttempt(ctx context.Context, resultPath string, changed *bool) (map[string]any, error) {
	page, err := f.follow(ctx, f.Options.ChatGPTBase+"/?action=enable&factor=totp", f.Options.ChatGPTBase+"/")
	if err != nil {
		return nil, err
	}
	if f.isLoginURL(page.FinalURL) {
		return nil, expiredSession("TOTP_LOGIN_REQUIRED")
	}
	token := extractAccessToken(page.Response.Text())
	if token == "" {
		return nil, errors.New("TOTP_ACCESS_TOKEN_MISSING")
	}
	h := mfaHeaders(f.Options.ChatGPTBase, token, f.deviceID, "/backend-api/accounts/mfa_info")
	var info map[string]any
	if _, err = f.Client.JSON(ctx, http.MethodGet, f.Options.ChatGPTBase+"/backend-api/accounts/mfa_info", nil, RequestOptions{Headers: h, Referer: f.Options.ChatGPTBase + "/"}, &info); err != nil {
		return nil, err
	}
	if hasTOTP(info) {
		out := map[string]any{"version": 1, "already_enabled": true, "email": f.Options.Email}
		if resultPath != "" {
			if err := WritePrivateJSON(resultPath, out); err != nil {
				return nil, err
			}
		}
		if err := f.save("email_verified", nil); err != nil {
			return nil, err
		}
		return out, nil
	}
	h = mfaHeaders(f.Options.ChatGPTBase, token, f.deviceID, "/backend-api/accounts/mfa/enroll")
	var enroll map[string]any
	// After sending a state-changing enrollment request, its result may be
	// uncertain. Never restart the whole operation and enroll twice.
	*changed = true
	if _, err = f.Client.JSON(ctx, http.MethodPost, f.Options.ChatGPTBase+"/backend-api/accounts/mfa/enroll", map[string]any{"factor_type": "totp"}, RequestOptions{Headers: h, Origin: f.Options.ChatGPTBase, Referer: f.Options.ChatGPTBase + "/"}, &enroll); err != nil {
		return nil, err
	}
	secret := normalizeTOTPSecret(str(enroll["secret"]))
	sid := str(enroll["session_id"])
	if secret == "" || sid == "" {
		return nil, errors.New("TOTP_ENROLL_INVALID")
	}
	uri, err := BuildTOTPURI(f.Options.Email, secret)
	if err != nil {
		return nil, errors.New("TOTP_ENROLL_INVALID: invalid Base32 seed")
	}
	out := map[string]any{"version": 1, "already_enabled": false, "activation_mode": "automatic", "activation_succeeded": false, "email": f.Options.Email, "secret": secret, "otpauth_uri": uri}
	if f.Options.TOTPEnrollment != nil {
		if e := f.Options.TOTPEnrollment(ctx, secret, uri); e != nil {
			return nil, e
		}
	}
	// Persist the private pending result before activation so interruption
	// retains setup material instead of losing an enrolled, unactivated seed.
	if resultPath != "" {
		if err := WritePrivateJSON(resultPath, out); err != nil {
			return nil, err
		}
	}
	code, e := GenerateTOTP(secret, time.Now())
	if e != nil {
		return nil, e
	}
	for {
		h = mfaHeaders(f.Options.ChatGPTBase, token, f.deviceID, "/backend-api/accounts/mfa/user/activate_enrollment")
		var act map[string]any
		_, e = f.Client.JSON(ctx, http.MethodPost, f.Options.ChatGPTBase+"/backend-api/accounts/mfa/user/activate_enrollment", map[string]any{"code": code, "factor_type": "totp", "session_id": sid}, RequestOptions{Headers: h, Origin: f.Options.ChatGPTBase, Referer: f.Options.ChatGPTBase + "/"}, &act)
		if e == nil && act["success"] == true {
			out["activation_succeeded"] = true
			out["activated_at"] = nowRFC3339()
			break
		}
		if e != nil {
			var httpErr *HTTPError
			if !errors.As(e, &httpErr) || httpErr.StatusCode != http.StatusBadRequest {
				return nil, e
			}
		}
		for {
			code, e = f.ask(ctx, "totp_setup", "2FA setup OTP (6 digits, q=quit)")
			if e != nil {
				return nil, e
			}
			if digits6.MatchString(code) {
				break
			}
		}
	}
	if resultPath != "" {
		if err := WritePrivateJSON(resultPath, out); err != nil {
			return nil, err
		}
	}
	// Activation is already committed. A delayed or failed status query is
	// only a warning and must never trigger a second enrollment.
	var confirmed map[string]any
	h = mfaHeaders(f.Options.ChatGPTBase, token, f.deviceID, "/backend-api/accounts/mfa_info")
	if _, err = f.Client.JSON(ctx, http.MethodGet, f.Options.ChatGPTBase+"/backend-api/accounts/mfa_info", nil, RequestOptions{Headers: h, Referer: f.Options.ChatGPTBase + "/"}, &confirmed); err != nil || !hasTOTP(confirmed) {
		f.emit("totp_status_pending", "running", "2FA activation succeeded; enabled status is not yet confirmed")
	}
	if err := f.save("email_verified", nil); err != nil {
		return nil, err
	}
	return out, nil
}

// AddPassword performs the post-login password enrollment flow. Passwords are
// accepted only in memory and the result is written with mode 0600.
func (f *Flow) AddPassword(ctx context.Context, newPassword, resultPath string) error {
	// A cached session or a password verified during an earlier operation is
	// not proof that this pending value was applied. Only this call's fresh,
	// fully completed login may settle an interrupted password addition.
	f.verifiedPassword = ""
	defer func() { f.verifiedPassword = "" }()
	if err := f.loadCheckpoint(); err != nil {
		return err
	}
	if !validNewPassword(newPassword) {
		return errors.New("password must be 12-128 chars with upper/lower/digit/symbol")
	}
	mayRecover := f.checkpoint != nil || f.canReuseLogin()
	for attempt := 0; ; attempt++ {
		changed := false
		err := f.prepareEnrollmentSession(ctx)
		if err == nil {
			if f.verifiedPassword == newPassword {
				// The previous request may have committed remotely even if its
				// response was lost. A successful login with the exact candidate
				// proves the result without sending password/add again.
				err = f.finishPasswordAdded(newPassword, resultPath)
				if err == nil {
					f.emit("password_recovered", "running", "Previously submitted password confirmed by authenticated login")
				}
			} else {
				err = f.addPasswordAttempt(ctx, newPassword, resultPath, &changed)
			}
		}
		if err == nil || attempt != 0 || !mayRecover || changed || ctx.Err() != nil || !isExpiredSession(err) {
			return err
		}
		if err = f.resetExpiredSession(ctx); err != nil {
			return err
		}
	}
}

func (f *Flow) addPasswordAttempt(ctx context.Context, newPassword, resultPath string, changed *bool) error {
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
	*changed = true
	if _, e = f.Client.JSON(ctx, http.MethodPost, f.Options.AuthBase+"/api/accounts/password/add", map[string]any{"password": newPassword}, RequestOptions{Headers: h, Origin: f.Options.AuthBase, Referer: ref}, &data); e != nil {
		return e
	}
	if continuation(data) == "" && pageType(data) != "external_url" {
		return errors.New("ADD_PASSWORD_RESULT_INVALID")
	}
	return f.finishPasswordAdded(newPassword, resultPath)
}

func (f *Flow) finishPasswordAdded(newPassword, resultPath string) error {
	if resultPath != "" {
		if err := WritePrivateJSON(resultPath, map[string]any{"version": 1, "email": f.Options.Email, "password": newPassword, "added_at": nowRFC3339()}); err != nil {
			return err
		}
	}
	return f.save("email_verified", nil)
}
func mfaHeaders(origin, token, device, path string) http.Header {
	h := http.Header{}
	h.Set("Authorization", "Bearer "+token)
	h.Set("Oai-Device-Id", device)
	h.Set("Oai-Session-Id", randomUUID())
	h.Set("Oai-Language", "zh-CN")
	h.Set("X-Openai-Target-Path", path)
	h.Set("X-Openai-Target-Route", path)
	h.Set("Origin", origin)
	return h
}

func validNewPassword(password string) bool {
	n := len(utf16.Encode([]rune(password)))
	symbol := strings.IndexFunc(password, func(r rune) bool { return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') }) >= 0
	return n >= 12 && n <= 128 && strings.ContainsAny(password, "abcdefghijklmnopqrstuvwxyz") && strings.ContainsAny(password, "ABCDEFGHIJKLMNOPQRSTUVWXYZ") && strings.ContainsAny(password, "0123456789") && symbol
}
func hasTOTP(v map[string]any) bool {
	if v["mfa_enabled_v2"] != true {
		return false
	}
	f := payload(v["factors"])
	for _, x := range slice(f["totp"]) {
		a := payload(x)
		if str(a["factor_type"]) == "totp" || str(a["id"]) != "" {
			return true
		}
	}
	return false
}
func extractAccessToken(s string) string {
	for _, raw := range []string{s, html.UnescapeString(s)} {
		re := regexp.MustCompile(`["']accessToken["']\s*:\s*["']((?:\\.|[^"'\\])+)`)
		if m := re.FindStringSubmatch(raw); len(m) > 1 {
			var v string
			if json.Unmarshal([]byte(`"`+m[1]+`"`), &v) == nil && len(v) >= 20 {
				return v
			}
		}
	}
	return ""
}

// sessionResult retains the legacy private session export shape. It is never
// used by public task APIs or logs.
func (f *Flow) sessionResult(oauth map[string]any) (FlowResult, error) {
	if f.Options.SessionOutputPath == "" {
		return FlowResult{}, errors.New("private session output path is required")
	}
	value := map[string]any{"generated_at": nowRFC3339(), "chatgpt_base": f.Options.ChatGPTBase, "auth_base": f.Options.AuthBase, "warning": "This file contains login cookies and OAuth data. Do not share it.", "cookies": f.Client.Jar.Cookies(), "web": f.web, "codex": oauth}
	if err := WritePrivateJSON(f.Options.SessionOutputPath, value); err != nil {
		return FlowResult{}, err
	}
	return FlowResult{Web: f.web, OAuth: oauth}, nil
}
