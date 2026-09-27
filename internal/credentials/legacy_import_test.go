//go:build !windows

package credentials

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"golang.org/x/sys/unix"
)

type importKeychain struct {
	mu     sync.Mutex
	values map[keychainCall]string
	calls  []keychainCall
	hook   func(context.Context, keychainCall) error
}

func (m *importKeychain) run(ctx context.Context, args []string, input string) ([]byte, error) {
	call := keychainCall{operation: args[0]}
	for i := 1; i+1 < len(args); i++ {
		switch args[i] {
		case "-s":
			call.service = args[i+1]
		case "-a":
			call.account = args[i+1]
		}
	}
	m.mu.Lock()
	m.calls = append(m.calls, call)
	m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if m.hook != nil {
		if err := m.hook(ctx, call); err != nil {
			return nil, err
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	key := keychainCall{service: call.service, account: call.account}
	switch call.operation {
	case "find-generic-password":
		if value, ok := m.values[key]; ok {
			return []byte(value), nil
		}
		return nil, os.ErrNotExist
	case "add-generic-password":
		if args[len(args)-1] != "-w" || len(strings.Split(input, "\n")) != 3 {
			return nil, errors.New("fixture expected key only on stdin")
		}
		m.values[key] = strings.Split(input, "\n")[0]
		return nil, nil
	default:
		return nil, errors.New("fixture rejects unexpected Keychain mutation")
	}
}

func (m *importKeychain) set(service, email, value string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.values[keychainCall{service: service, account: normalizedEmail(email)}] = value
}

func (m *importKeychain) get(service, email string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.values[keychainCall{service: service, account: normalizedEmail(email)}]
}

func (m *importKeychain) snapshotCalls() []keychainCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]keychainCall(nil), m.calls...)
}

func importFixture(t *testing.T) (string, *SecureFileStore, *importKeychain) {
	t.Helper()
	m := &importKeychain{values: map[keychainCall]string{}}
	dest := &SecureFileStore{Root: t.TempDir(), platform: "darwin", keychainService: macIsolatedServicePrefix + "import-fixture", run: m.run}
	return t.TempDir(), dest, m
}

func writeImportFixture(t *testing.T, root, email, suffix, format string, key []byte, value Credentials) []byte {
	t.Helper()
	plain, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var encoded string
	if format == "early-go" {
		encoded, err = encrypt(key, plain)
	} else {
		encoded, err = encryptCredentialEnvelope(key, plain)
	}
	if err != nil {
		t.Fatal(err)
	}
	data := []byte(encoded + "\n")
	if err = os.WriteFile(filepath.Join(root, credentialID(email)+suffix), data, 0600); err != nil {
		t.Fatal(err)
	}
	return data
}

func assertImportCounts(t *testing.T, got, want LegacyImportResult) {
	t.Helper()
	if got != want {
		t.Fatalf("import counters = %+v, want %+v", got, want)
	}
	if got.Requested != got.Imported+got.SkippedExisting+got.Missing+got.Failed+got.Unprocessed {
		t.Fatal("import counters do not account for every requested account")
	}
}

