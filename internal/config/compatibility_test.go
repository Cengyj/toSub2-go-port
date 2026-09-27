package config

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestLegacyCLIFlags(t *testing.T) {
	cfg, err := Parse([]string{"--email", "owner@example.test", "--phone=+8613800000000", "--out=session.json", "--sub2api-out=oauth.json", "--checkpoint=checkpoint.json", "--output-mode=both", "--sub2api-name=Personal", "--concurrency=3", "--priority=0", "--rate-multiplier=0.5", "--workspace-id=workspace", "--verbose", "--debug-auth"})
	if err != nil {
		t.Fatal(err)
	}
	c := cfg.CLI
	if !c.Enabled() || c.Email != "owner@example.test" || c.Phone != "+8613800000000" || c.Out != "session.json" || c.Sub2APIOut != "oauth.json" || c.Checkpoint != "checkpoint.json" || c.OutputMode != "both" || c.AccountName != "Personal" || c.Concurrency != 3 || c.Priority != 0 || c.RateMultiplier != 0.5 || c.WorkspaceID != "workspace" || !c.Verbose || !c.DebugAuth {
		t.Fatalf("flags were not preserved: %+v", c)
	}
	for _, flag := range []string{"--web-only", "--no-sub2api-export", "--output-mode=session", "--output-mode=sub2api", "--setup-totp", "--add-password", "--native-http"} {
		if _, err = Parse([]string{"--email=owner@example.test", flag}); err != nil {
			t.Fatalf("%s: %v", flag, err)
		}
	}
	if _, err = Parse([]string{"--resume-checkpoint=private.json"}); err != nil {
		t.Fatal(err)
	}
	if _, err = Parse([]string{"--refresh-sub2api=oauth.json"}); err != nil {
		t.Fatal(err)
	}
}

func TestLegacyCLIRejectsMissingRequestedOAuthExport(t *testing.T) {
	// The original protocol-login.mjs rejects sub2api-only output if either
	// option suppresses OAuth export. A session file must not mean success.
	for _, option := range []string{"--web-only", "--no-sub2api-export"} {
		t.Run(option, func(t *testing.T) {
			if _, err := Parse([]string{"--email=owner@example.test", "--output-mode=sub2api", option}); err == nil {
				t.Fatal("sub2api-only request accepted without an OAuth export")
			}
			for _, mode := range []string{"both", "session"} {
				if _, err := Parse([]string{"--email=owner@example.test", "--output-mode=" + mode, option}); err != nil {
					t.Fatalf("legacy session output %s rejected: %v", mode, err)
				}
			}
			// Legacy suboperations exit before the login-output branch. These
			// unrelated flags must not interfere with enrollment or refresh.
			for _, operation := range []string{"--setup-totp", "--add-password", "--refresh-sub2api=fixture.json"} {
				if _, err := Parse([]string{"--email=owner@example.test", operation, "--output-mode=sub2api", option}); err != nil {
					t.Fatalf("legacy suboperation rejected: %v", err)
				}
			}
		})
	}
}

