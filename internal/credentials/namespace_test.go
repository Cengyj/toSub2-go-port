package credentials

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"tosub2-go-port/internal/privatefile"
)

type keychainCall struct {
	operation, service, account string
}

type recordedKeychain struct {
	memory memoryKeychain
	mu     sync.Mutex
	calls  []keychainCall
}

func (m *recordedKeychain) run(ctx context.Context, args []string, input string) ([]byte, error) {
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
	return m.memory.run(ctx, args, input)
}

func TestIsolatedKeychainIgnoresLegacyServicesAndFiles(t *testing.T) {
	service := macIsolatedServicePrefix + "0123456789abcdef"
	root, configured, home := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("TOSUB2_MAC_KEYCHAIN_SERVICE", service)
	t.Setenv("TOSUB2_MAC_CREDENTIAL_ROOT", configured)
	key := bytes.Repeat([]byte{7}, 32)
	legacyKey := base64.StdEncoding.EncodeToString(key)
	m := &recordedKeychain{memory: memoryKeychain{values: map[string]string{
		macKeychainService: legacyKey, macLegacyKeyService: legacyKey,
	}}}
	s, err := newSecureFileStore(root, "darwin")
	if err != nil {
		t.Fatal(err)
	}
	s.run = m.run
	email := "Owner@Example.test"
	legacyPaths := []string{
		filepath.Join(root, credentialID(email)+".cred"),
		filepath.Join(root, credentialID(email)+".enc"),
		filepath.Join(configured, credentialID(email)+".enc"),
		filepath.Join(home, macLegacyRootName, credentialID(email)+".enc"),
	}
	for _, path := range legacyPaths {
		writeEncrypted(t, path, key)
	}
	if s.Root != configured || s.fallbackRoot != "" || !reflect.DeepEqual(s.paths(email), []string{s.path(email)}) {
		t.Fatal("isolated credential lookup escaped its configured .cred path")
	}
	if _, err = s.Load(email); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("isolated runtime restored a legacy credential: %v", err)
	}
	value := Credentials{Password: "isolated-password-canary", TOTPSecret: "isolated-totp-canary", ProxyURL: "http://user:isolated-proxy-canary@localhost:8080"}
	if err = s.Save(email, value); err != nil {
		t.Fatal(err)
	}
	got, err := s.Load(email)
	if err != nil || !reflect.DeepEqual(got, value) {
		t.Fatalf("isolated credential roundtrip failed: %v", err)
	}
	data, err := os.ReadFile(s.path(email))
	if err != nil {
		t.Fatal(err)
	}
	for _, canary := range []string{"isolated-password-canary", "isolated-totp-canary", "isolated-proxy-canary"} {
		if bytes.Contains(data, []byte(canary)) {
			t.Fatal("isolated credential file contains plaintext")
		}
	}
	file, err := os.Open(s.path(email))
	if err != nil {
		t.Fatal(err)
	}
	verifyErr := privatefile.Verify(file)
	_ = file.Close()
	if verifyErr != nil {
		t.Fatal(verifyErr)
	}
	if err = s.Delete(email); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(s.path(email)); !os.IsNotExist(err) {
		t.Fatal("isolated credential ciphertext was not deleted")
	}
	for _, path := range legacyPaths {
		data, err = os.ReadFile(path)
		if err != nil {
			t.Fatal("isolated deletion touched an earlier credential file")
		}
		if _, err = decryptCredentialEnvelope(key, string(data)); err != nil {
			t.Fatal("isolated write changed an earlier credential file")
		}
	}
	if m.memory.values[macKeychainService] != legacyKey || m.memory.values[macLegacyKeyService] != legacyKey {
		t.Fatal("isolated deletion touched earlier Keychain services")
	}
	if _, ok := m.memory.values[service]; ok {
		t.Fatal("isolated deletion left its Keychain key")
	}
	for _, call := range m.calls {
		if call.service != service || call.account != "owner@example.test" {
			t.Fatal("isolated operation used a different namespace or noncanonical account")
		}
	}
}

func TestIsolatedKeychainServiceAndRootFreezeAtConstruction(t *testing.T) {
	firstService, secondService := macIsolatedServicePrefix+"first", macIsolatedServicePrefix+"second"
	firstRoot, secondRoot := t.TempDir(), t.TempDir()
	m := &memoryKeychain{values: map[string]string{}}
	t.Setenv("TOSUB2_MAC_KEYCHAIN_SERVICE", firstService)
	t.Setenv("TOSUB2_MAC_CREDENTIAL_ROOT", firstRoot)
	first, err := newSecureFileStore(t.TempDir(), "darwin")
	if err != nil {
		t.Fatal(err)
	}
	first.run = m.run
	t.Setenv("TOSUB2_MAC_KEYCHAIN_SERVICE", secondService)
	t.Setenv("TOSUB2_MAC_CREDENTIAL_ROOT", secondRoot)
	second, err := newSecureFileStore(t.TempDir(), "darwin")
	if err != nil {
		t.Fatal(err)
	}
	second.run = m.run
	const email = "same-owner@example.test"
	for i, store := range []*SecureFileStore{first, second} {
		if err := store.Save(email, Credentials{Password: []string{"first-synthetic-password", "second-synthetic-password"}[i]}); err != nil {
			t.Fatal(err)
		}
	}
	if first.Root != firstRoot || second.Root != secondRoot || m.values[firstService] == "" || m.values[secondService] == "" || m.values[firstService] == m.values[secondService] {
		t.Fatal("runtime namespaces shared a key or followed changed environment")
	}
	if err := first.Delete(email); err != nil {
		t.Fatal(err)
	}
	value, err := second.Load(email)
	if err != nil || value.Password != "second-synthetic-password" {
		t.Fatalf("deleting first runtime damaged second runtime: %v", err)
	}
}

