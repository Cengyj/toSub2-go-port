//go:build !windows

package credentials

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func fileKeyFixture(t *testing.T) (*SecureFileStore, string) {
	t.Helper()
	base := t.TempDir()
	keyPath, root := filepath.Join(base, "keys", "master.key"), filepath.Join(base, "data", "credentials")
	if err := InitializeFileKey(keyPath, root); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TOSUB2_CREDENTIAL_KEY_FILE", keyPath)
	s, err := newSecureFileStore(root, "linux")
	if err != nil {
		t.Fatal(err)
	}
	return s, keyPath
}

func fileKeyTestCredentials() Credentials {
	return Credentials{Password: "synthetic-file-key-password", TOTPSecret: "SYNTHETICACTIVATEDSEED", PendingTOTPSecret: "SYNTHETICPENDINGSEED", PendingTOTPURI: "otpauth://synthetic-file-key", TOTPResetPending: true, TOTPResetOperationID: "synthetic-operation", MailAPIURL: "https://mail.invalid/private-key", MailHeaders: map[string]string{"Authorization": "synthetic-mail-token"}}
}

func TestFileKeyCredentialsAndRecoverySurviveReopen(t *testing.T) {
	s, keyPath := fileKeyFixture(t)
	email := "owner@example.invalid"
	want := fileKeyTestCredentials()
	if err := s.Save(email, want); err != nil {
		t.Fatal(err)
	}
	state, err := s.SealContext(t.Context(), email, []byte("synthetic-recovery-evidence"), []byte("operation-id"))
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := newSecureFileStore(s.Root, "linux")
	if err != nil {
		t.Fatal(err)
	}
	got, err := reopened.Load(" Owner@Example.invalid ")
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatal("credentials did not survive store reconstruction", err)
	}
	plain, err := reopened.OpenContext(t.Context(), email, state, []byte("operation-id"))
	if err != nil || string(plain) != "synthetic-recovery-evidence" {
		t.Fatal("recovery evidence did not survive store reconstruction", err)
	}
	raw, err := os.ReadFile(s.path(email))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{want.Password, want.TOTPSecret, want.PendingTOTPSecret, want.PendingTOTPURI, want.MailAPIURL, want.MailHeaders["Authorization"]} {
		if bytes.Contains(raw, []byte(secret)) {
			t.Fatal("plaintext credential persisted")
		}
	}
	if bytes.Contains(state, []byte("synthetic-recovery-evidence")) {
		t.Fatal("plaintext recovery evidence persisted")
	}
	// Deletion of an account must not invalidate the encrypted operation
	// journal or the root's key identity used for later recovery.
	if err := reopened.Delete(email); err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.Load(email); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("deleted credentials still available", err)
	}
	if _, err := reopened.OpenContext(t.Context(), email, state, []byte("operation-id")); err != nil {
		t.Fatal("account deletion lost independent recovery evidence", err)
	}
	before, _ := os.ReadFile(keyPath)
	if err := InitializeFileKey(keyPath, s.Root); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(keyPath)
	if !bytes.Equal(before, after) {
		t.Fatal("initializer replaced existing key")
	}
}

