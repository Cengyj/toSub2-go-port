package config

import (
	"errors"
	"strings"
)

func (c Config) CredentialImportEnabled() bool {
	return c.CredentialImportRequested || c.ImportLegacyCredentials != "" || c.CredentialEmailsFile != ""
}

func (c Config) ValidateCredentialImport() error {
	if strings.TrimSpace(c.ImportLegacyCredentials) == "" || strings.TrimSpace(c.CredentialEmailsFile) == "" {
		return errors.New("offline credential import requires both --import-legacy-credentials and --credential-emails-file")
	}
	if c.CLI.Enabled() {
		return errors.New("offline credential import cannot be combined with account operation flags")
	}
	if c.WindowsService {
		return errors.New("offline credential import cannot be combined with --windows-service")
	}
	if strings.TrimSpace(c.OutputRoot) == "" {
		return errors.New("offline credential import requires a destination output root")
	}
	return nil
}