func TestLegacyImportFormatsPreserveSourceAndUseIndependentKey(t *testing.T) {
	for _, tc := range []struct{ name, suffix, format, service string }{
		{"node-enc", ".enc", "node", macKeychainService},
		{"go-cred", ".cred", "node", macKeychainService},
		{"early-go", ".cred", "early-go", macLegacyKeyService},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source, dest, keys := importFixture(t)
			const email = "synthetic-owner@example.test"
			key := bytes.Repeat([]byte{51}, 32)
			encodedKey := base64.StdEncoding.EncodeToString(key)
			keys.set(tc.service, email, encodedKey)
			value := Credentials{Password: "password-import-canary", TOTPSecret: "totp-import-canary", ProxyURL: "http://user:proxy-import-canary@localhost:8080", MailAPIURL: "https://mail.example.test/api?key=mail-import-canary", MailMethod: "POST", MailBody: "body-import-canary", MailHeaders: map[string]string{"Authorization": "Bearer header-import-canary"}, PendingPassword: "pending-password-import-canary", PendingTOTPSecret: "pending-totp-import-canary", PendingTOTPURI: "otpauth://totp/synthetic?secret=pending-totp-import-canary"}
			before := writeImportFixture(t, source, email, tc.suffix, tc.format, key, value)
			result, err := importLegacyMacCredentials(context.Background(), LegacyImportOptions{SourceRoot: source, Emails: []string{" Synthetic-Owner@Example.Test ", email}}, dest, "darwin", keys.run)
			if err != nil {
				t.Fatal(err)
			}
			assertImportCounts(t, result, LegacyImportResult{Requested: 1, Imported: 1})
			got, err := dest.Load(email)
			if err != nil || !reflect.DeepEqual(got, value) {
				t.Fatalf("imported credential fields changed: %v", err)
			}
			after, err := os.ReadFile(filepath.Join(source, credentialID(email)+tc.suffix))
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("import changed original ciphertext")
			}
			info, err := os.Stat(filepath.Join(source, credentialID(email)+tc.suffix))
			if err != nil || info.Mode().Perm() != 0600 {
				t.Fatal("import changed source permissions")
			}
			if keys.get(tc.service, email) != encodedKey || keys.get(dest.keychainService, email) == encodedKey || keys.get(dest.keychainService, email) == "" {
				t.Fatal("import changed or reused legacy Keychain key")
			}
			ciphertext, err := os.ReadFile(dest.path(email))
			if err != nil || bytes.Contains(ciphertext, []byte("import-canary")) {
				t.Fatal("destination is missing or contains plaintext")
			}
			for _, call := range keys.snapshotCalls() {
				if call.service != dest.keychainService && call.operation != "find-generic-password" {
					t.Fatal("import mutated source Keychain")
				}
			}
			beforeCalls := len(keys.snapshotCalls())
			result, err = importLegacyMacCredentials(context.Background(), LegacyImportOptions{SourceRoot: source, Emails: []string{email}}, dest, "darwin", keys.run)
			if err != nil {
				t.Fatal(err)
			}
			assertImportCounts(t, result, LegacyImportResult{Requested: 1, SkippedExisting: 1})
			for _, call := range keys.snapshotCalls()[beforeCalls:] {
				if call.service != dest.keychainService || call.operation != "find-generic-password" {
					t.Fatal("existing account was overwritten or source was unnecessarily read")
				}
			}
		})
	}
}

func TestLegacyImportDirectKeychainAndTruncatedCompleteFields(t *testing.T) {
	for _, raw := range []string{
		`{"password":"direct-password-canary","totpSecret":"direct-totp-canary"}`,
		`{"password":"direct-password-canary","totpSecret":"direct-totp-canary","proxyUrl":"incomplete`,
	} {
		source, dest, keys := importFixture(t)
		const email = "direct@example.test"
		keys.set(macKeychainService, email, raw)
		result, err := importLegacyMacCredentials(context.Background(), LegacyImportOptions{SourceRoot: source, Emails: []string{email}}, dest, "darwin", keys.run)
		if err != nil {
			t.Fatal(err)
		}
		assertImportCounts(t, result, LegacyImportResult{Requested: 1, Imported: 1})
		got, err := dest.Load(email)
		if err != nil || got.Password != "direct-password-canary" || got.TOTPSecret != "direct-totp-canary" || got.ProxyURL != "" {
			t.Fatalf("direct legacy fields did not survive migration: %v", err)
		}
		if keys.get(macKeychainService, email) != raw {
			t.Fatal("source direct Keychain value was modified")
		}
		entries, err := os.ReadDir(source)
		if err != nil || len(entries) != 0 {
			t.Fatal("direct Keychain migration wrote source files")
		}
	}
}

