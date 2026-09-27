package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"tosub2-go-port/internal/config"
	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/logging"
	"tosub2-go-port/internal/protocol"
	"tosub2-go-port/internal/store"
)

const cliUsage = `toSub2 Go

Console:
  tosub2 --host 127.0.0.1 --port 4399
Single authorized account:
  tosub2 --email you@example.com --output-mode sub2api
  tosub2 --resume-checkpoint private/login-checkpoint.json
  tosub2 --refresh-sub2api private/sub2api-import-oauth.json
  tosub2 --email you@example.com --setup-totp
  tosub2 --email you@example.com --add-password
Offline macOS credential import (stop the destination service first):
  tosub2 --import-legacy-credentials /legacy/credentials --credential-emails-file emails.txt
  tosub2 --import-legacy-credentials /legacy/credentials --credential-emails-file job-meta.json
Requires TOSUB2_MAC_KEYCHAIN_SERVICE with an isolated local namespace.
Destination uses TOSUB2_MAC_CREDENTIAL_ROOT or --output-root/credentials.
Input: one bare email per line, legacy job metadata, or an email/job JSON list;
UTF-8, maximum 1 MiB and 10000 entries. Existing accounts are not overwritten.
Only aggregate counters are printed; no server, browser or account login starts.

Login secrets use CHATGPT_LOGIN_PASSWORD, CHATGPT_TOTP_SECRET and
CHATGPT_NEW_PASSWORD. Manual OTP/phone input is read from stdin and never logged.
Optional mail automation uses CHATGPT_MAIL_API_URL. Type q to cancel a prompt.

Outputs: --output-mode both|session|sub2api (default both), --out,
--sub2api-out, --checkpoint, --resume-checkpoint, --totp-result,
--password-add-result. Session and checkpoint files are private credentials.
Private defaults: tmp/chatgpt-totp-setup.json and
tmp/chatgpt-password-add-result.json (OS-secure platforms only).
Operations: --web-only, --no-sub2api-export, --setup-totp, --add-password,
--refresh-sub2api. Metadata: --sub2api-name, --concurrency, --priority,
--rate-multiplier, --workspace-id. Input: --email, --phone.
Connectivity: --proxy, --tls-profile, --chatgpt-base, --auth-base,
--codex-client-id, --codex-redirect-uri, --request-timeout-ms.
Diagnostics: --verbose/-v, --debug-auth (both show sanitized stage events).
--native-http explicitly selects Go's standard HTTP/TLS diagnostic transport,
with no Chrome fingerprint. Default web authentication uses the same native
Chromium session from its first request. Independent refresh and callback
code grants use Go tls-client; --tls-profile selects those Go TLS profiles.
Production never runs Node or Python.
`

// runCLI shares the production runner with the console. Its private paths are
// supplied only by trusted process arguments, never by console API input.
func runCLI(ctx context.Context, cfg config.Config, logger *slog.Logger) (bool, error) {
	return runCLIWithIO(ctx, cfg, logger, os.Stdin, os.Stdout)
}
func runCLIWithIO(ctx context.Context, cfg config.Config, logger *slog.Logger, input io.Reader, output io.Writer) (bool, error) {
	return runCLIWithRunnerFactory(ctx, cfg, logger, input, output, makeJobRunner)
}

