package credentials

import (
	"bytes"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestMacCredentialRootControlsNewWritesAndReadsEarlierGoStore(t *testing.T) {
	fallback, configured := t.TempDir(), filepath.Join(t.TempDir(), "configured-vault")
	t.Setenv("TOSUB2_MAC_CREDENTIAL_ROOT", configured)
	key := bytes.Repeat([]byte{5}, 32)
	m := &memoryKeychain{values: map[string]string{macKeychainService: base64.StdEncoding.EncodeToString(key)}}
	s, err := newSecureFileStore(fallback, "darwin")
	if err != nil {
		t.Fatal(err)
	}
	s.run = m.run
	email := "owner@example.test"
	oldPath := filepath.Join(fallback, credentialID(email)+".cred")
	writeEncrypted(t, oldPath, key)
	old, err := s.Load(email)
	if err != nil || old.Password != "old-secret" {
		t.Fatalf("earlier Go store unavailable: %v", err)
	}
	if err = s.Save(email, Credentials{Password: "new-synthetic-password"}); err != nil {
		t.Fatal(err)
	}
	if s.Root != configured {
		t.Fatal("configured credential root ignored")
	}
	data, err := os.ReadFile(filepath.Join(configured, credentialID(email)+".cred"))
	if err != nil || bytes.Contains(data, []byte("new-synthetic-password")) {
		t.Fatal("configured encrypted write missing or plaintext")
	}
	value, err := s.Load(email)
	if err != nil || value.Password != "new-synthetic-password" {
		t.Fatalf("configured ciphertext not authoritative: %v", err)
	}
	if err = s.Delete(email); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{s.path(email), oldPath} {
		if _, err = os.Stat(p); !os.IsNotExist(err) {
			t.Fatal("delete left stale encrypted credentials")
		}
	}
}

func TestMacCredentialRootDoesNotRedirectOtherPlatforms(t *testing.T) {
	root := t.TempDir()
	t.Setenv("TOSUB2_MAC_CREDENTIAL_ROOT", filepath.Join(t.TempDir(), "mac-only"))
	s, err := newSecureFileStore(root, "windows")
	if err != nil || s.Root != root || s.fallbackRoot != "" {
		t.Fatal("macOS setting changed Windows credential path")
	}
	if _, err = newSecureFileStore(root, "linux"); !errors.Is(err, ErrUnavailable) {
		t.Fatal("environment enabled unsafe Linux persistence")
	}
}