func TestIsolatedKeychainUnavailableNeverWritesPlaintext(t *testing.T) {
	t.Setenv("TOSUB2_MAC_KEYCHAIN_SERVICE", macIsolatedServicePrefix+"unavailable")
	t.Setenv("TOSUB2_MAC_CREDENTIAL_ROOT", "")
	root := t.TempDir()
	s, err := newSecureFileStore(root, "darwin")
	if err != nil {
		t.Fatal(err)
	}
	m := &recordedKeychain{memory: memoryKeychain{unavailable: true}}
	s.run = m.run
	if err = s.Save("owner@example.test", Credentials{Password: "must-not-persist"}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("unavailable isolated Keychain did not fail safely: %v", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 || m.memory.writes != 0 {
		t.Fatal("unavailable isolated Keychain persisted a fallback")
	}
}

func TestMacKeychainServiceValidation(t *testing.T) {
	for _, suffix := range []string{"a", "0123456789abcdef", "ABC_def-123.example", strings.Repeat("x", macIsolatedServiceSuffixLimit)} {
		t.Run("valid_"+suffix, func(t *testing.T) {
			t.Setenv("TOSUB2_MAC_KEYCHAIN_SERVICE", macIsolatedServicePrefix+suffix)
			if _, err := newSecureFileStore(t.TempDir(), "darwin"); err != nil {
				t.Fatal(err)
			}
		})
	}
	invalid := []string{" ", macKeychainService, macLegacyKeyService, macIsolatedServicePrefix, "unrelated.secret-canary", macIsolatedServicePrefix + strings.Repeat("x", macIsolatedServiceSuffixLimit+1)}
	for _, suffix := range []string{"with space", "slash/name", "back\\slash", "line\nfeed", "secret:canary", "secret@canary", "非ASCII"} {
		invalid = append(invalid, macIsolatedServicePrefix+suffix)
	}
	for i, service := range invalid {
		t.Run(string(rune('a'+i)), func(t *testing.T) {
			t.Setenv("TOSUB2_MAC_KEYCHAIN_SERVICE", service)
			s, err := newSecureFileStore(t.TempDir(), "darwin")
			if err == nil || s != nil {
				t.Fatal("invalid runtime Keychain service accepted")
			}
			if strings.Contains(err.Error(), "secret-canary") || strings.Contains(err.Error(), "secret:canary") || strings.Contains(err.Error(), "secret@canary") {
				t.Fatal("invalid service error disclosed user input")
			}
		})
	}
}

func TestMacKeychainNamespaceDefaultsAndOtherPlatforms(t *testing.T) {
	t.Setenv("TOSUB2_MAC_KEYCHAIN_SERVICE", "")
	t.Setenv("TOSUB2_MAC_CREDENTIAL_ROOT", "")
	s, err := newSecureFileStore(t.TempDir(), "darwin")
	if err != nil || !reflect.DeepEqual(s.keychainServices(), []string{macKeychainService, macLegacyKeyService}) {
		t.Fatal("unset service changed legacy compatibility")
	}
	t.Setenv("TOSUB2_MAC_KEYCHAIN_SERVICE", macIsolatedServicePrefix+"later")
	if !reflect.DeepEqual(s.keychainServices(), []string{macKeychainService, macLegacyKeyService}) {
		t.Fatal("default store changed service after construction")
	}
	direct := &SecureFileStore{Root: t.TempDir(), platform: "darwin"}
	if !reflect.DeepEqual(direct.keychainServices(), []string{macKeychainService, macLegacyKeyService}) {
		t.Fatal("directly constructed test store lost legacy defaults")
	}
	t.Setenv("TOSUB2_MAC_KEYCHAIN_SERVICE", "invalid-but-macos-only")
	windows, err := newSecureFileStore(t.TempDir(), "windows")
	if err != nil || windows.keychainService != "" {
		t.Fatal("macOS namespace affected Windows credentials")
	}
	if _, err := newSecureFileStore(t.TempDir(), "linux"); !errors.Is(err, ErrUnavailable) {
		t.Fatal("macOS namespace enabled unsafe Linux persistence")
	}
}
