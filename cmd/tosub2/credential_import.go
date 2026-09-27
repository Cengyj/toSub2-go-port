package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"

	"tosub2-go-port/internal/config"
	"tosub2-go-port/internal/credentials"
)

const maxCredentialEmailFileBytes = 1 << 20
const maxCredentialImportEmails = 10000

type credentialImportDependencies struct {
	newStore          func(config.Config) (credentials.Store, error)
	importCredentials func(context.Context, credentials.LegacyImportOptions, credentials.Store) (credentials.LegacyImportResult, error)
}

func runCredentialImport(ctx context.Context, cfg config.Config, output io.Writer) (bool, error) {
	return runCredentialImportWithDependencies(ctx, cfg, output, credentialImportDependencies{
		newStore:          newCredentialImportStore,
		importCredentials: credentials.ImportLegacyMacCredentials,
	})
}

func newCredentialImportStore(cfg config.Config) (credentials.Store, error) {
	if strings.TrimSpace(os.Getenv("TOSUB2_MAC_KEYCHAIN_SERVICE")) == "" {
		return nil, errors.New("offline credential import requires an explicitly configured isolated Keychain namespace")
	}
	// Construction freezes the environment-selected namespace and root without
	// accessing Keychain. No global service or plaintext fallback is accepted by
	// ImportLegacyMacCredentials, including on non-macOS platforms.
	store, err := credentials.NewSecureFileStore(filepath.Join(cfg.OutputRoot, "credentials"))
	if err != nil {
		return nil, errors.New("isolated secure credential destination is unavailable")
	}
	return store, nil
}

func runCredentialImportWithDependencies(ctx context.Context, cfg config.Config, output io.Writer, deps credentialImportDependencies) (handled bool, operationErr error) {
	if !cfg.CredentialImportEnabled() || cfg.CLI.Help {
		return false, nil
	}
	handled = true
	result := credentials.LegacyImportResult{}
	defer func() {
		// This output deliberately has no emails, paths, provider messages or
		// credential fields, including when only part of an import succeeded.
		if err := json.NewEncoder(output).Encode(result); err != nil {
			operationErr = errors.New("credential import counters could not be written")
		}
	}()
	if err := cfg.ValidateCredentialImport(); err != nil {
		return true, err
	}
	if err := ctx.Err(); err != nil {
		return true, err
	}
	emails, err := readCredentialImportEmails(cfg.CredentialEmailsFile)
	if err != nil {
		return true, err
	}
	result.Requested, result.Unprocessed = len(emails), len(emails)
	if err := ctx.Err(); err != nil {
		return true, err
	}
	if deps.newStore == nil || deps.importCredentials == nil {
		return true, errors.New("offline credential import is unavailable")
	}
	destination, err := deps.newStore(cfg)
	if err != nil || destination == nil {
		return true, errors.New("offline credential import requires an available isolated secure destination")
	}
	result, err = deps.importCredentials(ctx, credentials.LegacyImportOptions{SourceRoot: cfg.ImportLegacyCredentials, Emails: emails}, destination)
	if err != nil {
		switch {
		case errors.Is(err, context.Canceled):
			return true, context.Canceled
		case errors.Is(err, context.DeadlineExceeded):
			return true, context.DeadlineExceeded
		case errors.Is(err, credentials.ErrUnavailable):
			return true, credentials.ErrUnavailable
		case errors.Is(err, credentials.ErrLegacyImportIncomplete):
			return true, credentials.ErrLegacyImportIncomplete
		default:
			return true, errors.New("offline credential import failed; source and account details are not logged")
		}
	}
	if result.Missing > 0 || result.Failed > 0 || result.Unprocessed > 0 {
		return true, credentials.ErrLegacyImportIncomplete
	}
	return true, nil
}

