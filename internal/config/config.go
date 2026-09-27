package config

import (
	"flag"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	DefaultHost          = "127.0.0.1"
	DefaultPort          = 4399
	DefaultMaxActiveJobs = 20
)

// Config contains process-level configuration. Secrets are intentionally not
// represented here; credentials are loaded through the credential store.
type Config struct {
	Host                       string
	Port                       int
	OutputRoot                 string
	MaxActiveJobs              int
	StartupToken               string `json:"-"`
	TLSProfile                 string
	ChatGPTBase                string
	AuthBase                   string
	ProxyURL                   string   `json:"-"`
	ProxyURLs                  []string `json:"-"`
	ProxyMaxAttempts           int
	ProxyMaxAttemptsSet        bool
	ProxyConnectionRetryBaseMS int
	SameProxyRetries           int
	SameProxyRetryDelayMS      int
	SameProxyRetryDelaySet     bool
	SameProxyRiskRetries       int
	SameProxyRiskRetryDelayMS  int
	RequestTimeoutMS           int
	CLI                        CLIConfig `json:"-"`
	CodexClientID              string
	CodexRedirectURI           string
	MailPollIntervalMS         int
	MailPollTimeoutMS          int
	SMSPollIntervalMS          int
	SMSPollTimeoutMS           int
	SMSPollIntervalSet         bool
	SMSPollTimeoutSet          bool
	BrowserEnabled             bool
	ChromePath                 string
	SentinelSDKURL             string
	ShutdownTimeoutMS          int
	WindowsService             bool
	ImportLegacyCredentials    string `json:"-"`
	CredentialEmailsFile       string `json:"-"`
	CredentialImportRequested  bool   `json:"-"`
}

func Default() Config {
	root := strings.TrimSpace(os.Getenv("ONBOARDING_OUTPUT_ROOT"))
	if root == "" {
		root = filepath.Join("tmp", "chatgpt-onboarding-console")
	}
	proxyMax, proxyMaxSet := envNonnegativeIntSetting("CHATGPT_PROXY_MAX_ATTEMPTS", 10)
	connectionBase := envIntIncludingZero("PROXY_CONNECTION_RETRY_BASE_MS", 1000)
	if connectionBase < 1 {
		connectionBase = 1
	}
	connectionDelay, connectionDelaySet := envNonnegativeIntSetting("CHATGPT_SAME_PROXY_CONNECTION_RETRY_DELAY_MS", connectionBase)
	smsInterval, smsIntervalSet := legacySMSSetting("SMS_POLL_INTERVAL_MS", "LUBAN_SMS_POLL_INTERVAL_MS", 3000)
	smsTimeout, smsTimeoutSet := legacySMSSetting("SMS_POLL_TIMEOUT_MS", "LUBAN_SMS_POLL_TIMEOUT_MS", 600000)
	return Config{
		Host:                       envString("ONBOARDING_HOST", DefaultHost),
		Port:                       envInt("ONBOARDING_PORT", DefaultPort),
		OutputRoot:                 root,
		MaxActiveJobs:              envInt("TOSUB2_MAX_ACTIVE_JOBS", DefaultMaxActiveJobs),
		StartupToken:               strings.TrimSpace(os.Getenv("TOSUB2_CONSOLE_TOKEN")),
		TLSProfile:                 envString("TOSUB2_TLS_PROFILE", "auto"),
		ChatGPTBase:                envString("CHATGPT_BASE", "https://chatgpt.com"),
		AuthBase:                   envString("AUTH_BASE", "https://auth.openai.com"),
		ProxyURL:                   strings.TrimSpace(os.Getenv("CHATGPT_PROXY_URL")),
		ProxyURLs:                  parseProxyPool(os.Getenv("CHATGPT_PROXY_URLS")),
		ProxyMaxAttempts:           proxyMax,
		ProxyMaxAttemptsSet:        proxyMaxSet,
		ProxyConnectionRetryBaseMS: connectionBase,
		SameProxyRetries:           envNonnegativeInt("CHATGPT_SAME_PROXY_CONNECTION_RETRIES", 3),
		SameProxyRetryDelayMS:      connectionDelay,
		SameProxyRetryDelaySet:     connectionDelaySet,
		SameProxyRiskRetries:       envNonnegativeInt("CHATGPT_SAME_PROXY_RISK_RETRIES", 3),
		SameProxyRiskRetryDelayMS:  envNonnegativeInt("CHATGPT_SAME_PROXY_RISK_RETRY_DELAY_MS", 1000),
		RequestTimeoutMS:           envInt("TOSUB2_REQUEST_TIMEOUT_MS", 30000),
		CodexClientID:              envString("CODEX_CLIENT_ID", "app_EMoamEEZ73f0CkXaXp7hrann"),
		CodexRedirectURI:           envString("CODEX_REDIRECT_URI", "http://localhost:1455/auth/callback"),
		MailPollIntervalMS:         envInt("MAIL_POLL_INTERVAL_MS", 2500),
		MailPollTimeoutMS:          envInt("MAIL_POLL_TIMEOUT_MS", 600000),
		SMSPollIntervalMS:          smsInterval,
		SMSPollTimeoutMS:           smsTimeout,
		SMSPollIntervalSet:         smsIntervalSet,
		SMSPollTimeoutSet:          smsTimeoutSet,
		BrowserEnabled:             envBool("TOSUB2_BROWSER_ENABLED", true),
		ChromePath:                 strings.TrimSpace(os.Getenv("CHROME_PATH")),
		SentinelSDKURL:             envString("TOSUB2_SENTINEL_SDK_URL", "https://sentinel.openai.com/backend-api/sentinel/sdk.js"),
		ShutdownTimeoutMS:          envInt("TOSUB2_SHUTDOWN_TIMEOUT_MS", 10000),
	}
}