func TestCLIOptionsNeverSilentlyStartConsole(t *testing.T) {
	for _, args := range [][]string{{"--phone=123"}, {"--checkpoint=private.json"}, {"--out=private.json"}, {"--verbose"}, {"--output-mode=session"}, {"--setup-totp"}, {"--email=not an email"}, {"--email=owner@example.test", "--setup-totp", "--add-password"}, {"--native-http"}, {"--email=owner@example.test", "--rate-multiplier=NaN"}, {"--email=owner@example.test", "--rate-multiplier=Inf"}, {"--email=owner@example.test", "--totp-result=secret.json"}, {"--email=owner@example.test", "--password-add-result=secret.json"}} {
		if _, err := Parse(args); err == nil {
			t.Fatalf("unexpected success: %v", args)
		}
	}
	for _, flag := range []string{"--help", "-h"} {
		cfg, err := Parse([]string{flag})
		if err != nil || !cfg.CLI.Help || !cfg.CLI.Enabled() {
			t.Fatalf("help: %v", err)
		}
	}
	cfg, err := Parse(nil)
	if err != nil || cfg.CLI.Enabled() {
		t.Fatalf("console mode: %v", err)
	}
	if cfg.CLI.OutputMode != "both" {
		t.Fatal("legacy default output mode changed")
	}
}
func TestConfigurationErrorDoesNotEchoSecretArgument(t *testing.T) {
	for _, args := range [][]string{{"--unknown=synthetic-secret"}, {"--port=synthetic-secret"}, {"--proxy=http://user:synthetic-secret@"}, {"--auth-base=https://user:synthetic-secret@example.test"}, {"synthetic-secret"}} {
		_, err := Parse(args)
		if err == nil || strings.Contains(err.Error(), "synthetic-secret") {
			t.Fatalf("unsafe or missing parse error: %v", err)
		}
	}
}
func TestEnvironmentProxyPoolAndLegacyPolling(t *testing.T) {
	t.Setenv("CHATGPT_PROXY_URLS", "http://one.example:8080, socks5h://two.example:1080\nhttp://one.example:8080\r\n https://three.example:443")
	t.Setenv("CHATGPT_PROXY_MAX_ATTEMPTS", "4")
	t.Setenv("CHATGPT_SAME_PROXY_CONNECTION_RETRIES", "0")
	t.Setenv("CHATGPT_SAME_PROXY_CONNECTION_RETRY_DELAY_MS", "")
	t.Setenv("CHATGPT_SAME_PROXY_RISK_RETRY_DELAY_MS", "50")
	t.Setenv("TOSUB2_REQUEST_TIMEOUT_MS", "750")
	t.Setenv("SMS_POLL_INTERVAL_MS", "")
	t.Setenv("SMS_POLL_TIMEOUT_MS", "")
	t.Setenv("LUBAN_SMS_POLL_INTERVAL_MS", "200")
	t.Setenv("LUBAN_SMS_POLL_TIMEOUT_MS", "900")
	cfg, err := Parse(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg.ProxyURLs, []string{"http://one.example:8080", "socks5h://two.example:1080", "https://three.example:443"}) {
		t.Fatalf("pool=%v", cfg.ProxyURLs)
	}
	if cfg.ProxyMaxAttempts != 4 || cfg.SameProxyRetries != 0 || cfg.SameProxyRetryDelayMS != 1000 || cfg.SameProxyRiskRetryDelayMS != 50 || cfg.SameProxyRiskRetries != 3 || cfg.RequestTimeoutMS != 750 || cfg.SMSPollIntervalMS != 200 || cfg.SMSPollTimeoutMS != 900 {
		t.Fatalf("unexpected configuration: %+v", cfg)
	}
	cfg, err = Parse([]string{"--host=::1", "--request-timeout-ms=1234", "--connection-retries=2", "--proxy-max-attempts=3"})
	if err != nil || cfg.Addr() != "[::1]:4399" || cfg.RequestTimeoutMS != 1234 || cfg.SameProxyRetries != 2 || cfg.ProxyMaxAttempts != 3 {
		t.Fatalf("overrides: %+v err=%v", cfg, err)
	}
}
func TestSecretsExcludedFromConfigurationJSON(t *testing.T) {
	cfg := Default()
	cfg.ProxyURL = "http://user:secret-password@example.test"
	cfg.ProxyURLs = []string{cfg.ProxyURL}
	cfg.CLI.Email = "private-email@example.test"
	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "secret-password") || strings.Contains(string(b), "private-email") {
		t.Fatal("process-only private configuration was serialized")
	}
}
func TestConnectionValidation(t *testing.T) {
	for _, args := range [][]string{{"--proxy-max-attempts=-1"}, {"--connection-retries=-1"}, {"--connection-retries=21"}, {"--request-timeout-ms=0"}, {"--proxy=ftp://example.test"}, {"--chatgpt-base=file:///private"}} {
		if _, err := Parse(args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	for _, phone := range []string{"13800000000", "+0123456789", "+1234", "+1234567890123456", "+86 13800000000"} {
		if _, err := Parse([]string{"--email=owner@example.test", "--phone=" + phone}); err == nil {
			t.Fatal("invalid E.164 phone accepted")
		}
	}
	t.Setenv("MAIL_POLL_TIMEOUT_MS", "-10")
	if _, err := Parse(nil); err == nil {
		t.Fatal("negative polling timeout accepted")
	}
}
