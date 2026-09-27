package main

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"tosub2-go-port/internal/browser"
	"tosub2-go-port/internal/challenge"
	"tosub2-go-port/internal/config"
	"tosub2-go-port/internal/console"
	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/logging"
	"tosub2-go-port/internal/mail"
	"tosub2-go-port/internal/protocol"
	"tosub2-go-port/internal/store"
	"tosub2-go-port/internal/transport"
)

// dist contains the production static console. Build pipelines may replace
// these files with the React output; the server never invokes Node at runtime.
//
//go:embed web/dist
var staticFiles embed.FS

func main() {
	cfg, err := config.Parse(os.Args[1:])
	if err != nil {
		slog.Error("configuration error", "error", err)
		os.Exit(2)
	}
	logger := logging.New()
	rootCtx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	if handled, err := runCredentialImport(rootCtx, cfg, os.Stdout); handled {
		if err != nil {
			logger.Error("offline credential import failed", "error", err)
			os.Exit(1)
		}
		return
	}
	if handled, err := runPlatformService(cfg, logger); handled {
		if err != nil {
			logger.Error("service operation failed", "error", err)
			os.Exit(1)
		}
		return
	}
	if handled, err := runCLI(rootCtx, cfg, logger); handled {
		if err != nil {
			logger.Error("CLI operation failed", "error", err)
			os.Exit(1)
		}
		return
	}
	if err := runConsole(rootCtx, cfg, logger, nil); err != nil {
		logger.Error("console service failed", "error", err)
		os.Exit(1)
	}
}

// runConsole shares cancellation between signals and Windows Service Control.
// ready is called only after bind, restore and startup.
func runConsole(parent context.Context, cfg config.Config, logger *slog.Logger, ready func()) error {
	rootCtx, cancelRoot := context.WithCancel(parent)
	defer cancelRoot()
	st, err := store.New(cfg.OutputRoot)
	if err != nil {
		return fmt.Errorf("store initialization failed: %w", err)
	}
	listener, err := net.Listen("tcp", cfg.Addr())
	if err != nil {
		return fmt.Errorf("HTTP listener initialization failed: %w", err)
	}
	defer listener.Close()
	manager := jobs.NewManager(rootCtx, cfg.MaxActiveJobs, st, logger)
	manager.SetRunner(makeJobRunner(cfg, manager, logger))
	if err := manager.Restore(rootCtx); err != nil {
		// Failed recovery must not replace original metadata with a partial snapshot.
		return fmt.Errorf("job metadata restore failed: %w", err)
	}
	manager.Start()
	consoleServer := console.NewServer(cfg, manager, staticFiles, logger)
	monitorCtx, stopMonitor := context.WithCancel(rootCtx)
	defer stopMonitor()
	consoleServer.StartMonitor(monitorCtx)
	// Sub2API allows 120 seconds per upstream request and may process a batch
	// sequentially. A global 60-second write deadline used to discard valid
	// results before the handler could reply. Each outbound call remains
	// bounded; root cancellation also cancels foreground request handlers.
	server := &http.Server{
		Addr: cfg.Addr(), Handler: consoleServer.Handler(),
		ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 60 * time.Second, IdleTimeout: 120 * time.Second,
		BaseContext: func(net.Listener) context.Context { return rootCtx },
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(listener) }()
	logger.Info("toSub2 Go console listening", "addr", cfg.Addr(), "outputRoot", cfg.OutputRoot)
	if cfg.Host != "localhost" && (net.ParseIP(cfg.Host) == nil || !net.ParseIP(cfg.Host).IsLoopback()) {
		logger.Warn("console is listening on a non-loopback interface; use a trusted LAN and never expose it directly to the public Internet")
	}
	if ready != nil {
		ready()
	}
	var serveErr error
	select {
	case <-rootCtx.Done():
	case serveErr = <-serveDone:
		cancelRoot()
	}
	stopMonitor()
	timeout := time.Duration(cfg.ShutdownTimeoutMS) * time.Millisecond
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var closeErrors []error
	if err := server.Shutdown(shutdownCtx); err != nil {
		_ = server.Close()
		closeErrors = append(closeErrors, fmt.Errorf("HTTP shutdown: %w", err))
	}
	if err := consoleServer.CloseContext(shutdownCtx); err != nil {
		closeErrors = append(closeErrors, fmt.Errorf("console resource shutdown: %w", err))
	}
	if err := manager.Shutdown(shutdownCtx); err != nil {
		closeErrors = append(closeErrors, fmt.Errorf("job manager shutdown: %w", err))
	}
	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		closeErrors = append(closeErrors, fmt.Errorf("HTTP server stopped: %w", serveErr))
	}
	logger.Info("toSub2 stopped")
	return errors.Join(closeErrors...)
}

