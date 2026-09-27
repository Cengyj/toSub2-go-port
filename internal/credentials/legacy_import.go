package credentials

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	netmail "net/mail"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// LegacyImportOptions selects the exact legacy directory and accounts to read.
// Importing is explicit; ordinary isolated Load never falls back to this source.
type LegacyImportOptions struct {
	SourceRoot string
	Emails     []string
}

// LegacyImportResult contains no account identifiers or credential material.
// The six counters always account for every distinct requested account.
type LegacyImportResult struct {
	Requested       int `json:"requested"`
	Imported        int `json:"imported"`
	SkippedExisting int `json:"skippedExisting"`
	Missing         int `json:"missing"`
	Failed          int `json:"failed"`
	Unprocessed     int `json:"unprocessed"`
}

var ErrLegacyImportIncomplete = errors.New("legacy credential import completed with account failures")

// ImportLegacyMacCredentials reads the specified old macOS directory and the
// legacy global Keychain services, then writes a separately encrypted copy to
// the current isolated Store. It never changes the source or overwrites an
// existing destination account. Callers must stop the destination service to
// exclude concurrent writers in other processes.
func ImportLegacyMacCredentials(ctx context.Context, opts LegacyImportOptions, destination Store) (LegacyImportResult, error) {
	return importLegacyMacCredentials(ctx, opts, destination, runtime.GOOS, runSecurity)
}

func importLegacyMacCredentials(ctx context.Context, opts LegacyImportOptions, destination Store, platform string, sourceRunner securityRunner) (LegacyImportResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	emails := []string{}
	seen := map[string]bool{}
	for _, raw := range opts.Emails {
		email := normalizedEmail(raw)
		if !seen[email] {
			seen[email] = true
			emails = append(emails, email)
		}
	}
	result := LegacyImportResult{Requested: len(emails), Unprocessed: len(emails)}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if platform != "darwin" {
		return result, ErrUnavailable
	}
	dest, ok := destination.(*SecureFileStore)
	if !ok || dest == nil || dest.platform != "darwin" || strings.TrimSpace(dest.Root) == "" || dest.keychainService == "" || validateMacKeychainService(dest.keychainService) != nil {
		return result, errors.New("legacy import requires an isolated macOS credential store")
	}
	if len(emails) == 0 || strings.TrimSpace(opts.SourceRoot) == "" {
		return result, errors.New("legacy credential source directory and accounts are required")
	}
	root, err := filepath.Abs(opts.SourceRoot)
	if err != nil || filepath.Dir(root) == root {
		return result, errors.New("invalid legacy credential source directory")
	}
	sourceDir, err := openLegacyImportRoot(root)
	if err != nil {
		return result, errors.New("legacy credential source directory is not safely accessible")
	}
	defer sourceDir.Close()
	if info, err := os.Stat(dest.Root); err == nil {
		sourceInfo, statErr := sourceDir.Stat()
		if statErr != nil || os.SameFile(sourceInfo, info) {
			return result, errors.New("legacy credential source and destination must be separate directories")
		}
		destinationDir, openErr := openLegacyImportRoot(dest.Root)
		if openErr != nil {
			return result, errors.New("credential destination directory is not safely accessible")
		}
		destinationDir.Close()
	} else if !errors.Is(err, os.ErrNotExist) {
		return result, errors.New("credential destination directory is not safely accessible")
	}
	// Construct only a legacy key reader. File selection below is pinned to
	// sourceDir and deliberately never calls the normal store.paths fallback.
	source := &SecureFileStore{Root: root, platform: "darwin", run: sourceRunner}
	for _, email := range emails {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		parsed, validErr := netmail.ParseAddress(email)
		if validErr != nil || parsed.Address != email || strings.ContainsAny(email, "\r\n\x00") {
			result.Unprocessed--
			result.Failed++
			continue
		}
		outcome, err := importLegacyAccount(ctx, sourceDir, source, dest, email)
		if ctx.Err() != nil && outcome == "" {
			return result, ctx.Err()
		}
		result.Unprocessed--
		if err != nil {
			result.Failed++
			continue
		}
		switch outcome {
		case "imported":
			result.Imported++
		case "existing":
			result.SkippedExisting++
		case "missing":
			result.Missing++
		}
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if result.Failed > 0 {
		return result, ErrLegacyImportIncomplete
	}
	return result, nil
}

func importLegacyAccount(parent context.Context, sourceDir *os.File, source, dest *SecureFileStore, email string) (string, error) {
	ctx, cancel := boundedContext(parent)
	defer cancel()
	if err := lockCredentialStore(ctx); err != nil {
		return "", err
	}
	defer unlockCredentialStore()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := validateLegacyImportDestination(dest, email); err != nil {
		return "", err
	}
	if _, err := dest.keychainLoadContext(ctx, email); err == nil {
		return "existing", nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", errors.New("existing destination credential could not be verified")
	}
	value, err := readLegacyImportCredential(ctx, sourceDir, source, email)
	if errors.Is(err, os.ErrNotExist) {
		return "missing", nil
	}
	if err != nil {
		return "", err
	}
	if err = ctx.Err(); err != nil {
		return "", err
	}
	if err = dest.keychainSaveContext(ctx, email, value); err != nil {
		return "", errors.New("credential import could not be saved securely")
	}
	// A successful write is counted only after the same namespace can read
	// and verify it. Errors remain explicit even if a partial copy exists.
	restored, err := dest.keychainLoadContext(ctx, email)
	if err != nil {
		return "", errors.New("imported credential could not be verified")
	}
	want, wantErr := json.Marshal(value)
	got, gotErr := json.Marshal(restored)
	defer clear(want)
	defer clear(got)
	if wantErr != nil || gotErr != nil || !bytes.Equal(want, got) {
		return "", errors.New("imported credential verification failed")
	}
	return "imported", nil
}

func validateLegacyImportDestination(dest *SecureFileStore, email string) error {
	dir, err := openLegacyImportRoot(dest.Root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return errors.New("credential destination directory is not safely accessible")
	}
	defer dir.Close()
	file, err := openLegacyImportFile(dir, credentialID(email)+".cred")
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return errors.New("existing destination credential file is not safely accessible")
	}
	if err := file.Close(); err != nil {
		return errors.New("existing destination credential file could not be closed")
	}
	return nil
}

