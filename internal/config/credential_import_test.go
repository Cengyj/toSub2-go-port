package config

import (
	"strings"
	"testing"
)

func TestOfflineCredentialImportFlagsNeverFallThroughToConsole(t *testing.T) {
	args := []string{"--import-legacy-credentials=fixture-source", "--credential-emails-file=fixture-emails"}
	cfg, err := Parse(args)
	if err != nil || !cfg.CredentialImportEnabled() || cfg.CLI.Enabled() || cfg.ImportLegacyCredentials != "fixture-source" || cfg.CredentialEmailsFile != "fixture-emails" {
		t.Fatalf("offline mode unavailable: %v", err)
	}
	for _, flags := range [][]string{
		{"--import-legacy-credentials=fixture-source"},
		{"--credential-emails-file=fixture-emails"},
		{"--import-legacy-credentials="},
		{"--credential-emails-file="},
	} {
		if _, err := Parse(flags); err == nil {
			t.Fatal("incomplete offline command silently fell through")
		}
	}
}

func TestOfflineCredentialImportRejectsAccountAndServiceFlags(t *testing.T) {
	args := []string{"--import-legacy-credentials=fixture-private-path", "--credential-emails-file=fixture-private-list"}
	for _, flag := range []string{"--email=fixture-private@example.test", "--phone=+12025550123", "--setup-totp", "--add-password", "--refresh-sub2api=private.json", "--resume-checkpoint=private.json", "--out=private.json", "--output-mode=sub2api", "--native-http", "--verbose", "--windows-service"} {
		_, err := Parse(append(append([]string(nil), args...), flag))
		if err == nil {
			t.Fatalf("conflicting flag accepted: %s", strings.Split(flag, "=")[0])
		}
		if strings.Contains(err.Error(), "fixture-private") {
			t.Fatal("conflict error retained user values")
		}
	}
}

func TestOfflineCredentialImportHelpHasNoSideEffects(t *testing.T) {
	cfg, err := Parse([]string{"--import-legacy-credentials=fixture-source", "--help"})
	if err != nil || !cfg.CLI.Help {
		t.Fatal("help attempted to validate or perform import")
	}
}