func runCLIWithRunnerFactory(ctx context.Context, cfg config.Config, logger *slog.Logger, input io.Reader, output io.Writer, factory func(config.Config, *jobs.Manager, *slog.Logger) jobs.Runner) (bool, error) {
	if !cfg.CLI.Enabled() {
		return false, nil
	}
	if cfg.CLI.Help {
		_, err := io.WriteString(output, cliUsage)
		return true, err
	}
	if err := cfg.CLI.Validate(); err != nil {
		return true, err
	}
	if logger == nil {
		logger = logging.NewWithWriter(io.Discard)
	}
	if err := validateCLICredentialDelivery(cfg.CLI, runtime.GOOS, os.Getenv("CHATGPT_NEW_PASSWORD")); err != nil {
		return true, err
	}
	email, err := cliAccountEmail(cfg.CLI)
	if err != nil {
		return true, err
	}
	random := make([]byte, 10)
	if _, err = rand.Read(random); err != nil {
		return true, err
	}
	id := "cli-" + hex.EncodeToString(random)
	jobDir := filepath.Join(cfg.OutputRoot, id)
	st, err := store.New(jobDir)
	if err != nil {
		return true, err
	}
	operation := "login"
	switch {
	case cfg.CLI.SetupTOTP:
		operation = "setup-totp"
	case cfg.CLI.AddPassword:
		operation = "add-password"
	case cfg.CLI.RefreshSub2API != "":
		operation = "refresh"
	}
	taskCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	manager := jobs.NewManager(taskCtx, 1, st, logger)
	manager.SetRunner(factory(cfg, manager, logger))
	events, unsubscribe := manager.Subscribe(64)
	defer unsubscribe()
	values := cliInput(cfg.CLI, jobDir)
	for key, env := range map[string]string{"password": "CHATGPT_LOGIN_PASSWORD", "totpSecret": "CHATGPT_TOTP_SECRET", "newPassword": "CHATGPT_NEW_PASSWORD", "mailApiUrl": "CHATGPT_MAIL_API_URL"} {
		if value := os.Getenv(env); value != "" {
			values[key] = value
		}
	}
	if cfg.CLI.ResumeCheckpoint != "" && cfg.CLI.ResumeCheckpoint != values["checkpointPath"].(string) {
		if err = copyCLIPrivateJSON(cfg.CLI.ResumeCheckpoint, values["checkpointPath"].(string)); err != nil {
			return true, err
		}
	}
	// Refresh uses its stable source directly. Only a successfully refreshed
	// destination may populate the optional job download cache; pre-staging
	// the old source could expose consumed refresh tokens after a cache error.
	job, err := manager.Submit(jobs.Spec{ID: id, Email: email, Operation: operation, Input: values})
	if err != nil {
		return true, err
	}
	manager.Start()
	defer func() {
		timeout := time.Duration(cfg.ShutdownTimeoutMS) * time.Millisecond
		if timeout <= 0 {
			timeout = 10 * time.Second
		}
		shutdownCtx, stop := context.WithTimeout(context.Background(), timeout)
		defer stop()
		_ = manager.Shutdown(shutdownCtx)
	}()
	// Read only while the task requests input. A blocking terminal read must not
	// prevent cancellation of the actual job or its Chromium process.
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 1024), 1<<20)
	// This single-account command owns stdin until it exits. Closing an owned
	// file/pipe unblocks a pending prompt read during signal cancellation.
	if closer, ok := input.(io.ReadCloser); ok {
		defer closer.Close()
	}
	type lineResult struct {
		value string
		err   error
	}
	lines := make(chan lineResult, 1)
	readPending := false
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	waiting := ""
	for {
		current, _ := manager.Get(job.ID)
		if current != nil && current.FinishedAt != nil {
			if err := cliTaskError(cfg.CLI, current); err != nil {
				return true, err
			}
			return true, finishCLIOutput(cfg, jobDir, output)
		}
		select {
		case <-ctx.Done():
			manager.Cancel(id)
			return true, ctx.Err()
		case <-ticker.C:
		case event := <-events:
			if event.JobID != id {
				continue
			}
			if cfg.CLI.Verbose || cfg.CLI.DebugAuth {
				fmt.Fprintf(output, "[%s] %s\n", event.Stage, event.Status)
			}
			if event.Prompt != "" && (event.Status == jobs.StatusWaitingInput || event.Status == "awaiting_input") {
				// The web console permits manual overrides while mail polling is
				// active. CLI stdin belongs to a manual prompt only; reading it
				// now would consume a later password/OTP or abort at EOF.
				if event.Stage == "email_otp" && event.Data["mailStatus"] == "polling" {
					continue
				}
				waiting = event.Stage
				fmt.Fprintln(output, logging.RedactText(event.Prompt))
				if !readPending {
					readPending = true
					go func() {
						r := lineResult{}
						if scanner.Scan() {
							r.value = scanner.Text()
						} else {
							r.err = scanner.Err()
							if r.err == nil {
								r.err = io.EOF
							}
						}
						select {
						case lines <- r:
						case <-taskCtx.Done():
						}
					}()
				}
			}
		case line := <-lines:
			readPending = false
			if line.err != nil {
				manager.Cancel(id)
				return true, errors.New("stdin ended or failed while task required input")
			}
			if waiting != "password" {
				line.value = strings.TrimSpace(line.value)
			}
			if strings.EqualFold(line.value, "q") {
				manager.Cancel(id)
				return true, context.Canceled
			}
			manager.Input(id, map[string]any{waiting: line.value})
			waiting = ""
		}
	}
}

