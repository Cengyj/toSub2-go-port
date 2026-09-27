package config

import (
	"strings"
	"testing"
)

func TestParseBoundsAndLegacyFlags(t *testing.T) {
	c, err := Parse([]string{"--host=127.0.0.1", "--port=4400", "--max-active-jobs=20"})
	if err != nil || c.Port != 4400 || c.MaxActiveJobs != 20 {
		t.Fatalf("config=%+v err=%v", c, err)
	}
	if _, err := Parse([]string{"--port=0"}); err == nil {
		t.Fatal("invalid port accepted")
	}
	if _, err := Parse([]string{"--max-active-jobs=21"}); err == nil {
		t.Fatal("more than 20 jobs accepted")
	}
}

func TestSentinelLoaderConfiguration(t *testing.T) {
	t.Setenv("TOSUB2_SENTINEL_SDK_URL", "https://sdk.example.test/version/loader.js")
	cfg, err := Parse(nil)
	if err != nil || cfg.SentinelSDKURL != "https://sdk.example.test/version/loader.js" {
		t.Fatalf("SDK environment not honored: %v", err)
	}
	for _, raw := range []string{"https://sdk.example.test/loader.js", "http://localhost:1234/loader.js", "http://127.0.0.1:1234/loader.js", "http://[::1]:1234/loader.js"} {
		cfg, err = Parse([]string{"--sentinel-sdk-url=" + raw})
		if err != nil || cfg.SentinelSDKURL != raw {
			t.Fatalf("trusted loader rejected: %v", err)
		}
	}
	for _, raw := range []string{"http://sdk.example.test/loader.js", "http://127.0.0.1.attacker.test/sdk.js", "https://user:secret@example.test/sdk.js", "file:///tmp/sdk.js", "javascript:alert(1)", "https://example.test/sdk.js#fragment", "/loader.js"} {
		if _, err = Parse([]string{"--sentinel-sdk-url=" + raw}); err == nil {
			t.Fatalf("unsafe loader accepted: %q", raw)
		} else if strings.Contains(err.Error(), "secret") {
			t.Fatal("configuration error disclosed credentials")
		}
	}
}

func TestWindowsServiceModeCannotConsumeInteractiveInput(t *testing.T) {
	if cfg, err := Parse([]string{"--windows-service", "--help"}); err != nil || !cfg.CLI.Help || cfg.WindowsService {
		t.Fatal("help attempted to launch a Windows service")
	}
	if _, err := Parse([]string{"--windows-service", "--email=fixture@example.test"}); err == nil {
		t.Fatal("interactive service accepted")
	}
	cfg, err := Parse([]string{"--windows-service"})
	if err != nil || !cfg.WindowsService {
		t.Fatalf("service mode unavailable: %v", err)
	}
}
