package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"tosub2-go-port/internal/accountlock"
	"tosub2-go-port/internal/config"
	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/legacyruntime"
	"tosub2-go-port/internal/mail"
	"tosub2-go-port/internal/privatestate"
	"tosub2-go-port/internal/protocol"
	"tosub2-go-port/internal/transport"
)

func makeJobRunner(cfg config.Config, manager *jobs.Manager, logger *slog.Logger) (jobs.Runner, error) {
	if err := privatestate.Require(cfg.OutputRoot); err != nil {
		return nil, err
	}
	secureStore, storeErr := credentials.NewSecureFileStore(filepath.Join(cfg.OutputRoot, "credentials"))
	var credStore credentials.Store = secureStore
	durableCredentials := storeErr == nil
	if storeErr != nil {
		// Only an unconfigured, unsupported platform may run ephemerally.
		// An explicitly configured or previously bound vault must fail closed:
		// losing its key must never silently start a second, in-memory vault.
		if storeErr != credentials.ErrUnavailable {
			return nil, fmt.Errorf("secure credential store initialization failed: %w", storeErr)
		}
		credStore = credentials.NewMemoryStore()
		logger.Warn("credentials remain in memory; OS protected storage unavailable")
	}
	return makeJobRunnerWithCredentials(cfg, manager, logger, credStore, durableCredentials), nil
}

func makeJobRunnerWithCredentials(cfg config.Config, manager *jobs.Manager, logger *slog.Logger, credStore credentials.Store, durableCredentials bool) jobs.Runner {
	manager.SetCredentialStore(credStore)
	if protector, ok := credStore.(credentials.StateProtector); ok {
		if err := privatestate.Register(cfg.OutputRoot, protector); err != nil {
			return func(context.Context, *jobs.Job, func(jobs.JobEvent)) error { return err }
		}
	}
	runner := jobOperationRunner{cfg: cfg, manager: manager, logger: logger, credStore: credStore, durableCredentials: durableCredentials}

	upload := func(ctx context.Context, job *jobs.Job, emit func(jobs.JobEvent)) error {
		return uploadTOTPWorkflowAtOrigin(ctx, cfg.OutputRoot, cfg.ChatGPTBase, manager, job, emit)
	}
	return makeReloginUploadRunner(manager, makeTOTPWorkflowRunner(manager, credStore, runner.run, upload), upload)
}

type jobOperationRunner struct {
	cfg                config.Config
	manager            *jobs.Manager
	logger             *slog.Logger
	credStore          credentials.Store
	durableCredentials bool
}