func validateCLICredentialDelivery(c config.CLIConfig, platform, explicitPassword string) error {
	if platform == "darwin" || platform == "windows" {
		return nil
	}
	if c.TOTPResult != "" || c.PasswordAddResult != "" {
		return errors.New("private password/TOTP result exports require OS-protected credentials on this platform; no plaintext fallback")
	}
	if c.SetupTOTP {
		return errors.New("CLI TOTP setup requires OS-protected credential storage so the enrollment seed survives process exit")
	}
	if c.AddPassword && explicitPassword == "" {
		return errors.New("without OS-protected credential storage, CLI add-password requires a user-held CHATGPT_NEW_PASSWORD; a generated password would be lost on exit")
	}
	return nil
}

// Console suboperations deliberately retain the account's OAuth availability
// status. CLI exit codes instead describe whether the requested operation
// succeeded; an existing OAuth export must not hide a suboperation failure.
func cliTaskError(c config.CLIConfig, job *jobs.Job) error {
	if message := logging.RedactText(strings.TrimSpace(job.Error)); message != "" {
		return errors.New(message)
	}
	if job.Status == jobs.StatusCompleted || ((c.SetupTOTP || c.AddPassword) && job.Status == "resume_available") {
		return nil
	}
	return errors.New("CLI task " + job.Status)
}

func cliAccountEmail(c config.CLIConfig) (string, error) {
	email := strings.ToLower(strings.TrimSpace(c.Email))
	if c.ResumeCheckpoint != "" {
		checkpoint, err := protocol.ReadCheckpoint(c.ResumeCheckpoint)
		if err != nil {
			return "", errors.New("cannot read resume checkpoint")
		}
		if email != "" && !strings.EqualFold(checkpoint.Email, email) {
			return "", errors.New("checkpoint email differs from requested account")
		}
		email = checkpoint.Email
	}
	if c.RefreshSub2API != "" {
		data, err := os.ReadFile(c.RefreshSub2API)
		if err != nil {
			return "", errors.New("cannot read OAuth import for refresh")
		}
		source, err := decodeRefreshDocument(data)
		if err != nil {
			return "", errors.New("refresh source must contain a valid first account")
		}
		_, sourceEmail, _, err := refreshSourceIdentity(source, protocol.DefaultCodexClientID)
		if err != nil {
			return "", errors.New("refresh source must contain a valid first account")
		}
		if email != "" && (sourceEmail == "" || !strings.EqualFold(email, sourceEmail)) {
			return "", errors.New("refresh source email differs from requested account")
		}
		if email == "" {
			email = sourceEmail
		}
	}
	if email == "" {
		return "", errors.New("CLI source is missing account email; supply --email")
	}
	return strings.ToLower(strings.TrimSpace(email)), nil
}

// A checkpoint output filename does not authorize restoring its old account
// state. Console recovery is automatic; CLI recovery is always explicit.
func shouldRestoreCheckpoint(cfg config.Config, operation string) bool {
	switch operation {
	case "relogin", "refresh", "regenerate", "reauthorize":
		return false
	}
	if cfg.CLI.Enabled() {
		return cfg.CLI.ResumeCheckpoint != "" && !cfg.CLI.WebOnly
	}
	return true
}