func TestFileKeyRejectsCiphertextSubstitutionAndTamper(t *testing.T) {
	s, _ := fileKeyFixture(t)
	if err := s.Save("a@example.invalid", fileKeyTestCredentials()); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(s.path("a@example.invalid"))
	if err := os.WriteFile(s.path("b@example.invalid"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load("b@example.invalid"); err == nil {
		t.Fatal("cross-account credential substitution accepted")
	}
	if err := s.Save("b@example.invalid", Credentials{Password: "replacement"}); err == nil {
		t.Fatal("undecryptable existing ciphertext overwritten")
	}
	preserved, _ := os.ReadFile(s.path("b@example.invalid"))
	if !bytes.Equal(preserved, raw) {
		t.Fatal("failed save changed original ciphertext")
	}
	var envelope stateCiphertext
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	envelope.Data[0] ^= 1
	tampered, _ := json.Marshal(envelope)
	if err := os.WriteFile(s.path("a@example.invalid"), tampered, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load("a@example.invalid"); err == nil {
		t.Fatal("tampered ciphertext accepted")
	}
	state, err := s.SealContext(t.Context(), "a@example.invalid", []byte("evidence"), []byte("operation-1"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.OpenContext(t.Context(), "b@example.invalid", state, []byte("operation-1")); err == nil {
		t.Fatal("cross-account recovery evidence accepted")
	}
	if _, err := s.OpenContext(t.Context(), "a@example.invalid", state, []byte("operation-2")); err == nil {
		t.Fatal("cross-operation recovery evidence accepted")
	}
	if err := json.Unmarshal(state, &envelope); err != nil {
		t.Fatal(err)
	}
	envelope.Type = fileKeyCredentialType
	forged, _ := json.Marshal(envelope)
	if err := os.WriteFile(s.path("a@example.invalid"), forged, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load("a@example.invalid"); err == nil {
		t.Fatal("recovery evidence used as credentials")
	}
}

func TestFileKeyMissingOrChangedRefusesReadWriteDeleteAndReinitialize(t *testing.T) {
	for _, mode := range []string{"missing", "changed", "unsafe-mode", "missing-reference", "changed-reference"} {
		t.Run(mode, func(t *testing.T) {
			s, keyPath := fileKeyFixture(t)
			email := "owner@example.invalid"
			if err := s.Save(email, fileKeyTestCredentials()); err != nil {
				t.Fatal(err)
			}
			original, _ := os.ReadFile(s.path(email))
			var err error
			switch mode {
			case "missing":
				err = os.Remove(keyPath)
			case "changed":
				err = os.WriteFile(keyPath, bytes.Repeat([]byte{9}, 32), 0600)
			case "unsafe-mode":
				err = os.Chmod(keyPath, 0644)
			case "missing-reference":
				err = os.Remove(filepath.Join(s.Root, fileKeyMarker))
			case "changed-reference":
				err = os.WriteFile(filepath.Join(s.Root, fileKeyMarker), []byte(`{"version":1,"key_sha256":"wrong"}`), 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.Load(email); err == nil {
				t.Fatal("invalid key/reference accepted for read")
			}
			if err := s.Save(email, Credentials{}); err == nil {
				t.Fatal("invalid key/reference accepted for write")
			}
			if err := s.Delete(email); err == nil {
				t.Fatal("invalid key/reference accepted for delete")
			}
			if _, err := s.SealContext(t.Context(), email, []byte("state"), nil); err == nil {
				t.Fatal("invalid key/reference accepted for new recovery state")
			}
			if _, err := newSecureFileStore(s.Root, "linux"); err == nil || err == ErrUnavailable {
				t.Fatal("invalid configured key allowed store construction or memory fallback", err)
			}
			if err := InitializeFileKey(keyPath, s.Root); err == nil {
				t.Fatal("invalid key/reference silently replaced by initializer")
			}
			preserved, _ := os.ReadFile(s.path(email))
			if !bytes.Equal(original, preserved) {
				t.Fatal("invalid key changed original ciphertext")
			}
		})
	}
}

func TestFileKeyConfigurationBoundaries(t *testing.T) {
	t.Setenv("TOSUB2_CREDENTIAL_KEY_FILE", "")
	if _, err := newSecureFileStore(filepath.Join(t.TempDir(), "absent"), "linux"); err != ErrUnavailable {
		t.Fatal("unconfigured fresh Linux store should remain explicitly unavailable", err)
	}
	s, keyPath := fileKeyFixture(t)
	t.Setenv("TOSUB2_CREDENTIAL_KEY_FILE", "")
	if _, err := newSecureFileStore(s.Root, "linux"); err == nil || err == ErrUnavailable {
		t.Fatal("dropping environment silently downgraded an existing encrypted store", err)
	}
	for _, kind := range []string{"empty", "long", "short", "symlink", "directory", "hardlink"} {
		t.Run(kind, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "key")
			var err error
			switch kind {
			case "empty":
				err = os.WriteFile(path, nil, 0600)
			case "long":
				err = os.WriteFile(path, make([]byte, 33), 0600)
			case "short":
				err = os.WriteFile(path, make([]byte, 31), 0600)
			case "symlink":
				err = os.Symlink(keyPath, path)
			case "directory":
				err = os.Mkdir(path, 0700)
			case "hardlink":
				err = os.Link(keyPath, path)
			}
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv("TOSUB2_CREDENTIAL_KEY_FILE", path)
			if _, err := newSecureFileStore(filepath.Join(t.TempDir(), "creds"), "linux"); err == nil || err == ErrUnavailable {
				t.Fatal("unsafe key file accepted or silently downgraded", err)
			}
		})
	}
}

func TestFileKeyReadOnlyKeyAndCanceledOperations(t *testing.T) {
	s, keyPath := fileKeyFixture(t)
	if err := os.Chmod(keyPath, 0400); err != nil {
		t.Fatal(err)
	}
	if err := s.Save("owner@example.invalid", fileKeyTestCredentials()); err != nil {
		t.Fatal("read-only key file cannot be used", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := s.SaveContext(ctx, "owner@example.invalid", Credentials{}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := s.LoadContext(ctx, "owner@example.invalid"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := s.DeleteContext(ctx, "owner@example.invalid"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := s.SealContext(ctx, "owner@example.invalid", nil, nil); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	got, err := s.Load("owner@example.invalid")
	if err != nil || !reflect.DeepEqual(got, fileKeyTestCredentials()) {
		t.Fatal("canceled operations changed credentials", err)
	}
}

func TestFileKeyConcurrentInitializationPreservesOneKey(t *testing.T) {
	base := t.TempDir()
	keyPath, root := filepath.Join(base, "keys", "master.key"), filepath.Join(base, "data", "credentials")
	var group sync.WaitGroup
	results := make(chan error, 8)
	for i := 0; i < cap(results); i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			results <- InitializeFileKey(keyPath, root)
		}()
	}
	group.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	key, err := readFileKey(keyPath)
	if err != nil || len(key) != 32 {
		t.Fatal("initializer did not publish one complete key", err)
	}
	entries, err := os.ReadDir(filepath.Dir(keyPath))
	if err != nil || len(entries) != 1 {
		t.Fatal("initializer retained key copies or temporary files", err)
	}
}

func TestFileKeyProcessHelper(t *testing.T) {
	root := os.Getenv("TOSUB2_FILE_KEY_TEST_ROOT")
	if root == "" {
		t.Skip("subprocess-only fixture")
	}
	s, err := newSecureFileStore(root, "linux")
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Load("owner@example.invalid")
	if err != nil || !reflect.DeepEqual(got, fileKeyTestCredentials()) {
		t.Fatal("fresh process failed to recover persisted credentials", err)
	}
	state, err := os.ReadFile(filepath.Join(root, "synthetic-state.bin"))
	if err != nil {
		t.Fatal(err)
	}
	plain, err := s.OpenContext(t.Context(), "owner@example.invalid", state, []byte("synthetic-operation"))
	if err != nil || string(plain) != "synthetic-evidence" {
		t.Fatal("fresh process failed to recover private operation evidence", err)
	}
}

func TestFileKeySurvivesProcessRestart(t *testing.T) {
	s, _ := fileKeyFixture(t)
	if err := s.Save("owner@example.invalid", fileKeyTestCredentials()); err != nil {
		t.Fatal(err)
	}
	state, err := s.SealContext(t.Context(), "owner@example.invalid", []byte("synthetic-evidence"), []byte("synthetic-operation"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.Root, "synthetic-state.bin"), state, 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestFileKeyProcessHelper$", "-test.timeout=30s")
	cmd.Env = append(os.Environ(), "TOSUB2_FILE_KEY_TEST_ROOT="+s.Root)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("restart fixture failed: %v\n%s", err, output)
	} else if !strings.Contains(string(output), "PASS") {
		t.Fatal("restart helper did not run")
	}
}