func TestLegacyImportReportsPartialFailureAndNeverDisclosesInputs(t *testing.T) {
	source, dest, keys := importFixture(t)
	const good, bad, missing = "good-canary@example.test", "bad-canary@example.test", "missing-canary@example.test"
	key := bytes.Repeat([]byte{8}, 32)
	keys.set(macKeychainService, good, base64.StdEncoding.EncodeToString(key))
	keys.set(macKeychainService, bad, base64.StdEncoding.EncodeToString(key))
	writeImportFixture(t, source, good, ".enc", "node", key, Credentials{Password: "secret-canary"})
	if err := os.WriteFile(filepath.Join(source, credentialID(bad)+".enc"), []byte("damaged-secret-canary"), 0600); err != nil {
		t.Fatal(err)
	}
	result, err := importLegacyMacCredentials(context.Background(), LegacyImportOptions{SourceRoot: source, Emails: []string{bad, missing, good, "invalid-secret-canary"}}, dest, "darwin", keys.run)
	if !errors.Is(err, ErrLegacyImportIncomplete) {
		t.Fatal("partial failure did not return its explicit error")
	}
	assertImportCounts(t, result, LegacyImportResult{Requested: 4, Imported: 1, Missing: 1, Failed: 2})
	data, marshalErr := json.Marshal(result)
	if marshalErr != nil || strings.Contains(string(data)+err.Error(), "canary") || strings.Contains(err.Error(), source) {
		t.Fatal("import report disclosed account or credential material")
	}
	if _, err := dest.Load(good); err != nil {
		t.Fatal("one bad source prevented independent valid import")
	}
}

func TestLegacyImportCancellationCountsUnprocessedAndPreservesContext(t *testing.T) {
	for _, cancelAt := range []string{"before-start", "second-source"} {
		t.Run(cancelAt, func(t *testing.T) {
			source, dest, keys := importFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			emails := []string{"first@example.test", "second@example.test", "third@example.test"}
			for _, email := range emails {
				keys.set(macKeychainService, email, `{"password":"cancel-canary"}`)
			}
			if cancelAt == "before-start" {
				cancel()
			} else {
				keys.hook = func(ctx context.Context, call keychainCall) error {
					if call.service == macKeychainService && call.account == emails[1] {
						cancel()
						return ctx.Err()
					}
					return nil
				}
			}
			result, err := importLegacyMacCredentials(ctx, LegacyImportOptions{SourceRoot: source, Emails: emails}, dest, "darwin", keys.run)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation lost context error: %v", err)
			}
			want := LegacyImportResult{Requested: 3, Unprocessed: 3}
			if cancelAt == "second-source" {
				want.Imported, want.Unprocessed = 1, 2
			}
			assertImportCounts(t, result, want)
			for _, call := range keys.snapshotCalls() {
				if call.account == emails[2] || (call.account == emails[1] && call.operation != "find-generic-password") {
					t.Fatal("canceled import kept processing or writing accounts")
				}
			}
			if cancelAt == "before-start" && len(keys.snapshotCalls()) != 0 {
				t.Fatal("already canceled import accessed Keychain")
			}
		})
	}
}

