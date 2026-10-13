package config

import (
	"errors"
	"flag"
	"math"
	"net/mail"
	"regexp"
	"strings"
)

// CLIConfig contains process-local protocol CLI options. Login secrets continue
// to use CHATGPT_LOGIN_PASSWORD/CHATGPT_TOTP_SECRET, never argv or metadata.
type CLIConfig struct {
	Email, Phone                                                                           string
	AuthorizationMethod                                                                    string
	OutputMode, Out, Sub2APIOut, RefreshSub2API, Checkpoint, ResumeCheckpoint              string
	TOTPResult, PasswordAddResult, AccountName, WorkspaceID                                string
	Concurrency, Priority                                                                  int
	RateMultiplier                                                                         float64
	Help, Verbose, DebugAuth, WebOnly, SetupTOTP, AddPassword, NoSub2APIExport, NativeHTTP bool
	Requested                                                                              bool
}

func (c CLIConfig) Enabled() bool {
	return c.Requested || c.Help || c.Email != "" || c.ResumeCheckpoint != "" || c.RefreshSub2API != "" || c.SetupTOTP || c.AddPassword
}
func registerCLIFlags(fs *flag.FlagSet, c *CLIConfig) {
	fs.StringVar(&c.Email, "email", "", "run a single authorized account in terminal")
	fs.StringVar(&c.AuthorizationMethod, "authorization-method", "", "OAuth authorization: chatgpt (default) or bps; resume/refresh inherit saved method")
	fs.StringVar(&c.Phone, "phone", "", "manual E.164 phone number")
	fs.StringVar(&c.OutputMode, "output-mode", "both", "CLI output mode: both, session, or sub2api")
	fs.StringVar(&c.Out, "out", "", "private session output; defaults to tmp/chatgpt-protocol-session.json")
	fs.StringVar(&c.Sub2APIOut, "sub2api-out", "", "OAuth import output; defaults to tmp/sub2api-import-oauth.json")
	fs.StringVar(&c.RefreshSub2API, "refresh-sub2api", "", "refresh an existing OAuth import")
	fs.StringVar(&c.Checkpoint, "checkpoint", "", "save a private resumable login checkpoint")
	fs.StringVar(&c.ResumeCheckpoint, "resume-checkpoint", "", "resume a v1 login checkpoint")
	fs.StringVar(&c.TOTPResult, "totp-result", "", "optional private TOTP result output on OS-secure hosts")
	fs.StringVar(&c.PasswordAddResult, "password-add-result", "", "optional private added-password output on OS-secure hosts")
	fs.StringVar(&c.AccountName, "sub2api-name", "", "Sub2API account name")
	fs.StringVar(&c.WorkspaceID, "workspace-id", "", "select an authorized workspace")
	fs.IntVar(&c.Concurrency, "concurrency", 10, "Sub2API concurrency")
	fs.IntVar(&c.Priority, "priority", 1, "Sub2API priority")
	fs.Float64Var(&c.RateMultiplier, "rate-multiplier", 1, "Sub2API rate multiplier")
	fs.BoolVar(&c.Help, "help", false, "show usage")
	fs.BoolVar(&c.Help, "h", false, "show usage")
	fs.BoolVar(&c.Verbose, "verbose", false, "show sanitized stage events")
	fs.BoolVar(&c.Verbose, "v", false, "show sanitized stage events")
	fs.BoolVar(&c.DebugAuth, "debug-auth", false, "show sanitized auth stage events")
	fs.BoolVar(&c.WebOnly, "web-only", false, "complete ChatGPT web login without Codex OAuth")
	fs.BoolVar(&c.NoSub2APIExport, "no-sub2api-export", false, "save session without exchanging the OAuth code")
	fs.BoolVar(&c.SetupTOTP, "setup-totp", false, "set up TOTP on an authorized account")
	fs.BoolVar(&c.AddPassword, "add-password", false, "add an explicit or generated strong password")
	fs.BoolVar(&c.NativeHTTP, "native-http", false, "explicit standard Go HTTP transport (no Chrome TLS fingerprint)")
}
func (c CLIConfig) Validate() error {
	if c.Help {
		return nil
	}
	switch strings.ToLower(strings.TrimSpace(c.AuthorizationMethod)) {
	case "", "chatgpt":
	case "bps":
		if c.WebOnly || c.SetupTOTP || c.AddPassword {
			return errors.New("BPS cannot be combined with --web-only, --setup-totp, or --add-password")
		}
	default:
		return errors.New("authorization-method must be chatgpt or bps")
	}
	if c.Email != "" {
		a, e := mail.ParseAddress(strings.TrimSpace(c.Email))
		if e != nil || a.Address != strings.TrimSpace(c.Email) {
			return errors.New("invalid CLI email address")
		}
	}
	if c.Phone != "" && !regexp.MustCompile(`^\+[1-9][0-9]{6,14}$`).MatchString(c.Phone) {
		return errors.New("CLI phone must be an E.164 number, for example +8613800000000")
	}
	modes := 0
	for _, b := range []bool{c.SetupTOTP, c.AddPassword, c.RefreshSub2API != ""} {
		if b {
			modes++
		}
	}
	if modes > 1 {
		return errors.New("choose only one CLI operation: setup-totp, add-password, or refresh-sub2api")
	}
	if c.RefreshSub2API != "" && c.ResumeCheckpoint != "" {
		return errors.New("--refresh-sub2api cannot be combined with --resume-checkpoint")
	}
	if c.OutputMode != "sub2api" && c.OutputMode != "both" && c.OutputMode != "session" {
		return errors.New("output-mode must be sub2api, session, or both")
	}
	if modes == 0 && c.OutputMode == "sub2api" && (c.WebOnly || c.NoSub2APIExport) {
		return errors.New("sub2api-only output requires Codex OAuth export; remove --web-only and --no-sub2api-export or choose --output-mode=session")
	}
	if c.Enabled() && c.Email == "" && c.ResumeCheckpoint == "" && c.RefreshSub2API == "" {
		return errors.New("CLI operation requires --email or --resume-checkpoint")
	}
	if c.Concurrency < 1 || c.Concurrency > 10000 || c.Priority < 0 || c.Priority > 10000 || c.RateMultiplier <= 0 || math.IsNaN(c.RateMultiplier) || math.IsInf(c.RateMultiplier, 0) {
		return errors.New("invalid Sub2API concurrency, priority, or rate multiplier")
	}
	if c.TOTPResult != "" && !c.SetupTOTP {
		return errors.New("--totp-result requires --setup-totp")
	}
	if c.PasswordAddResult != "" && !c.AddPassword {
		return errors.New("--password-add-result requires --add-password")
	}
	return nil
}

// MarkRequested distinguishes omitted default flags from an explicit command
// with missing account information. A CLI-only option must never start the
// console silently.
func (c *CLIConfig) markRequested(fs *flag.FlagSet) {
	cliNames := map[string]bool{}
	probe := flag.NewFlagSet("cli-options", flag.ContinueOnError)
	var unused CLIConfig
	registerCLIFlags(probe, &unused)
	probe.VisitAll(func(f *flag.Flag) { cliNames[f.Name] = true })
	fs.Visit(func(f *flag.Flag) {
		if cliNames[f.Name] {
			c.Requested = true
		}
	})
}