func readCredentialImportEmails(path string) ([]string, error) {
	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() || before.Size() > maxCredentialEmailFileBytes {
		return nil, errors.New("credential selection file must be a readable regular file no larger than 1 MiB")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("credential selection file could not be opened")
	}
	defer file.Close()
	after, err := file.Stat()
	if err != nil || !after.Mode().IsRegular() || !os.SameFile(before, after) || after.Size() > maxCredentialEmailFileBytes {
		return nil, errors.New("credential selection file changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(file, maxCredentialEmailFileBytes+1))
	defer clear(data)
	if err != nil || len(data) > maxCredentialEmailFileBytes || !utf8.Valid(data) {
		return nil, errors.New("credential selection file is unreadable, too large or not UTF-8")
	}
	return parseCredentialImportEmails(data)
}

func parseCredentialImportEmails(data []byte) ([]string, error) {
	data = bytes.TrimSpace(bytes.TrimPrefix(data, []byte{0xef, 0xbb, 0xbf}))
	if len(data) == 0 {
		return nil, errors.New("credential selection file is empty")
	}
	var values []string
	var document any
	if json.Unmarshal(data, &document) == nil {
		var entries []any
		switch value := document.(type) {
		case string:
			entries = []any{value}
		case []any:
			entries = value
		case map[string]any:
			if jobs, exists := value["jobs"]; exists {
				if _, ambiguous := value["email"]; ambiguous {
					return nil, errors.New("credential selection JSON has conflicting account containers")
				}
				var ok bool
				entries, ok = jobs.([]any)
				if !ok {
					return nil, errors.New("credential selection JSON has an invalid account list")
				}
			} else {
				entries = []any{value}
			}
		default:
			return nil, errors.New("credential selection JSON must contain an email or account list")
		}
		for _, entry := range entries {
			switch value := entry.(type) {
			case string:
				values = append(values, value)
			case map[string]any:
				// Original console-server.mjs saveJobMetadata writes a single
				// {version:1,email,...} object. Ignore all non-email fields.
				email, ok := value["email"].(string)
				if !ok {
					return nil, errors.New("credential selection JSON contains an invalid account entry")
				}
				values = append(values, email)
			default:
				return nil, errors.New("credential selection JSON contains an invalid account entry")
			}
		}
	} else {
		for _, line := range strings.Split(string(data), "\n") {
			if value := strings.TrimSpace(line); value != "" {
				values = append(values, value)
			}
		}
	}
	if len(values) == 0 || len(values) > maxCredentialImportEmails {
		return nil, errors.New("credential selection file requires between 1 and 10000 entries")
	}
	emails := make([]string, 0, len(values))
	seen := map[string]bool{}
	for _, value := range values {
		email := strings.ToLower(strings.TrimSpace(value))
		if !validCredentialImportEmail(email) {
			return nil, errors.New("credential selection file contains an invalid email address")
		}
		if !seen[email] {
			seen[email] = true
			emails = append(emails, email)
		}
	}
	return emails, nil
}

var credentialImportLocalPart = regexp.MustCompile("^[a-z0-9.!#$%&'*+/=?^_`{|}~-]+$")
var credentialImportDomainLabel = regexp.MustCompile("^[a-z0-9-]+$")
var credentialImportTLD = regexp.MustCompile("^(?:[a-z]{2,63}|xn--[a-z0-9-]{2,59})$")

// Match the legacy console's bare email contract. Display names, address lists,
// source import lines containing passwords, controls and local domains fail.
func validCredentialImportEmail(email string) bool {
	if email == "" || len(email) > 254 || strings.Count(email, "@") != 1 {
		return false
	}
	local, domain, _ := strings.Cut(email, "@")
	if len(local) == 0 || len(local) > 64 || strings.HasPrefix(local, ".") || strings.HasSuffix(local, ".") || strings.Contains(local, "..") || !credentialImportLocalPart.MatchString(local) {
		return false
	}
	labels := strings.Split(domain, ".")
	if len(labels) < 2 {
		return false
	}
	for _, label := range labels {
		if label == "" || len(label) > 63 || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") || !credentialImportDomainLabel.MatchString(label) {
			return false
		}
	}
	return credentialImportTLD.MatchString(labels[len(labels)-1])
}