// Parse reads command-line flags and environment variables. Existing Node
// deployments commonly pass --host/--port, so those names remain supported.
func Parse(args []string) (Config, error) {
	cfg := Default()
	fs := flag.NewFlagSet("tosub2", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&cfg.Host, "host", cfg.Host, "HTTP listen host")
	fs.IntVar(&cfg.Port, "port", cfg.Port, "HTTP listen port")
	fs.StringVar(&cfg.OutputRoot, "output-root", cfg.OutputRoot, "persistent output directory")
	fs.IntVar(&cfg.MaxActiveJobs, "max-active-jobs", cfg.MaxActiveJobs, "maximum concurrently running jobs")
	fs.StringVar(&cfg.StartupToken, "console-token", cfg.StartupToken, "optional API startup token")
	fs.StringVar(&cfg.TLSProfile, "tls-profile", cfg.TLSProfile, "TLS client profile")
	fs.StringVar(&cfg.ChatGPTBase, "chatgpt-base", cfg.ChatGPTBase, "ChatGPT base URL")
	fs.StringVar(&cfg.AuthBase, "auth-base", cfg.AuthBase, "OpenAI auth base URL")
	fs.StringVar(&cfg.ProxyURL, "proxy", cfg.ProxyURL, "HTTP/HTTPS/SOCKS5 proxy URL")
	fs.StringVar(&cfg.CodexClientID, "codex-client-id", cfg.CodexClientID, "Codex OAuth client ID")
	fs.StringVar(&cfg.CodexRedirectURI, "codex-redirect-uri", cfg.CodexRedirectURI, "Codex OAuth redirect URI")
	fs.BoolVar(&cfg.BrowserEnabled, "browser", cfg.BrowserEnabled, "enable Chromium browser sessions")
	fs.StringVar(&cfg.ChromePath, "chrome-path", cfg.ChromePath, "Chromium executable")
	fs.StringVar(&cfg.SentinelSDKURL, "sentinel-sdk-url", cfg.SentinelSDKURL, "trusted Sentinel SDK loader (HTTPS or local test server)")
	fs.BoolVar(&cfg.WindowsService, "windows-service", false, "run under Windows Service Control Manager")
	fs.StringVar(&cfg.ImportLegacyCredentials, "import-legacy-credentials", "", "offline import from an explicitly selected legacy macOS credential directory")
	fs.StringVar(&cfg.CredentialEmailsFile, "credential-emails-file", "", "authorized emails as a legacy job-meta JSON file or one email per line")
	registerCLIFlags(fs, &cfg.CLI)
	fs.IntVar(&cfg.ProxyMaxAttempts, "proxy-max-attempts", cfg.ProxyMaxAttempts, "maximum proxy connection attempts")
	fs.IntVar(&cfg.SameProxyRetries, "connection-retries", cfg.SameProxyRetries, "same proxy connection retries")
	fs.IntVar(&cfg.RequestTimeoutMS, "request-timeout-ms", cfg.RequestTimeoutMS, "request timeout in milliseconds")
	if err := fs.Parse(args); err != nil {
		return Config{}, fmt.Errorf("invalid command-line arguments (use --help)")
	}
	if fs.NArg() != 0 {
		return Config{}, fmt.Errorf("unexpected positional arguments (use --email for a single account)")
	}
	cfg.CLI.markRequested(fs)
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "proxy-max-attempts" {
			cfg.ProxyMaxAttemptsSet = true
		}
		if f.Name == "import-legacy-credentials" || f.Name == "credential-emails-file" {
			cfg.CredentialImportRequested = true
		}
	})
	if cfg.CLI.Help {
		cfg.WindowsService = false
		return cfg, nil
	}
	if cfg.CredentialImportEnabled() {
		if err := cfg.ValidateCredentialImport(); err != nil {
			return Config{}, err
		}
		// This command never listens or contacts an account service. Unrelated
		// deployment/network defaults must not force it into normal startup.
		return cfg, nil
	}
	if err := cfg.CLI.Validate(); err != nil {
		return Config{}, err
	}
	if cfg.WindowsService && cfg.CLI.Enabled() {
		return Config{}, fmt.Errorf("Windows service mode cannot run an interactive account CLI operation")
	}
	if cfg.Host == "" || strings.HasPrefix(cfg.Host, "-") {
		return Config{}, fmt.Errorf("host must be a valid hostname or IP address")
	}
	if cfg.Port < 1 || cfg.Port > 65535 {
		return Config{}, fmt.Errorf("port must be between 1 and 65535")
	}
	if cfg.MaxActiveJobs < 1 || cfg.MaxActiveJobs > 20 {
		return Config{}, fmt.Errorf("max-active-jobs must be between 1 and 20")
	}
	if cfg.OutputRoot == "" {
		return Config{}, fmt.Errorf("output-root cannot be empty")
	}
	if cfg.ShutdownTimeoutMS < 1 {
		cfg.ShutdownTimeoutMS = 10000
	}
	if cfg.ProxyMaxAttempts < 0 {
		return Config{}, fmt.Errorf("proxy-max-attempts must be nonnegative")
	}
	if cfg.SameProxyRetries < 0 || cfg.SameProxyRetries > 20 {
		return Config{}, fmt.Errorf("connection-retries must be between 0 and 20")
	}
	if cfg.SameProxyRetryDelayMS < 0 || cfg.SameProxyRiskRetryDelayMS < 0 || cfg.SameProxyRiskRetries < 0 || cfg.SameProxyRiskRetries > 20 || cfg.RequestTimeoutMS < 1 {
		return Config{}, fmt.Errorf("invalid connection retry delay or request timeout")
	}
	if cfg.MailPollIntervalMS < 1 || cfg.MailPollTimeoutMS < 1 {
		return Config{}, fmt.Errorf("mail polling intervals and timeouts must be positive")
	}
	for _, raw := range append(append([]string(nil), cfg.ProxyURLs...), cfg.ProxyURL) {
		if raw == "" {
			continue
		}
		u, e := url.Parse(raw)
		if e != nil || u.Hostname() == "" {
			return Config{}, fmt.Errorf("invalid proxy URL")
		}
		switch u.Scheme {
		case "http", "https", "socks5", "socks5h":
		default:
			return Config{}, fmt.Errorf("unsupported proxy scheme")
		}
	}
	for _, raw := range []string{cfg.ChatGPTBase, cfg.AuthBase} {
		u, e := url.Parse(raw)
		if e != nil || u.Hostname() == "" || u.User != nil || (u.Scheme != "https" && u.Scheme != "http") {
			return Config{}, fmt.Errorf("auth and ChatGPT base URLs must be HTTP or HTTPS without credentials")
		}
	}
	if err := validateSDKURL(cfg.SentinelSDKURL); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func validateSDKURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
		return fmt.Errorf("Sentinel SDK loader must be an HTTPS URL without credentials or fragment")
	}
	if u.Scheme == "https" {
		return nil
	}
	ip := net.ParseIP(u.Hostname())
	if u.Scheme == "http" && (strings.EqualFold(u.Hostname(), "localhost") || (ip != nil && ip.IsLoopback())) {
		return nil
	}
	return fmt.Errorf("Sentinel SDK loader requires HTTPS; HTTP is only allowed for loopback fixtures")
}