func TestLegacyImportUnsafeSourceFilesFailBeforeReadingLegacyKey(t *testing.T) {
	for _, kind := range []string{"world-readable", "symlink", "hardlink", "directory", "fifo"} {
		t.Run(kind, func(t *testing.T) {
			source, dest, keys := importFixture(t)
			const email = "unsafe@example.test"
			path := filepath.Join(source, credentialID(email)+".enc")
			var err error
			switch kind {
			case "world-readable":
				if err = os.WriteFile(path, []byte("synthetic"), 0644); err != nil {
					t.Fatal(err)
				}
				// Set the intended unsafe mode explicitly: a restrictive test
				// process umask would otherwise create a safe 0600 file.
				if err = os.Chmod(path, 0644); err != nil {
					t.Fatal(err)
				}
				info, statErr := os.Stat(path)
				if statErr != nil {
					t.Fatal(statErr)
				}
				if info.Mode().Perm() != 0644 {
					t.Fatalf("world-readable fixture mode = %04o, want 0644", info.Mode().Perm())
				}
			case "symlink", "hardlink":
				other := filepath.Join(t.TempDir(), "source")
				if err = os.WriteFile(other, []byte("synthetic"), 0600); err != nil {
					t.Fatal(err)
				}
				if kind == "symlink" {
					err = os.Symlink(other, path)
				} else {
					err = os.Link(other, path)
				}
			case "directory":
				err = os.Mkdir(path, 0700)
			case "fifo":
				err = unix.Mkfifo(path, 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			result, err := importLegacyMacCredentials(context.Background(), LegacyImportOptions{SourceRoot: source, Emails: []string{email}}, dest, "darwin", keys.run)
			if !errors.Is(err, ErrLegacyImportIncomplete) {
				t.Fatalf("unsafe file accepted: %v", err)
			}
			assertImportCounts(t, result, LegacyImportResult{Requested: 1, Failed: 1})
			for _, call := range keys.snapshotCalls() {
				if call.service != dest.keychainService || call.operation != "find-generic-password" {
					t.Fatal("unsafe source triggered legacy key read or destination write")
				}
			}
		})
	}
}

func TestLegacyImportRejectsUnsafeRootsAndNonIsolatedDestinations(t *testing.T) {
	for _, kind := range []string{"source-symlink", "source-writable", "same-root", "destination-symlink", "global-destination", "memory-destination", "wrong-platform"} {
		t.Run(kind, func(t *testing.T) {
			source, dest, keys := importFixture(t)
			var destination Store = dest
			platform := "darwin"
			switch kind {
			case "source-symlink":
				link := filepath.Join(t.TempDir(), "source-link")
				if err := os.Symlink(source, link); err != nil {
					t.Fatal(err)
				}
				source = link
			case "source-writable":
				if err := os.Chmod(source, 0777); err != nil {
					t.Fatal(err)
				}
			case "same-root":
				dest.Root = source
			case "destination-symlink":
				link := filepath.Join(t.TempDir(), "destination-link")
				if err := os.Symlink(dest.Root, link); err != nil {
					t.Fatal(err)
				}
				dest.Root = link
			case "global-destination":
				dest.keychainService = ""
			case "memory-destination":
				destination = NewMemoryStore()
			case "wrong-platform":
				platform = "linux"
			}
			result, err := importLegacyMacCredentials(context.Background(), LegacyImportOptions{SourceRoot: source, Emails: []string{"owner@example.test"}}, destination, platform, keys.run)
			if err == nil || strings.Contains(err.Error(), source) {
				t.Fatal("unsafe configuration accepted or path leaked")
			}
			assertImportCounts(t, result, LegacyImportResult{Requested: 1, Unprocessed: 1})
			if len(keys.snapshotCalls()) != 0 {
				t.Fatal("unsafe configuration accessed Keychain")
			}
		})
	}
}

func TestLegacyImportPreservesCorruptDestinationWithoutReadingSource(t *testing.T) {
	source, dest, keys := importFixture(t)
	const email = "existing@example.test"
	before := []byte("broken-destination-secret-canary")
	if err := os.WriteFile(dest.path(email), before, 0600); err != nil {
		t.Fatal(err)
	}
	keys.set(dest.keychainService, email, base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{8}, 32)))
	keys.set(macKeychainService, email, `{"password":"valid-source-canary"}`)
	result, err := importLegacyMacCredentials(context.Background(), LegacyImportOptions{SourceRoot: source, Emails: []string{email}}, dest, "darwin", keys.run)
	if !errors.Is(err, ErrLegacyImportIncomplete) {
		t.Fatal("unreadable existing account did not report failure")
	}
	assertImportCounts(t, result, LegacyImportResult{Requested: 1, Failed: 1})
	after, err := os.ReadFile(dest.path(email))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("corrupt destination was overwritten")
	}
	for _, call := range keys.snapshotCalls() {
		if call.service != dest.keychainService || call.operation != "find-generic-password" {
			t.Fatal("destination failure read source or overwrote a key")
		}
	}
}

func TestLegacyImportSaveFailureDoesNotReportImportedOrDiscloseRunnerError(t *testing.T) {
	source, dest, keys := importFixture(t)
	const email = "save-failure@example.test"
	keys.set(macKeychainService, email, `{"password":"source-secret-canary"}`)
	keys.hook = func(_ context.Context, call keychainCall) error {
		if call.operation == "add-generic-password" {
			return errors.New("private-runner-error-secret-canary")
		}
		return nil
	}
	result, err := importLegacyMacCredentials(context.Background(), LegacyImportOptions{SourceRoot: source, Emails: []string{email}}, dest, "darwin", keys.run)
	if !errors.Is(err, ErrLegacyImportIncomplete) || strings.Contains(err.Error(), "canary") {
		t.Fatal("save failure was hidden or disclosed runner details")
	}
	assertImportCounts(t, result, LegacyImportResult{Requested: 1, Failed: 1})
	if _, err := os.Stat(dest.path(email)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("failed key creation persisted a credential fallback")
	}
}