func cliInput(c config.CLIConfig, jobDir string) map[string]any {
	checkpoint := c.Checkpoint
	if checkpoint == "" {
		checkpoint = c.ResumeCheckpoint
	}
	if checkpoint == "" && c.RefreshSub2API != "" {
		checkpoint = filepath.Join(jobDir, "login-checkpoint.json")
	}
	if checkpoint == "" {
		dest := c.Sub2APIOut
		if dest == "" {
			dest = "tmp/sub2api-import-oauth.json"
		}
		checkpoint = dest + ".checkpoint.json"
	}
	session := c.Out
	if session == "" {
		session = "tmp/chatgpt-protocol-session.json"
	}
	mode := c.OutputMode
	if c.NoSub2APIExport {
		mode = "session"
	}
	return map[string]any{
		"phone": c.Phone, "checkpointPath": checkpoint, "outputPath": filepath.Join(jobDir, "sub2api-import-oauth.json"),
		"sessionOutputPath": session, "outputMode": mode, "webOnly": c.WebOnly,
		"accountName": c.AccountName, "workspaceId": c.WorkspaceID,
		"sub2apiConcurrency": c.Concurrency, "priority": c.Priority, "rateMultiplier": c.RateMultiplier,
	}
}

func cliOperationResultPath(c config.CLIConfig, totp bool) string {
	if totp {
		if c.TOTPResult != "" {
			return c.TOTPResult
		}
		return "tmp/chatgpt-totp-setup.json"
	}
	if c.PasswordAddResult != "" {
		return c.PasswordAddResult
	}
	return "tmp/chatgpt-password-add-result.json"
}

func copyCLIPrivateJSON(source, dest string) error {
	data, err := os.ReadFile(source)
	if err != nil {
		return errors.New("cannot read private CLI input file")
	}
	var value any
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.UseNumber()
	if decoder.Decode(&value) != nil {
		return errors.New("invalid private CLI JSON file")
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return errors.New("invalid private CLI JSON file")
	}
	return protocol.WritePrivateJSON(dest, value)
}

func cliRefreshPaths(c config.CLIConfig) (source, target string, err error) {
	if c.RefreshSub2API == "" {
		return "", "", errors.New("CLI refresh source is required")
	}
	source, err = filepath.Abs(c.RefreshSub2API)
	if err != nil {
		return "", "", errors.New("CLI refresh source path is invalid")
	}
	target = c.Sub2APIOut
	if target == "" {
		target = c.RefreshSub2API
	}
	target, err = filepath.Abs(target)
	if err != nil {
		return "", "", errors.New("CLI refresh output path is invalid")
	}
	return source, target, nil
}

func finishCLIOutput(cfg config.Config, dir string, out io.Writer) error {
	if cfg.CLI.SetupTOTP || cfg.CLI.AddPassword {
		// The shared runner owns the private result schema and secure-store
		// commit. Do not reconstruct enrollment results from pre-existing keys.
		_, err := fmt.Fprintln(out, "Account operation completed.")
		return err
	}
	if cfg.CLI.RefreshSub2API != "" {
		_, target, err := cliRefreshPaths(cfg.CLI)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(target)
		if err != nil {
			return errors.New("completed refresh has no private OAuth output")
		}
		if _, err := decodeRefreshDocument(data); err != nil {
			return errors.New("completed refresh has invalid private OAuth output")
		}
		_, err = fmt.Fprintln(out, "OAuth import saved.")
		return err
	}
	if cfg.CLI.RefreshSub2API == "" && (cfg.CLI.OutputMode == "session" || cfg.CLI.NoSub2APIExport || cfg.CLI.WebOnly) {
		path := cfg.CLI.Out
		if path == "" {
			path = "tmp/chatgpt-protocol-session.json"
		}
		if _, err := os.Stat(path); err != nil {
			return errors.New("completed task has no private session output")
		}
		_, err := fmt.Fprintln(out, "Private session saved.")
		return err
	}
	source := filepath.Join(dir, "sub2api-import-oauth.json")
	target := cfg.CLI.Sub2APIOut
	if target == "" {
		target = cfg.CLI.RefreshSub2API
	}
	if target == "" {
		target = "tmp/sub2api-import-oauth.json"
	}
	if err := copyCLIPrivateJSON(source, target); err != nil {
		return err
	}
	_, err := fmt.Fprintln(out, "OAuth import saved.")
	return err
}