func (c Config) Addr() string { return net.JoinHostPort(c.Host, strconv.Itoa(c.Port)) }

func envString(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}
func envInt(name string, fallback int) int {
	value, err := strconv.Atoi(strings.TrimSpace(os.Getenv(name)))
	if err != nil || value == 0 {
		return fallback
	}
	return value
}
func envBool(name string, fallback bool) bool {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func envNonnegativeInt(name string, fallback int) int {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	n, e := strconv.Atoi(raw)
	if e != nil || n < 0 {
		return fallback
	}
	return n
}
func parseProxyPool(raw string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, part := range strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == '\n' || r == '\r' }) {
		v := strings.TrimSpace(part)
		if v != "" && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

// Explicit settings distinguish an intentional zero from an uninitialized
// Config literal. Legacy SMS primary variables override aliases even at zero.
func envNonnegativeIntSetting(name string, fallback int) (int, bool) {
	raw := strings.TrimSpace(os.Getenv(name))
	n, err := strconv.Atoi(raw)
	if raw == "" || err != nil || n < 0 {
		return fallback, false
	}
	return n, true
}
func envIntIncludingZero(name string, fallback int) int {
	n, err := strconv.Atoi(strings.TrimSpace(os.Getenv(name)))
	if err != nil {
		return fallback
	}
	return n
}
func legacySMSSetting(primary, alias string, fallback int) (int, bool) {
	for _, name := range []string{primary, alias} {
		raw := os.Getenv(name)
		if raw == "" {
			continue
		}
		// JavaScript Number(" ") is zero, and negative delays/totals are
		// respectively a yielded immediate poll and an immediate timeout.
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" {
			return 0, true
		}
		n, err := strconv.Atoi(trimmed)
		if err != nil {
			return fallback, false
		}
		return n, true
	}
	return fallback, false
}