func readLegacyImportCredential(ctx context.Context, dir *os.File, source *SecureFileStore, email string) (Credentials, error) {
	var encrypted []byte
	for _, suffix := range []string{".cred", ".enc"} {
		file, err := openLegacyImportFile(dir, credentialID(email)+suffix)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return Credentials{}, errors.New("legacy credential file is not safely accessible")
		}
		encrypted, err = io.ReadAll(io.LimitReader(file, (2<<20)+1))
		closeErr := file.Close()
		if err != nil || closeErr != nil || len(encrypted) == 0 || len(encrypted) > 2<<20 {
			return Credentials{}, errors.New("legacy credential file could not be read")
		}
		break
	}
	defer clear(encrypted)
	if err := ctx.Err(); err != nil {
		return Credentials{}, err
	}
	kv, err := source.readKeychain(ctx, email)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) && len(encrypted) == 0 {
			return Credentials{}, os.ErrNotExist
		}
		return Credentials{}, errors.New("legacy credential key is unavailable")
	}
	defer clear(kv.key)
	if len(encrypted) == 0 {
		if kv.direct != nil {
			return *kv.direct, nil
		}
		return Credentials{}, os.ErrNotExist
	}
	if len(kv.key) != 32 {
		return Credentials{}, errors.New("legacy credential key is unavailable")
	}
	plain, err := decryptCredentialEnvelope(kv.key, strings.TrimSpace(string(encrypted)))
	defer clear(plain)
	if err != nil {
		return Credentials{}, errors.New("legacy credential file could not be decrypted")
	}
	return decodeCredentials(plain)
}