func makeJobRunner(cfg config.Config, manager *jobs.Manager, logger *slog.Logger) jobs.Runner {
	secureStore, storeErr := credentials.NewSecureFileStore(filepath.Join(cfg.OutputRoot, "credentials"))
	var credStore credentials.Store = secureStore
	durableCredentials := storeErr == nil
	if storeErr != nil {
		credStore = credentials.NewMemoryStore()
		logger.Warn("credentials remain in memory; OS protected storage unavailable")
	}
	return makeJobRunnerWithCredentials(cfg, manager, logger, credStore, durableCredentials)
}

func makeJobRunnerWithCredentials(cfg config.Config, manager *jobs.Manager, logger *slog.Logger, credStore credentials.Store, durableCredentials bool) jobs.Runner {
	browsers := browser.NewManager(cfg.ChromePath, 4)
	manager.SetCredentialStore(credStore)
	return func(ctx context.Context, job *jobs.Job, emit func(jobs.JobEvent)) (taskErr error) {
		vals, _ := manager.InputCopy(job.ID)
		if vals == nil {
			vals = map[string]any{}
		}
		op := strings.ToLower(strings.TrimSpace(job.Operation))
		checkpointPath := filepath.Join(cfg.OutputRoot, job.ID, "login-checkpoint.json")
		outputPath := filepath.Join(cfg.OutputRoot, job.ID, "sub2api-import-oauth.json")
		if cfg.CLI.Enabled() {
			if v, ok := vals["checkpointPath"].(string); ok && v != "" {
				checkpointPath = v
			}
			if v, ok := vals["outputPath"].(string); ok && v != "" {
				outputPath = v
			}
		}
		var restored *protocol.Checkpoint
		refreshRequested := op == "refresh" || op == "regenerate" || op == "reauthorize"
		refreshSource, refreshTarget := outputPath, outputPath
		if refreshRequested && cfg.CLI.Enabled() && cfg.CLI.RefreshSub2API != "" {
			var err error
			refreshSource, refreshTarget, err = cliRefreshPaths(cfg.CLI)
			if err != nil {
				return err
			}
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
		if shouldRestoreCheckpoint(cfg, op) {
			if cp, e := protocol.ReadCheckpoint(checkpointPath); e == nil {
				if e := validateAuthenticationCheckpoint(cp, cfg, job.Email); e != nil {
					return e
				}
				restored = &cp
			} else if !errors.Is(e, os.ErrNotExist) {
				return e
			}
		}
		if restored != nil && protocol.HasSavedOAuthGrant(*restored) && (op == "" || op == "login" || op == "full" || op == "resume") {
			result, err := protocol.FinalizeSavedOAuthGrant(ctx, *restored, savedGrantOptions(cfg, job.Email, checkpointPath, outputPath, vals))
			if err == nil {
				manager.UpdateResult(job.ID, len(result.Export.Accounts) > 0, "已从保存的授权结果恢复输出文件")
			}
			return err
		}
		savedCredentials, credentialErr := credentials.LoadContext(ctx, credStore, job.Email)
		if credentialErr != nil && !errors.Is(credentialErr, os.ErrNotExist) {
			return fmt.Errorf("secure credentials could not be loaded: %w", credentialErr)
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
		proxyURL := cfg.ProxyURL
		if v, ok := vals["proxyUrl"].(string); ok {
			proxyURL = v
		}
		if restored != nil {
			binding, present, e := protocol.CheckpointTransportSession(*restored)
			if e != nil {
				return e
			}
			if present {
				if protocol.ProxyDigest(proxyURL) != binding.ProxySHA256 {
					if protocol.ProxyDigest(savedCredentials.ProxyURL) != binding.ProxySHA256 {
						return errors.New("CHECKPOINT_SESSION_MISMATCH: saved proxy session unavailable; re-login required")
					}
					proxyURL = savedCredentials.ProxyURL
				}
			}
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
		// An interrupted activation may have succeeded remotely. The pending
		// seed is only a login candidate; it becomes active after MFA accepts it.
		totpCandidate := totpSecret
		pendingTOTPRecovery := totpCandidate == "" && savedCredentials.PendingTOTPSecret != ""
		if pendingTOTPRecovery {
			totpCandidate = savedCredentials.PendingTOTPSecret
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
		protectCredentials := func(value credentials.Credentials, clearPendingTOTP, clearPendingPassword bool) error {
			old, e := credentials.LoadContext(ctx, credStore, job.Email)
			if e != nil && !errors.Is(e, os.ErrNotExist) {
				return e
			}
			if !clearPendingTOTP && value.PendingTOTPSecret == "" {
				value.PendingTOTPSecret, value.PendingTOTPURI = old.PendingTOTPSecret, old.PendingTOTPURI
			}
			if !clearPendingPassword && value.PendingPassword == "" {
				value.PendingPassword = old.PendingPassword
			}
			if e = credentials.SaveContext(ctx, credStore, job.Email, value); e != nil {
				return e
			}
			manager.UpdateStoredCredentials(job.ID, value)
			return nil
		}
		var sess *browser.Session
		requirements := map[string]bool{}
		manualAuth := false
		mailState := newMailInputState()
		mailRequest := mail.RequestConfig{Method: mailMethod, Body: mailBody, Headers: mailHeaders}
		mailEvent := func(status, message string) {
			// Mail progress is orthogonal to the protocol/input state. In
			// particular, late polling events must not reopen accepted input.
			emit(jobs.JobEvent{JobID: job.ID, Data: map[string]any{"mailStatus": status, "mailApiError": message}, At: time.Now().UTC()})
		}
		mailCutoff := time.Now().Add(-5 * time.Second).UnixMilli()
		if restored != nil {
			if stamp, e := time.Parse(time.RFC3339Nano, restored.UpdatedAt); e == nil {
				mailCutoff = stamp.Add(-5 * time.Second).UnixMilli()
			}
		}
		defer func() {
			if sess != nil {
				if err := browsers.Stop(sess); err != nil {
					taskErr = errors.Join(taskErr, fmt.Errorf("Chromium session cleanup failed: %w", err))
				}
			}
		}()
		input := func(ctx context.Context, stage, prompt string) (string, error) {
			if stage == "email_otp" && mailURL != "" {
				requirements["emailOtp"] = true
				if !mailState.paused {
					emit(jobs.JobEvent{JobID: job.ID, Stage: stage, Status: jobs.StatusWaitingInput, Prompt: "正在自动轮询邮箱验证码", Data: map[string]any{"mailStatus": "polling"}})
					value, err := pollMailInput(ctx, cfg, mailURL, mailRequest, mailCutoff, mailState, func() (string, bool) {
						value, ok := manager.ConsumeInput(job.ID, "email_otp", "emailOtp", "code", "value")
						if ok {
							manualAuth = true
						}
						return value, ok
					}, mailEvent)
					if err == nil {
						return value, nil
					}
					if !errors.Is(err, errMailPollingTimeout) {
						return "", err
					}
				}
				prompt = "自动收码等待超时，请手动输入邮箱验证码，或输入 r 重发"
			}
			manualAuth = true
			switch stage {
			case "email_otp":
				requirements["emailOtp"] = true
			case "totp":
				requirements["mfa"] = true
			case "password":
				requirements["password"] = true
			}
			emit(jobs.JobEvent{JobID: job.ID, Stage: stage, Status: jobs.StatusWaitingInput, Prompt: prompt, At: time.Now().UTC()})
			keys := []string{stage, strings.ReplaceAll(stage, "-", "_"), "code", "value"}
			if stage == "email_otp" {
				keys = append(keys, "emailOtp")
			}
			if stage == "totp" {
				keys = append(keys, "mfa_otp")
			}
			if stage == "sms_otp" {
				keys = append(keys, "phone_otp")
			}
			if stage == "totp_setup" {
				keys = append(keys, "totp_setup_otp")
			}
			for {
				if value, ok := manager.ConsumeInput(job.ID, keys...); ok {
					if stage == "password" {
						password = value
					}
					return value, nil
				}
				select {
				case <-ctx.Done():
					return "", ctx.Err()
				case <-time.After(250 * time.Millisecond):
				}
			}
		}
		// Web authentication always selects Chromium before its first request.
		// Independent refresh/code grants and explicit diagnostic native HTTP
		// retain Go transports; neither may switch identity mid-session.
		callbackOnly := cookieIndependentCallback(restored, op)
		legacyRevalidation := false
		if !cfg.CLI.NativeHTTP && !callbackOnly {
			var err error
			legacyRevalidation, err = checkpointBrowserPolicy(restored)
			if err != nil {
				return err
			}
		}
		var doer protocol.Doer
		resolvedProfile := cfg.TLSProfile
		var fingerprint *transport.FingerprintClient
		jar := transport.NewCookieJar()
		var fpErr error
		probeConfig := cfg
		if op == "refresh" || op == "regenerate" || op == "reauthorize" {
			// A refresh grant depends on the authorization server, not ChatGPT's
			// web home or an unrelated web login checkpoint.
			probeConfig.ChatGPTBase = cfg.AuthBase
		}
		if cfg.CLI.NativeHTTP {
			var native *transport.Client
			if restored != nil || callbackOnly {
				jar = transport.NewCookieJar()
				native, fpErr = transport.NewNativeClient(proxyURL, jar)
			} else {
				native, jar, proxyURL, fpErr = prepareNative(ctx, probeConfig, proxyURL)
			}
			if fpErr != nil {
				return fmt.Errorf("native HTTP transport unavailable: %w", fpErr)
			}
			defer native.Close()
			doer, resolvedProfile = native, transport.NativeProfile
		} else if refreshRequested || callbackOnly {
			if callbackOnly {
				jar = transport.NewCookieJar()
				// This is a cookie-independent PKCE grant, not a restored web
				// identity. Use the configured real profile instead of inheriting
				// a retired checkpoint profile; do not probe or rotate the proxy.
				fingerprint, fpErr = transport.NewFingerprintClient(cfg.TLSProfile, proxyURL, jar)
			} else {
				fingerprint, jar, proxyURL, fpErr = prepareFingerprint(ctx, probeConfig, proxyURL)
			}
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
		if cfg.CLI.NativeHTTP {
			pc.UserAgent = transport.NativeUserAgent
		}
		pc.ConnectionRetries = cfg.SameProxyRetries
		pc.RetryDelay = time.Duration(cfg.SameProxyRetryDelayMS) * time.Millisecond
		if cfg.RequestTimeoutMS > 0 {
			pc.Timeout = time.Duration(cfg.RequestTimeoutMS) * time.Millisecond
		}
		if refreshRequested {
			if e := refreshExistingExportAt(ctx, pc, cfg.AuthBase, cfg.CodexClientID, refreshSource, refreshTarget, job.Email); e == nil {
				completeRefresh("OAuth 导入文件已刷新")
				return nil
			} else if op == "refresh" || ctx.Err() != nil || isLocalRefreshFailure(e) {
				return e
			}
			// A new, explicitly requested reauthorization follows a failed grant.
			// Refresh cookies are never imported into this authentication session.
			op = "relogin"
			if !cfg.CLI.NativeHTTP {
				pc.Jar = transport.NewCookieJar()
			}
		}
		var browserTransport *browser.Transport
		ensureBrowser := func(ctx context.Context, device string) error {
			if sess == nil {
				return errors.New("BROWSER_REQUIRED: this Go-only operation cannot change its authentication identity; start an explicit browser login")
			}
			if device != "" {
				return sess.UpdateDeviceID(ctx, device)
			}
			return nil
		}
		if !cfg.CLI.NativeHTTP && !callbackOnly {
			if !cfg.BrowserEnabled {
				return errors.New("BROWSER_REQUIRED: web authentication requires Chromium from its first request; enable the browser and retry")
			}
			if restored != nil {
				pc.Jar.Merge(restored.Cookies)
			}
			device, err := authenticationDevice(restored, pc.Jar, cfg)
			if err != nil {
				return err
			}
			emit(jobs.JobEvent{JobID: job.ID, Stage: "browser_session_wait", Status: jobs.StatusRunning, Prompt: "正在等待 Chromium 会话；首次认证请求即使用固定的浏览器身份"})
			if restored == nil {
				sess, browserTransport, pc.Jar, proxyURL, err = prepareBrowserAuthentication(ctx, cfg, browsers, proxyURL, device)
			} else {
				sess, err = browsers.StartWait(ctx, proxyURL, "", "chromium-native", device)
				if err == nil {
					identity := sess.Identity()
					if !legacyRevalidation {
						err = protocol.ValidateTransportSession(*restored, "chromium-native", identity.UserAgent, proxyURL)
					}
					if err == nil {
						browserTransport = browser.NewTransport(sess, pc.Jar)
						err = browserTransport.ImportCookies(ctx)
					}
				}
			}
			if err != nil {
				return err
			}
			pc.Transport = browserTransport
			identity := sess.Identity()
			pc.UserAgent, pc.AcceptLanguage, resolvedProfile = identity.UserAgent, identity.Language, "chromium-native"
			emit(jobs.JobEvent{JobID: job.ID, Stage: "browser_bound_transport", Status: jobs.StatusRunning, Prompt: "认证协议已固定到同一 Chromium、代理、Cookie 和设备会话", Data: map[string]any{"tlsProfile": "chromium-native", "browserVersion": identity.BrowserVersion}})
		}
		pc.Challenge = func(ctx context.Context, rawURL string) error {
			if err := ensureBrowser(ctx, ""); err != nil {
				return err
			}
			emit(jobs.JobEvent{JobID: job.ID, Stage: "browser_challenge", Status: jobs.StatusRunning, Prompt: "正在同一浏览器会话中完成安全验证"})
			solveErr := retryBrowserChallenge(ctx, cfg.SameProxyRiskRetries, time.Duration(cfg.SameProxyRiskRetryDelayMS)*time.Millisecond, func() error {
				result, err := (challenge.BrowserSolver{Session: sess, Timeout: 70 * time.Second, OnInteractionRequired: func(prompt string) {
					emit(jobs.JobEvent{JobID: job.ID, Stage: "browser_interaction_required", Status: jobs.StatusRunning, Prompt: prompt})
				}}).SolveContext(ctx, rawURL)
				return validateBrowserReplayReadiness(result, err)
			}, func() {
				emit(jobs.JobEvent{JobID: job.ID, Stage: "browser_challenge_retry", Status: jobs.StatusRunning, Prompt: "使用原代理和原浏览器会话重试安全验证"})
			})
			if solveErr != nil {
				if ctx.Err() == nil {
					emit(browserVerificationFailureEvent(job.ID, solveErr))
				}
				return solveErr
			}
			emit(jobs.JobEvent{JobID: job.ID, Stage: "browser_protocol_replay", Status: jobs.StatusRunning, Prompt: "浏览器页面已就绪，正在原会话中复验受保护的协议请求"})
			return browserTransport.ExportCookies(ctx)
		}
		flow := protocol.NewFlow(pc, protocol.FlowOptions{CheckpointRestoreManaged: true, ChatGPTBase: cfg.ChatGPTBase, AuthBase: cfg.AuthBase, Email: job.Email, Password: loginPassword, TOTPSecret: totpCandidate, Phone: phone, ClientID: cfg.CodexClientID, RedirectURI: cfg.CodexRedirectURI, CheckpointPath: checkpointPath, OutputPath: outputPath, SaveCheckpoint: func(c protocol.Checkpoint) error {
			profile := resolvedProfile
			if sess != nil {
				profile = "chromium-native"
			}
			protocol.BindTransportSession(&c, profile, pc.UserAgent, proxyURL, sess != nil)
			if protocol.HasSavedOAuthGrant(c) {
				// Accepted one-use grants must survive cancellation or a locked vault.
				return protocol.WriteCheckpoint(checkpointPath, c)
			}
			if e := protectCredentials(credentials.Credentials{Password: password, TOTPSecret: totpSecret, ProxyURL: proxyURL, MailAPIURL: mailURL, MailMethod: mailMethod, MailBody: mailBody, MailHeaders: mailHeaders}, false, false); e != nil {
				return fmt.Errorf("cannot protect session credentials: %w", e)
			}
			return protocol.WriteCheckpoint(checkpointPath, c)
		}, Input: input, AuthStep: func(name string, automatic bool) {
			if name == "password" && automatic {
				// The pre-verification checkpoint mirrors saved credentials into
				// Manager.Input. This candidate is already in FlowOptions.Password;
				// consume only its matching copy so rejection waits for new input.
				manager.DiscardInputIfEqual(job.ID, "password", loginPassword)
			}
			requirements[name] = true
			if !automatic {
				manualAuth = true
			}
		}, PasswordVerified: func(ctx context.Context, verified string) error {
			if e := protectCredentials(credentials.Credentials{Password: verified, TOTPSecret: totpSecret, ProxyURL: proxyURL, MailAPIURL: mailURL, MailMethod: mailMethod, MailBody: mailBody, MailHeaders: mailHeaders}, false, true); e != nil {
				return fmt.Errorf("cannot protect verified password: %w", e)
			}
			password = verified
			return nil
		}, TOTPVerified: func(ctx context.Context, secret string) error {
			if !pendingTOTPRecovery || secret != savedCredentials.PendingTOTPSecret {
				return nil
			}
			if e := protectCredentials(credentials.Credentials{Password: password, TOTPSecret: secret, ProxyURL: proxyURL, MailAPIURL: mailURL, MailMethod: mailMethod, MailBody: mailBody, MailHeaders: mailHeaders}, true, false); e != nil {
				return fmt.Errorf("cannot protect verified TOTP seed: %w", e)
			}
			totpSecret, pendingTOTPRecovery = secret, false
			manager.UpdateTOTPSetup(job.ID, "", "", "", true)
			return nil
		}, TOTPEnrollment: func(ctx context.Context, secret, uri string) error {
			manager.UpdateTOTPSetup(job.ID, secret, uri, "", false)
			// Store the enrollment seed before activation can change the account.
			return protectCredentials(credentials.Credentials{Password: password, TOTPSecret: totpSecret, PendingTOTPSecret: secret, PendingTOTPURI: uri, ProxyURL: proxyURL, MailAPIURL: mailURL, MailMethod: mailMethod, MailBody: mailBody, MailHeaders: mailHeaders}, true, false)
		}, BeforeEmailOTP: func(ctx context.Context) error {
			if mailURL == "" {
				return nil
			}
			return mailState.baseline(ctx, mailURL, mailRequest, mailEvent)
		}, EmailOTPRequested: func(requestedAt time.Time) {
			mailCutoff = requestedAt.Add(-5 * time.Second).UnixMilli()
			mailState.requested()
		}, EventData: func(stage, status, prompt string, data map[string]any) {
			// Flow asks before Input chooses automatic mail versus manual stdin.
			// Only Input may publish a waiting prompt: forwarding both events can
			// cancel automated CLI runs at EOF or consume the next manual OTP early.
			if status == "awaiting_input" || status == jobs.StatusWaitingInput {
				return
			}
			emit(jobs.JobEvent{JobID: job.ID, Stage: stage, Status: status, Prompt: prompt, Data: data, At: time.Now().UTC()})
		}, Sentinel: func(ctx context.Context, authBase, device, flowName string) (http.Header, error) {
			if e := ensureBrowser(ctx, device); e != nil {
				return nil, e
			}
			solver := challenge.BrowserSolver{Session: sess, Timeout: 120 * time.Second, OnInteractionRequired: func(prompt string) {
				emit(jobs.JobEvent{JobID: job.ID, Stage: "browser_interaction_required", Status: jobs.StatusRunning, Prompt: prompt})
			}}
			result, e := solver.Sentinel(ctx, authBase+sentinelPagePath(flowName), cfg.SentinelSDKURL, flowName)
			if e != nil {
				if ctx.Err() == nil {
					emit(browserVerificationFailureEvent(job.ID, e))
				}
				return nil, e
			}
			if e = browserTransport.ExportCookies(ctx); e != nil {
				return nil, e
			}
			if result.Provider != "sentinel" || result.Token == "" {
				return nil, fmt.Errorf("SENTINEL_TOKEN_MISSING: browser challenge produced %s, not a Sentinel SDK token", result.Provider)
			}
			h := http.Header{}
			h.Set("Openai-Sentinel-Token", result.Token)
			if result.SessionObserverToken != "" {
				h.Set("OpenAI-Sentinel-SO-Token", result.SessionObserverToken)
			}
			if result.SessionObserverStatus != "" && result.SessionObserverStatus != "available" {
				logger.Warn("Sentinel Session Observer token unavailable", "status", result.SessionObserverStatus)
			}
			return h, nil
		}})
		flow.Options.WorkspaceID, _ = vals["workspaceId"].(string)
		if cfg.CLI.Enabled() {
			flow.Options.OutputMode, _ = vals["outputMode"].(string)
			flow.Options.SessionOutputPath, _ = vals["sessionOutputPath"].(string)
			flow.Options.WebOnly, _ = vals["webOnly"].(bool)
			flow.Options.AccountName, _ = vals["accountName"].(string)
			flow.Options.Concurrency, _ = vals["sub2apiConcurrency"].(int)
			flow.Options.Priority, _ = vals["priority"].(int)
			flow.Options.RateMultiplier, _ = vals["rateMultiplier"].(float64)
		}
		if restored != nil {
			resume := *restored
			if callbackOnly {
				// PKCE/state are validated by Flow before exchange. Never send
				// cookies from another TLS/browser identity with this grant.
				resume.Cookies = []transport.Cookie{}
			} else if legacyRevalidation {
				emit(jobs.JobEvent{JobID: job.ID, Stage: "checkpoint_session_verifying", Status: jobs.StatusRunning, Prompt: "正在 Chromium 中只读复验旧 checkpoint 的已登录账号"})
				if e := revalidateLegacyWebSession(ctx, pc, resume, cfg); e != nil {
					return e
				}
				resume.Cookies = pc.Jar.Cookies()
				protocol.BindTransportSession(&resume, resolvedProfile, pc.UserAgent, proxyURL, true)
			} else {
				if e := protocol.ValidateTransportSession(resume, resolvedProfile, pc.UserAgent, proxyURL); e != nil {
					return e
				}
				if sess != nil {
					resume.Cookies = pc.Jar.Cookies()
				}
			}
			if e := flow.Restore(resume); e != nil {
				return e
			}
		}
		if op == "relogin" {
			if e := protocol.RemoveCheckpoint(checkpointPath); e != nil {
				return e
			}
		}
		if op := strings.ToLower(job.Operation); strings.Contains(op, "totp") || strings.Contains(op, "2fa") || strings.Contains(op, "setup-2fa") {
			if credStore == nil {
				return errors.New("secure credential store unavailable; refusing to start TOTP enrollment")
			}
			resultPath := filepath.Join(cfg.OutputRoot, job.ID, "totp-setup-result.json")
			if cfg.CLI.Enabled() {
				resultPath = cliOperationResultPath(cfg.CLI, true)
			}
			if !durableCredentials {
				resultPath = ""
			}
			result, err := flow.SetupTOTP(ctx, resultPath)
			if err != nil {
				manager.UpdateTOTPSetup(job.ID, "", "", "TOTP 设置失败，已生成的密钥保留在凭据存储", false)
				return err
			}
			if already, _ := result["already_enabled"].(bool); already {
				recoveryHint := ""
				if pendingTOTPRecovery {
					recoveryHint = "2FA 已启用；中断时保留的密钥尚未验证，请重新登录以验证并恢复"
				}
				manager.UpdateTOTPSetup(job.ID, "", "", recoveryHint, true)
				if resultPath != "" {
					_ = os.Remove(resultPath)
				}
				return nil
			}
			secret, _ := result["secret"].(string)
			if secret == "" {
				return errors.New("TOTP setup result missing secret")
			}
			var e error
			uri, _ := result["otpauth_uri"].(string)
			manager.UpdateTOTPSetup(job.ID, secret, uri, "", true)
			if e = protectCredentials(credentials.Credentials{Password: password, TOTPSecret: secret, ProxyURL: proxyURL, MailAPIURL: mailURL, MailMethod: mailMethod, MailBody: mailBody, MailHeaders: mailHeaders}, true, false); e != nil {
				manager.UpdateTOTPSetup(job.ID, secret, uri, e.Error(), false)
				return fmt.Errorf("credential save failed after TOTP setup: %w", e)
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
			resultPath := filepath.Join(cfg.OutputRoot, job.ID, "password-add-result.json")
			if cfg.CLI.Enabled() {
				resultPath = cliOperationResultPath(cfg.CLI, false)
			}
			if !durableCredentials {
				resultPath = ""
			}
			// Keep a newly chosen password in the protected store before the
			// remote mutation. If the response is lost, recovery can retry with
			// the exact same value without putting it in job metadata.
			if e := protectCredentials(credentials.Credentials{Password: password, PendingPassword: pw, TOTPSecret: totpSecret, ProxyURL: proxyURL, MailAPIURL: mailURL, MailMethod: mailMethod, MailBody: mailBody, MailHeaders: mailHeaders}, false, true); e != nil {
				return fmt.Errorf("credential preflight failed: %w", e)
			}
			if e := flow.AddPassword(ctx, pw, resultPath); e != nil {
				manager.UpdatePasswordAdded(job.ID, false, e.Error())
				return e
			}
			if credStore != nil {
				if e := protectCredentials(credentials.Credentials{Password: pw, TOTPSecret: totpSecret, ProxyURL: proxyURL, MailAPIURL: mailURL, MailMethod: mailMethod, MailBody: mailBody, MailHeaders: mailHeaders}, false, true); e != nil {
					return fmt.Errorf("credential save failed after password add: %w", e)
				}
			}
			manager.UpdatePasswordAdded(job.ID, true, "")
			if !cfg.CLI.Enabled() {
				_ = os.Remove(resultPath)
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
			automated := !manualAuth && len(requirements) > 0 && (password != "" || mailURL != "")
			reason := "上次完整登录使用可恢复资料自动完成"
			if !automated {
				reason = "上次授权需要人工输入或没有可恢复登录资料"
			}
			manager.RecordAuthorization(job.ID, automated, reason, requirements)
			manager.UpdateResult(job.ID, len(result.Export.Accounts) > 0, "输出文件已生成")
			_ = result
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
}

func randomStrongPassword() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("secure password generation failed: %w", err)
	}
	return "T2-" + base64.RawURLEncoding.EncodeToString(b) + "!aA9", nil
}

func refreshExistingExport(ctx context.Context, doer protocol.Doer, authBase, clientID, root, id, redirectURI string) error {
	_ = redirectURI
	path := filepath.Join(root, id, "sub2api-import-oauth.json")
	return refreshExistingFile(ctx, doer, authBase, clientID, path, "")
}

// updateRefreshedDocument operates on a validated private copy. The caller
// persists the accepted grant before applying optional metadata enrichment.
func updateRefreshedDocument(raw map[string]any, tok protocol.TokenSet, clientID string) map[string]any {
	account := raw["accounts"].([]any)[0].(map[string]any)
	saved := account["credentials"].(map[string]any)
	oldRefresh, _ := saved["refresh_token"].(string)
	extra, _ := account["extra"].(map[string]any)
	// Once the provider rotates a refresh token, metadata parsing must never
	// discard it. ID tokens can be omitted or use a provider-specific format.
	saved["access_token"] = tok.AccessToken
	if tok.RefreshToken != "" {
		saved["refresh_token"] = tok.RefreshToken
	} else {
		tok.RefreshToken = oldRefresh
	}
	if tok.IDToken != "" {
		saved["id_token"] = tok.IDToken
	} else {
		tok.IDToken, _ = saved["id_token"].(string)
	}
	email, _ := saved["email"].(string)
	if email == "" {
		email, _ = extra["email"].(string)
	}
	if ex, decodeErr := protocol.BuildSub2APIOAuthExport(tok, clientID, email, "", 0, 0, 0); decodeErr == nil {
		updated := ex.Accounts[0]
		if extra == nil {
			extra = map[string]any{}
			account["extra"] = extra
		}
		if accountID := updated.Credentials.ChatGPTAccountID; accountID != "" {
			saved["chatgpt_account_id"] = accountID
			extra["account_id"], extra["chatgpt_account_id"] = accountID, accountID
		}
		if userID, _ := updated.Extra["chatgpt_user_id"].(string); userID != "" {
			extra["chatgpt_user_id"] = userID
		}
		extra["client_id"] = clientID
		if updated.Credentials.Email != "" {
			saved["email"], extra["email"] = updated.Credentials.Email, updated.Credentials.Email
			account["name"] = updated.Name
		}
	}
	raw["type"], raw["version"] = "sub2api-data", 1
	if _, ok := raw["proxies"].([]any); !ok {
		raw["proxies"] = []any{}
	}
	raw["exported_at"] = time.Now().UTC().Format(time.RFC3339Nano)
	return raw
}
