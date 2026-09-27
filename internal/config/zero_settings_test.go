package config

import "testing"

func TestExplicitZeroProxyAttemptBudgetAndUninitializedConfiguration(t *testing.T) {
	t.Setenv("CHATGPT_PROXY_MAX_ATTEMPTS", "")
	cfg, err := Parse(nil)
	if err != nil || cfg.ProxyMaxAttempts != 10 || cfg.ProxyMaxAttemptsSet {
		t.Fatal("unset proxy budget lost default", err)
	}
	t.Setenv("CHATGPT_PROXY_MAX_ATTEMPTS", "0")
	cfg, err = Parse(nil)
	if err != nil || cfg.ProxyMaxAttempts != 0 || !cfg.ProxyMaxAttemptsSet {
		t.Fatal("explicit zero proxy budget was replaced", err)
	}
	for _, value := range []string{"0", "101"} {
		cfg, err = Parse([]string{"--proxy-max-attempts=" + value})
		if err != nil || !cfg.ProxyMaxAttemptsSet {
			t.Fatal("legacy nonnegative proxy budget rejected", err)
		}
	}
	for _, value := range []string{"-1", "invalid"} {
		t.Setenv("CHATGPT_PROXY_MAX_ATTEMPTS", value)
		cfg, err = Parse(nil)
		if err != nil || cfg.ProxyMaxAttempts != 10 || cfg.ProxyMaxAttemptsSet {
			t.Fatal("invalid legacy env did not restore default", err)
		}
	}
	if (Config{}).ProxyMaxAttemptsSet {
		t.Fatal("zero value mistaken for explicit budget")
	}
}

func TestLegacySMSZeroPrimaryOverridesAliases(t *testing.T) {
	for _, suffix := range []string{"INTERVAL", "TIMEOUT"} {
		t.Run(suffix, func(t *testing.T) {
			primary := "SMS_POLL_" + suffix + "_MS"
			alias := "LUBAN_SMS_POLL_" + suffix + "_MS"
			t.Setenv(primary, "")
			t.Setenv(alias, "")
			cfg, err := Parse(nil)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.SMSPollIntervalSet || cfg.SMSPollTimeoutSet {
				t.Fatal("unset SMS settings treated as explicit")
			}
			t.Setenv(alias, "0")
			cfg, err = Parse(nil)
			if err != nil {
				t.Fatal(err)
			}
			if suffix == "INTERVAL" && (cfg.SMSPollIntervalMS != 0 || !cfg.SMSPollIntervalSet) {
				t.Fatal("alias zero interval lost")
			}
			if suffix == "TIMEOUT" && (cfg.SMSPollTimeoutMS != 0 || !cfg.SMSPollTimeoutSet) {
				t.Fatal("alias zero timeout lost")
			}
			t.Setenv(alias, "750")
			t.Setenv(primary, "0")
			cfg, err = Parse(nil)
			if err != nil {
				t.Fatal(err)
			}
			if suffix == "INTERVAL" && cfg.SMSPollIntervalMS != 0 {
				t.Fatal("alias replaced explicit primary zero")
			}
			if suffix == "TIMEOUT" && cfg.SMSPollTimeoutMS != 0 {
				t.Fatal("alias replaced explicit primary zero")
			}
			t.Setenv(primary, "-1")
			cfg, err = Parse(nil)
			if err != nil {
				t.Fatal("legacy nonpositive SMS value rejected", err)
			}
			if suffix == "INTERVAL" && cfg.SMSPollIntervalMS != -1 {
				t.Fatal("negative interval not retained for Node timer clamping")
			}
			if suffix == "TIMEOUT" && cfg.SMSPollTimeoutMS != -1 {
				t.Fatal("negative timeout not retained for immediate manual fallback")
			}
		})
	}
}

func TestLegacyConnectionBackoffBaseAndExplicitOverride(t *testing.T) {
	t.Setenv("PROXY_CONNECTION_RETRY_BASE_MS", "4500")
	t.Setenv("CHATGPT_SAME_PROXY_CONNECTION_RETRY_DELAY_MS", "")
	cfg, err := Parse(nil)
	if err != nil || cfg.ProxyConnectionRetryBaseMS != 4500 || cfg.SameProxyRetryDelayMS != 4500 || cfg.SameProxyRetryDelaySet {
		t.Fatal("legacy base ignored", err)
	}
	for _, value := range []string{"0", "-5"} {
		t.Setenv("PROXY_CONNECTION_RETRY_BASE_MS", value)
		cfg, err = Parse(nil)
		if err != nil || cfg.ProxyConnectionRetryBaseMS != 1 || cfg.SameProxyRetryDelayMS != 1 {
			t.Fatal("legacy minimum 1 ms changed", err)
		}
	}
	t.Setenv("PROXY_CONNECTION_RETRY_BASE_MS", "4500")
	for _, value := range []string{"0", "80"} {
		t.Setenv("CHATGPT_SAME_PROXY_CONNECTION_RETRY_DELAY_MS", value)
		cfg, err = Parse(nil)
		if err != nil || !cfg.SameProxyRetryDelaySet {
			t.Fatal("explicit new override lost", err)
		}
		if value == "0" && cfg.SameProxyRetryDelayMS != 0 {
			t.Fatal("zero connection delay lost")
		}
		if value == "80" && cfg.SameProxyRetryDelayMS != 80 {
			t.Fatal("connection delay override lost")
		}
	}
}