func TestLegacyImportConcurrentCopiesSerializeWithoutOverwrite(t *testing.T) {
	source, dest, keys := importFixture(t)
	const email = "concurrent@example.test"
	keys.set(macKeychainService, email, `{"password":"concurrent-canary"}`)
	results := make(chan LegacyImportResult, 2)
	errorsCh := make(chan error, 2)
	for range 2 {
		go func() {
			result, err := importLegacyMacCredentials(context.Background(), LegacyImportOptions{SourceRoot: source, Emails: []string{email}}, dest, "darwin", keys.run)
			results <- result
			errorsCh <- err
		}()
	}
	a, b := <-results, <-results
	for range 2 {
		if err := <-errorsCh; err != nil {
			t.Fatal(err)
		}
	}
	if a.Imported+b.Imported != 1 || a.SkippedExisting+b.SkippedExisting != 1 || a.Unprocessed+b.Unprocessed != 0 {
		t.Fatal("concurrent imports did not atomically skip existing destination")
	}
	writes := 0
	for _, call := range keys.snapshotCalls() {
		if call.operation == "add-generic-password" {
			writes++
		}
	}
	if writes != 1 {
		t.Fatal("concurrent import generated more than one destination key")
	}
}

func TestLegacyImportReadsOnlyExplicitSourceDirectory(t *testing.T) {
	source, dest, keys := importFixture(t)
	const email = "selected-directory@example.test"
	key := bytes.Repeat([]byte{9}, 32)
	keys.set(macKeychainService, email, base64.StdEncoding.EncodeToString(key))
	outside := t.TempDir()
	t.Setenv("TOSUB2_MAC_CREDENTIAL_ROOT", outside)
	before := writeImportFixture(t, outside, email, ".enc", "node", key, Credentials{Password: "outside-source-canary"})
	result, err := importLegacyMacCredentials(context.Background(), LegacyImportOptions{SourceRoot: source, Emails: []string{email}}, dest, "darwin", keys.run)
	if err != nil {
		t.Fatal(err)
	}
	assertImportCounts(t, result, LegacyImportResult{Requested: 1, Missing: 1})
	if _, err := os.Stat(dest.path(email)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("explicit import fell back to another credential directory")
	}
	after, err := os.ReadFile(filepath.Join(outside, credentialID(email)+".enc"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("unselected source was modified")
	}
}

func TestLegacyImportFailedReadbackRemainsFailureAndRetryVerifiesExisting(t *testing.T) {
	source, dest, keys := importFixture(t)
	const email = "verify@example.test"
	keys.set(macKeychainService, email, `{"password":"readback-secret-canary"}`)
	readbackAttempted := false
	keys.hook = func(_ context.Context, call keychainCall) error {
		if call.service == dest.keychainService && call.operation == "find-generic-password" {
			if _, err := os.Stat(dest.path(email)); err == nil {
				readbackAttempted = true
				return errors.New("synthetic-keychain-locked-canary")
			}
		}
		return nil
	}
	result, err := importLegacyMacCredentials(context.Background(), LegacyImportOptions{SourceRoot: source, Emails: []string{email}}, dest, "darwin", keys.run)
	if !errors.Is(err, ErrLegacyImportIncomplete) || !readbackAttempted || strings.Contains(err.Error(), "canary") {
		t.Fatal("failed post-write verification was hidden or leaked private error")
	}
	assertImportCounts(t, result, LegacyImportResult{Requested: 1, Failed: 1})
	keys.hook = nil
	result, err = importLegacyMacCredentials(context.Background(), LegacyImportOptions{SourceRoot: source, Emails: []string{email}}, dest, "darwin", keys.run)
	if err != nil {
		t.Fatal(err)
	}
	assertImportCounts(t, result, LegacyImportResult{Requested: 1, SkippedExisting: 1})
	got, err := dest.Load(email)
	if err != nil || got.Password != "readback-secret-canary" {
		t.Fatal("retry could not verify the securely persisted partial copy")
	}
}
