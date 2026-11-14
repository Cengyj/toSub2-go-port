package credentials

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
)

func TestStateKeychainSurvivesCredentialDeletionAndBindsIdentity(t *testing.T) {
	m := &memoryKeychain{values: map[string]string{}}
	s := testMacStore(t, m)
	ctx := context.Background()
	const identity = "fixture@example.test"
	plain, aad := []byte(`{"cookie":"synthetic-cookie","seed":"synthetic-seed"}`), []byte("purpose/schema/operation/account")
	sealed, err := s.SealContext(ctx, identity, plain, aad)
	if err != nil {
		t.Fatal(err)
	}
	sealedAgain, err := s.SealContext(ctx, identity, plain, aad)
	if err != nil || bytes.Equal(sealed, sealedAgain) || bytes.Contains(sealed, []byte("synthetic-cookie")) {
		t.Fatal("state did not use randomized authenticated encryption")
	}
	if err := s.Save(identity, Credentials{Password: "synthetic-password"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(identity); err != nil {
		t.Fatal(err)
	}
	got, err := s.OpenContext(ctx, identity, sealed, aad)
	if err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("credential deletion destroyed recovery state: %v", err)
	}
	for _, pair := range []struct {
		identity string
		aad      []byte
	}{{"other@example.test", aad}, {identity, []byte("changed-operation")}} {
		if _, err := s.OpenContext(ctx, pair.identity, sealed, pair.aad); err == nil {
			t.Fatal("state accepted different identity or AAD")
		}
	}
	var env stateCiphertext
	if err := json.Unmarshal(sealed, &env); err != nil {
		t.Fatal(err)
	}
	env.Data[0] ^= 1
	damaged, _ := json.Marshal(env)
	if _, err := s.OpenContext(ctx, identity, damaged, aad); err == nil {
		t.Fatal("tampered ciphertext was accepted")
	}
}

func TestMissingOrLockedStateKeyNeverRegeneratesOrLooksAbsent(t *testing.T) {
	for _, locked := range []bool{false, true} {
		t.Run(map[bool]string{true: "locked", false: "missing"}[locked], func(t *testing.T) {
			m := &memoryKeychain{values: map[string]string{}}
			s := testMacStore(t, m)
			ctx := context.Background()
			sealed, err := s.SealContext(ctx, "fixture@example.test", []byte("secret"), []byte("aad"))
			if err != nil {
				t.Fatal(err)
			}
			writes := m.writes
			delete(m.values, s.stateService())
			m.unavailable = locked
			if _, err = s.OpenContext(ctx, "fixture@example.test", sealed, []byte("aad")); err == nil || errors.Is(err, os.ErrNotExist) {
				t.Fatalf("missing key treated as absent state: %v", err)
			}
			if _, err = s.SealContext(ctx, "fixture@example.test", []byte("new"), []byte("aad")); err == nil || m.writes != writes {
				t.Fatal("missing or locked key regenerated")
			}
		})
	}
}

func TestStateKeychainNamespaceAndSecretArgumentIsolation(t *testing.T) {
	m := &recordedKeychain{memory: memoryKeychain{values: map[string]string{}}}
	s := &SecureFileStore{Root: t.TempDir(), platform: "darwin", keychainService: macIsolatedServicePrefix + "synthetic", run: m.run}
	if _, err := s.SealContext(context.Background(), "fixture@example.test", []byte("synthetic-secret"), []byte("aad")); err != nil {
		t.Fatal(err)
	}
	for _, call := range m.calls {
		if call.service != s.keychainService+stateKeychainSuffix || call.account != credentialID("fixture@example.test") || strings.Contains(call.account, "@") {
			t.Fatal("state key escaped its namespace or exposed account email")
		}
	}
}

func TestDPAPIStateProtectionIsIndependentAndAuthenticated(t *testing.T) {
	wrappingKey := bytes.Repeat([]byte{42}, 32)
	s := &SecureFileStore{Root: t.TempDir(), platform: "windows"}
	s.protect = func(key []byte) ([]byte, error) { value, err := encrypt(wrappingKey, key); return []byte(value), err }
	s.unprotect = func(data []byte) ([]byte, error) { return decrypt(wrappingKey, string(data)) }
	ctx := context.Background()
	sealed, err := s.SealContext(ctx, "fixture", []byte("synthetic-recovery"), []byte("aad"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Delete("fixture"); err != nil {
		t.Fatal(err)
	}
	got, err := s.OpenContext(ctx, "fixture", sealed, []byte("aad"))
	if err != nil || string(got) != "synthetic-recovery" {
		t.Fatalf("DPAPI recovery failed: %v", err)
	}
	if _, err := s.OpenContext(ctx, "other", sealed, []byte("aad")); err == nil {
		t.Fatal("DPAPI state accepted other identity")
	}
	s.unprotect = func([]byte) ([]byte, error) { return nil, ErrUnavailable }
	if _, err := s.OpenContext(ctx, "fixture", sealed, []byte("aad")); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("DPAPI failure was hidden: %v", err)
	}
}

func TestStateProtectionHonorsCancellation(t *testing.T) {
	m := &memoryKeychain{values: map[string]string{}}
	s := testMacStore(t, m)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.SealContext(ctx, "fixture", []byte("secret"), nil); !errors.Is(err, context.Canceled) || m.writes != 0 {
		t.Fatalf("canceled state write touched keychain: %v", err)
	}
}