func (r jobOperationRunner) run(ctx context.Context, job *jobs.Job, emit func(jobs.JobEvent)) (taskErr error) {
	cfg, manager, logger := r.cfg, r.manager, r.logger
	credStore, durableCredentials := r.credStore, r.durableCredentials

	observations := newAccountOperationObservation(manager, logger, job, cfg.ChatGPTBase, cfg.AuthBase)
	defer observations.CompleteOperation(ctx, &taskErr)
	vals, _ := manager.InputCopy(job.ID)
	if vals == nil {
		vals = map[string]any{}
	}
	op := strings.ToLower(strings.TrimSpace(job.Operation))
	var resetOutcomeReported bool
	if op == "reset-2fa" || op == "complete-2fa" {
		defer func() {
			if !resetOutcomeReported {
				emitLocalResetDiagnostic(job, emit, nil, taskErr, false)
			}
		}()
	}
	if jobs.IsRemovedAuthenticationOperation(op) {
		return errors.New("TOTP_OPERATION_REMOVED: 独立核实和远端恢复已移除；仅已确认激活的记录可执行本地完成")
	}
	if op == "complete-2fa" {
		if !durableCredentials || credStore == nil {
			return errors.New("TOTP_RESET_PERSISTENCE_REQUIRED: 本地完成需要可用的持久安全凭据存储")
		}
		release, err := acquireOperationAccountLock(cfg.ChatGPTBase, job.Email)
		if err != nil {
			return err
		}
		defer func() { taskErr = errors.Join(taskErr, release()) }()
		if err := manager.CanCompleteTOTPReset(job.ID); err != nil {
			return err
		}
		path := filepath.Join(cfg.OutputRoot, job.ID, "totp-setup-result.json")
		result, readErr := protocol.ReadTOTPResetCompletion(path, job.Email)
		if readErr != nil {
			return readErr
		}
		err = finishBoundTOTPCompletion(ctx, path, manager, credStore, job, result)
		emitLocalResetDiagnostic(job, emit, result, err, err == nil)
		resetOutcomeReported = true
		return err
	}
	resetTOTP := op == "reset-2fa"
	setupTOTP := op == "setup-2fa" || op == "setup_2fa" || op == "setup-totp" || op == "totp_setup"
	if !resetTOTP && !setupTOTP && (strings.Contains(op, "totp") || strings.Contains(op, "2fa")) {
		return errors.New("TOTP_OPERATION_UNSUPPORTED: 不支持的 2FA 操作；未创建登录会话或发送请求")
	}
	authorizationMethod, methodErr := operationAuthorizationMethod(job.AuthorizationMethod, op)
	if methodErr != nil {
		return methodErr
	}
	if resetTOTP || setupTOTP || durableCredentials {
		release, err := acquireOperationAccountLock(cfg.ChatGPTBase, job.Email)
		if err != nil {
			return err
		}
		defer func() { taskErr = errors.Join(taskErr, release()) }()
	}
	if resetTOTP {
		if err := validateTOTPResetStart(cfg.OutputRoot, job.ID, job.Email, durableCredentials); err != nil {
			return err
		}
	}
	if setupTOTP && (credStore == nil || !durableCredentials) {
		return errors.New("TOTP_SETUP_PERSISTENCE_REQUIRED: 设置 2FA 需要可用的持久安全凭据存储")
	}
	grantPath := jobs.OAuthGrantPath(cfg.OutputRoot, job)
	if grantPath == "" {
		return errors.New("OAUTH_RESULT_ID_INVALID: 本地授权结果标识无效，未读取旧结果或连接远端")
	}
	outputPath := filepath.Join(cfg.OutputRoot, job.ID, "sub2api-import-oauth.json")
	if cfg.CLI.Enabled() {
		if v, ok := vals["grantPath"].(string); cfg.CLI.CompleteOAuthGrant != "" && ok && v != "" {
			grantPath = v
		}
		if v, ok := vals["outputPath"].(string); ok && v != "" {
			outputPath = v
		}
	}
	refreshRequested := op == "refresh" || op == "regenerate" || op == "reauthorize"
	refreshSource, refreshTarget := outputPath, outputPath
	if refreshRequested && cfg.CLI.Enabled() && cfg.CLI.RefreshSub2API != "" {
		var err error
		refreshSource, refreshTarget, err = cliRefreshPaths(cfg.CLI)
		if err != nil {
			return err
		}
	}
	if refreshRequested && op != "refresh" && refreshUsesDifferentMethod(refreshSource, authorizationMethod, cfg.CodexClientID) {
		refreshRequested, op = false, "relogin"
		observations.Switch(ctx, "authentication", nil)
	}
	completeRefresh := func(message string) {
		// The stable CLI destination owns the accepted grant. A failed job
		// cache copy must never cause another rotating grant or login.
		saved := true
		if refreshTarget != outputPath {
			if err := copyCLIPrivateJSON(refreshTarget, outputPath); err != nil {
				saved = false
				logger.Warn("refresh output saved; optional job download cache unavailable", "jobId", job.ID)
			}
		}
		manager.UpdateResult(job.ID, saved, message)
	}
	if refreshRequested {
		if handled, err := finishPendingRefreshAt(ctx, cfg.AuthBase, cfg.CodexClientID, refreshSource, refreshTarget, job.Email); handled || err != nil {
			if err == nil {
				completeRefresh("已从保存的刷新授权恢复输出文件")
			}
			return err
		}
	}
	if op == "complete-oauth-grant" || op == "resume" {
		grant, err := protocol.ReadSavedOAuthGrant(grantPath)
		if err != nil {
			return fmt.Errorf("OAUTH_GRANT_REQUIRED: only already received tokens can be completed locally: %w", err)
		}
		result, err := protocol.FinalizeSavedOAuthGrant(ctx, grant, savedGrantOptions(cfg, job.Email, grantPath, outputPath, authorizationMethod, vals))
		if err == nil {
			manager.UpdateResult(job.ID, len(result.Export.Accounts) > 0, "已保存已取得的授权令牌；未发起网络请求")
		}
		return err
	}
	savedCredentials, err := prepareOperationCredentials(ctx, manager, credStore, job, op, vals)
	if err != nil {
		return err
	}
	proxyURL := cfg.ProxyURL
	if v, ok := vals["proxyUrl"].(string); ok {
		proxyURL = v
	}
	password, _ := vals["password"].(string)
	if password == "" {
		password = os.Getenv("CHATGPT_LOGIN_PASSWORD")
	}
	loginPassword := password
	if loginPassword == "" {
		loginPassword = savedCredentials.PendingPassword
	}
	totpSecret, _ := vals["totpSecret"].(string)
	if totpSecret == "" {
		totpSecret = os.Getenv("CHATGPT_TOTP_SECRET")
	}
	totpCandidate := totpSecret
	totpFactorID := credentialTOTPFactorID(savedCredentials, totpCandidate)
	setupResultPath := filepath.Join(cfg.OutputRoot, job.ID, "totp-setup-result.json")
	if setupTOTP && cfg.CLI.Enabled() {
		setupResultPath = cliOperationResultPath(cfg.CLI, true)
	}
	if setupTOTP {
		if err := protocol.ValidateTOTPSetupStart(setupResultPath, job.Email); err != nil {
			return err
		}
	}
	phone, _ := vals["phone"].(string)
	mailURL, _ := vals["mailApiUrl"].(string)
	mailMethod, _ := vals["mailRequestMethod"].(string)
	mailBody, _ := vals["mailRequestBody"].(string)
	mailHeaders := map[string]string{}
	if raw, ok := vals["mailRequestHeaders"].(map[string]string); ok {
		mailHeaders = raw
	} else if raw, ok := vals["mailRequestHeaders"].(map[string]any); ok {
		for k, v := range raw {
			if x, ok := v.(string); ok {
				mailHeaders[k] = x
			}
		}
	}
	protectCredentials := operationCredentialProtector(ctx, credStore, job.Email, setupTOTP || durableCredentials, func(value credentials.Credentials) {
		manager.UpdateStoredCredentials(job.ID, value)
	})
	if strings.Contains(op, "password") && durableCredentials {
		resultPath := passwordOperationResultPath(cfg, job.ID)
		recovery, readErr := protocol.ReadPasswordAddRecovery(resultPath, job.Email)
		if readErr != nil {
			return readErr
		}
		if recovery != nil {
			requested, _ := vals["newPassword"].(string)
			if requested == "" {
				requested = os.Getenv("CHATGPT_NEW_PASSWORD")
			}
			if requested != "" && requested != recovery.Password || savedCredentials.PendingPassword != recovery.Password && savedCredentials.Password != recovery.Password {
				return errors.New("PASSWORD_ADD_RECOVERY_CONFLICT: 添加密码恢复材料与当前凭据或输入不一致，未登录或修改远端")
			}
			value := savedCredentials
			value.Password, value.PendingPassword = recovery.Password, ""
			if err := protectCredentials(value, false, true); err != nil {
				return err
			}
			if err := manager.UpdatePasswordAdded(job.ID, true, ""); err != nil {
				return err
			}
			if !cfg.CLI.Enabled() {
				if err := protocol.RemovePasswordAddEvidence(resultPath); err != nil {
					return err
				}
			}
			emit(jobs.JobEvent{JobID: job.ID, Stage: "password_recovered", Status: jobs.StatusRunning, Prompt: "已从保存的成功回执完成密码本地保存；未登录或修改远端"})
			return nil
		}
	}
	reportNetworkRetry := networkRetryReporter(ctx, job.ID, "", emit)
	interaction := newOperationInput(cfg, manager, job.ID, emit, mailURL, mail.RequestConfig{
		Method: mailMethod, Body: mailBody, Headers: mailHeaders, OnRetry: reportNetworkRetry,
	}, func(value string) { password = value })
	if resetTOTP {
		if err := preserveAccountCredentialBaseline(ctx, credStore, job.Email, proxyURL); err != nil {
			return err
		}
	}
	var doer protocol.Doer
	resolvedProfile := cfg.TLSProfile
	var fingerprint *transport.FingerprintClient
	jar := transport.NewCookieJar()
	var fpErr error
	probeConfig := cfg
	if refreshRequested || authorizationMethod == protocol.AuthorizationMethodBPS {
		// A refresh grant depends on the authorization server, not ChatGPT's
		// web home or an unrelated web login checkpoint.
		probeConfig.ChatGPTBase = cfg.AuthBase
	}
	if cfg.CLI.NativeHTTP {
		var native *transport.Client
		native, jar, proxyURL, fpErr = prepareNative(ctx, probeConfig, proxyURL)
		if fpErr != nil {
			return fmt.Errorf("native HTTP transport unavailable: %w", fpErr)
		}
		defer native.Close()
		doer, resolvedProfile = native, transport.NativeProfile
	} else if refreshRequested {
		fingerprint, jar, proxyURL, fpErr = prepareFingerprint(ctx, probeConfig, proxyURL)
		if fpErr != nil {
			// A standard net/http fallback changes the ClientHello and breaks the
			// selected profile's session binding. Fail the authorized task clearly
			// so callers can choose a supported profile instead.
			return fmt.Errorf("TLS fingerprint transport unavailable for %q: %w", cfg.TLSProfile, fpErr)
		}
		defer fingerprint.Close()
		doer = fingerprint
		resolvedProfile = fingerprint.Profile()
	}
	pc := protocol.NewClient(doer, jar, resolvedProfile)
	pc.OnRequestResult = observations.ObserveResult
	if cfg.CLI.NativeHTTP {
		pc.UserAgent = transport.NativeUserAgent
	}
	pc.ConnectionRetries = cfg.SameProxyRetries
	pc.RetryDelay = time.Duration(cfg.SameProxyRetryDelayMS) * time.Millisecond
	pc.OnRetry = reportNetworkRetry
	if cfg.RequestTimeoutMS > 0 {
		pc.Timeout = time.Duration(cfg.RequestTimeoutMS) * time.Millisecond
	}
	if refreshRequested {
		if e := refreshExistingExportAt(ctx, pc, cfg.AuthBase, cfg.CodexClientID, refreshSource, refreshTarget, job.Email); e == nil {
			completeRefresh("OAuth 导入文件已刷新")
			return nil
		} else if op == "refresh" || ctx.Err() != nil || isLocalRefreshFailure(e) {
			return e
		} else {
			observations.Switch(ctx, "authentication", e)
		}
		// A new, explicitly requested reauthorization follows a failed grant.
		// Refresh cookies are never imported into this authentication session.
		op = "relogin"
		pc.Jar = transport.NewCookieJar()
		if setter, ok := pc.Transport.(interface{ SetJar(*transport.CookieJar) }); ok {
			setter.SetJar(pc.Jar)
		}
	}
	var legacyTransport *legacyruntime.Client
	if !cfg.CLI.NativeHTTP {
		var err error
		// Full login after a rejected refresh uses its selected provider.
		probeConfig = authorizationProbeConfig(cfg, authorizationMethod)
		emit(jobs.JobEvent{JobID: job.ID, Stage: "legacy_session_start", Status: jobs.StatusRunning, Prompt: "正在建立协议登录会话"})
		legacyTransport, pc.Jar, proxyURL, err = prepareLegacyAuthentication(ctx, probeConfig, proxyURL, reportNetworkRetry)
		if err != nil {
			return err
		}
		defer func() {
			if err := legacyTransport.Close(); err != nil {
				taskErr = errors.Join(taskErr, fmt.Errorf("legacy runtime cleanup failed: %w", err))
			}
		}()
		pc.Transport = legacyTransport
		pc.UserAgent, pc.AcceptLanguage = legacyTransport.UserAgent(), legacyTransport.AcceptLanguage()
		resolvedProfile = legacyProfilePrefix + legacyTransport.Profile()
		emit(jobs.JobEvent{JobID: job.ID, Stage: "legacy_bound_transport", Status: jobs.StatusRunning, Prompt: "认证请求与动态脚本共用同一原版协议会话", Data: map[string]any{"tlsProfile": resolvedProfile}})
	}
	pc.Challenge = func(ctx context.Context, rawURL string) error {
		if legacyTransport != nil {
			emit(jobs.JobEvent{JobID: job.ID, Stage: "legacy_challenge", Status: jobs.StatusRunning, Prompt: "正在原版协议会话中处理网站验证"})
			return legacyTransport.Challenge(ctx, rawURL)
		}
		return errors.New("AUTH_CHALLENGE_UNAVAILABLE: this operation has no Go authentication session")
	}
	flow := protocol.NewFlow(pc, protocol.FlowOptions{AuthorizationMethod: authorizationMethod, ChatGPTBase: cfg.ChatGPTBase, AuthBase: cfg.AuthBase, Email: job.Email, Password: loginPassword, TOTPSecret: totpCandidate, TOTPFactorID: totpFactorID, Phone: phone, ClientID: cfg.CodexClientID, RedirectURI: cfg.CodexRedirectURI, GrantPath: grantPath, OutputPath: outputPath, Input: interaction.read, AuthStep: func(name string, automatic bool) {
		if name == "password" && automatic {
			// The saved credential candidate is already in FlowOptions.Password;
			// consume only its matching copy so rejection waits for new input.
			manager.DiscardInputIfEqual(job.ID, "password", loginPassword)
		}
		interaction.authStep(name, automatic)
	}, PasswordVerified: func(ctx context.Context, verified string) error {
		// Password addition saves its new candidate before this login. Verifying
		// the current password must retain that candidate until the later remote
		// addition is confirmed; a lost response still needs it after restart.
		if e := protectCredentials(credentials.Credentials{Password: verified, TOTPSecret: totpSecret, ProxyURL: proxyURL, MailAPIURL: mailURL, MailMethod: mailMethod, MailBody: mailBody, MailHeaders: mailHeaders}, false, !strings.Contains(op, "password")); e != nil {
			return fmt.Errorf("cannot protect verified password: %w", e)
		}
		password = verified
		return nil
	}, TOTPFactorVerified: func(ctx context.Context, secret, factorID string) error {
		if err := protectCredentials(credentials.Credentials{Password: password, TOTPSecret: secret, TOTPFactorID: factorID, ProxyURL: proxyURL, MailAPIURL: mailURL, MailMethod: mailMethod, MailBody: mailBody, MailHeaders: mailHeaders}, savedCredentials.PendingTOTPSecret == secret, false); err != nil {
			return fmt.Errorf("cannot protect verified TOTP factor binding: %w", err)
		}
		totpSecret, totpFactorID = secret, factorID
		return nil
	}, TOTPEnrollment: func(ctx context.Context, secret, uri string) error {
		// Store the enrollment seed before activation can change the account.
		// Keep the obtained material even if the independent metadata commit
		// fails, but never cross the activation boundary after that failure.
		factorID, err := setupEnrollmentFactorID(setupResultPath, job.Email, secret, uri)
		if err != nil {
			return err
		}
		if err := protectCredentials(credentials.Credentials{Password: password, TOTPSecret: totpSecret, PendingTOTPSecret: secret, PendingTOTPURI: uri, PendingTOTPFactorID: factorID, ProxyURL: proxyURL, MailAPIURL: mailURL, MailMethod: mailMethod, MailBody: mailBody, MailHeaders: mailHeaders}, true, false); err != nil {
			return err
		}
		return manager.UpdateTOTPSetup(job.ID, secret, uri, "", false)
	}, BeforeEmailOTP: interaction.beforeEmailOTP, EmailOTPRequested: interaction.emailOTPRequested, EventData: func(stage, status, prompt string, data map[string]any) {
		// Flow asks before Input chooses automatic mail versus manual stdin.
		// Only Input may publish a waiting prompt: forwarding both events can
		// cancel automated CLI runs at EOF or consume the next manual OTP early.
		if status == "awaiting_input" || status == jobs.StatusWaitingInput {
			return
		}
		emit(jobs.JobEvent{JobID: job.ID, Stage: stage, Status: status, Prompt: prompt, Data: data, At: time.Now().UTC()})
	}, Sentinel: func(ctx context.Context, authBase, device, flowName string) (http.Header, error) {
		if legacyTransport != nil {
			return legacyTransport.Sentinel(ctx, authBase, device, flowName, authBase+sentinelPagePath(flowName))
		}
		return nil, errors.New("SENTINEL_UNAVAILABLE: this operation has no Go authentication session")
	}})
	flow.Options.WorkspaceID, _ = vals["workspaceId"].(string)
	if cfg.CLI.Enabled() {
		flow.Options.OutputMode, _ = vals["outputMode"].(string)
		flow.Options.AccountName, _ = vals["accountName"].(string)
		flow.Options.Concurrency, _ = vals["sub2apiConcurrency"].(int)
		flow.Options.Priority, _ = vals["priority"].(int)
		flow.Options.RateMultiplier, _ = vals["rateMultiplier"].(float64)
	}
	if resetTOTP {
		resultPath := filepath.Join(cfg.OutputRoot, job.ID, "totp-setup-result.json")
		configureTOTPReset(flow, manager, credStore, job, resultPath)
		result, resetErr := flow.ResetTOTP(ctx, resultPath)
		committed := false
		operationID, _ := result["operation_id"].(string)
		if current, ok := manager.Get(job.ID); ok && operationID != "" && current.TOTPResetOperationID == operationID {
			committed = current.TOTPResetCommitted
		}
		if result != nil {
			defer func() { flow.EmitResetOutcome(result, taskErr, committed); resetOutcomeReported = true }()
		}
		observations.LogoutResult(result)
		stateErr := manager.UpdateTOTPResetResult(job.ID, result, resetErr)
		if stateErr != nil {
			return errors.Join(resetErr, stateErr)
		}
		if result["credential_persisted"] != true || committed {
			return resetErr
		}
		commitErr := finishTOTPResetLocally(ctx, manager, job, operationID)
		committed = commitErr == nil
		return errors.Join(resetErr, commitErr)
	}
	if setupTOTP {
		resultPath := setupResultPath
		result, err := flow.SetupTOTP(ctx, resultPath)
		if err != nil {
			return errors.Join(err, manager.UpdateTOTPSetup(job.ID, "", "", "TOTP 设置失败，已生成的密钥保留在凭据存储", false))
		}
		if already, _ := result["already_enabled"].(bool); already {
			recoveryHint := ""
			if savedCredentials.PendingTOTPSecret != "" {
				recoveryHint = "2FA 已启用；中断时保留的密钥尚未确认，请保留材料并人工处理"
			}
			if err := manager.UpdateTOTPSetup(job.ID, "", "", recoveryHint, true); err != nil {
				return err
			}
			if resultPath != "" {
				_ = os.Remove(resultPath)
			}
			return nil
		}
		if !protocol.SetupTOTPSeedVerified(result) {
			message := "TOTP_SETUP_SEED_UNVERIFIED: 官方已确认人工验证码激活成功，但保存的候选密钥尚未验证；保留待验证材料，不重复登记或激活"
			return errors.Join(errors.New(message), manager.MarkTOTPSetupUnverified(job.ID, message))
		}
		secret, _ := result["secret"].(string)
		if secret == "" {
			return errors.New("TOTP setup result missing secret")
		}
		var e error
		uri, _ := result["otpauth_uri"].(string)
		factorID, _ := result["new_totp_factor_id"].(string)
		if e = protectCredentials(credentials.Credentials{Password: password, TOTPSecret: secret, TOTPFactorID: factorID, ProxyURL: proxyURL, MailAPIURL: mailURL, MailMethod: mailMethod, MailBody: mailBody, MailHeaders: mailHeaders}, true, false); e != nil {
			return errors.Join(fmt.Errorf("credential save failed after TOTP setup: %w", e), manager.UpdateTOTPSetup(job.ID, secret, uri, e.Error(), false))
		}
		if err := manager.UpdateTOTPSetup(job.ID, secret, uri, "", true); err != nil {
			return err
		}
		if !cfg.CLI.Enabled() {
			_ = os.Remove(resultPath)
		}
		return nil
	}
	if strings.Contains(strings.ToLower(job.Operation), "password") {
		if credStore == nil {
			return errors.New("secure credential store unavailable; refusing to add password")
		}
		vals, _ := manager.InputCopy(job.ID)
		pw, _ := vals["newPassword"].(string)
		if pw == "" {
			pw = os.Getenv("CHATGPT_NEW_PASSWORD")
		}
		if pw == "" {
			pw = savedCredentials.PendingPassword
		}
		if pw == "" {
			var e error
			pw, e = randomStrongPassword()
			if e != nil {
				return e
			}
		}
		resultPath := passwordOperationResultPath(cfg, job.ID)
		if !durableCredentials {
			resultPath = ""
		}
		// Keep a newly chosen password in the protected store before the
		// remote mutation. An unknown result retains this candidate without
		// replaying the write; confirmed receipts permit local completion.
		if e := protectCredentials(credentials.Credentials{Password: password, PendingPassword: pw, TOTPSecret: totpSecret, ProxyURL: proxyURL, MailAPIURL: mailURL, MailMethod: mailMethod, MailBody: mailBody, MailHeaders: mailHeaders}, false, true); e != nil {
			return fmt.Errorf("credential preflight failed: %w", e)
		}
		passwordResult, e := flow.AddPassword(ctx, pw, resultPath)
		if e != nil {
			return errors.Join(e, manager.UpdatePasswordAdded(job.ID, false, e.Error()))
		}
		if passwordResult.Outcome == protocol.PasswordAlreadyExists {
			// PasswordVerified already saved the actual existing password. The
			// unused candidate must not become a successful password or result.
			if e := protectCredentials(credentials.Credentials{Password: password, TOTPSecret: totpSecret, ProxyURL: proxyURL, MailAPIURL: mailURL, MailMethod: mailMethod, MailBody: mailBody, MailHeaders: mailHeaders}, false, true); e != nil {
				return fmt.Errorf("cannot finish existing-password confirmation: %w", e)
			}
			return manager.MarkPasswordExisting(job.ID)
		}
		if credStore != nil {
			if e := protectCredentials(credentials.Credentials{Password: pw, TOTPSecret: totpSecret, ProxyURL: proxyURL, MailAPIURL: mailURL, MailMethod: mailMethod, MailBody: mailBody, MailHeaders: mailHeaders}, false, true); e != nil {
				return fmt.Errorf("credential save failed after password add: %w", e)
			}
		}
		if e := manager.UpdatePasswordAdded(job.ID, true, ""); e != nil {
			return e
		}
		if !cfg.CLI.Enabled() {
			if e := protocol.RemovePasswordAddEvidence(resultPath); e != nil {
				return e
			}
		}
		return nil
	}
	result, err := flow.Run(ctx)
	if err == nil {
		if credStore != nil {
			if e := protectCredentials(credentials.Credentials{Password: password, TOTPSecret: totpSecret, ProxyURL: proxyURL, MailAPIURL: mailURL, MailMethod: mailMethod, MailBody: mailBody, MailHeaders: mailHeaders}, false, false); e != nil {
				return fmt.Errorf("OAuth output saved but secure credential save failed: %w", e)
			}
		}
		automated := !interaction.manualAuth && len(interaction.requirements) > 0 && (password != "" || mailURL != "")
		reason := "上次完整登录使用可恢复资料自动完成"
		if !automated {
			reason = "上次授权需要人工输入或没有可恢复登录资料"
		}
		manager.RecordAuthorization(job.ID, automated, reason, interaction.requirements)
		manager.UpdateResult(job.ID, len(result.Export.Accounts) > 0, "输出文件已生成")
	}
	var authErr *protocol.HTTPError
	if errors.As(err, &authErr) {
		switch authErr.Code {
		case "account_deactivated", "account_deleted", "account_suspended", "account_disabled", "user_deactivated", "user_deleted", "user_suspended", "user_disabled":
			manager.UpdateAutoRepair(job.ID, job.AutoRepairPendingAccountIDs, job.AutoRepairBackend, authErr.Code, true)
		}
	}
	return err
}

func passwordOperationResultPath(cfg config.Config, id string) string {
	if cfg.CLI.Enabled() {
		return cliOperationResultPath(cfg.CLI, false)
	}
	return filepath.Join(cfg.OutputRoot, id, "password-add-result.json")
}

func acquireOperationAccountLock(origin, email string) (func() error, error) {
	if origin == "" {
		origin = "https://chatgpt.com"
	}
	return accountlock.Acquire(origin, email)
}
