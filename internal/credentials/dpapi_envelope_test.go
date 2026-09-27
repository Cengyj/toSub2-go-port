package credentials

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"testing"
)

func TestWindowsAESGCMEnvelopeAndLegacyDPAPIMigration(t *testing.T) {
	// Only the OS boundary is mocked here. File AES-GCM, authentication checks,
	// envelope compatibility and atomic writer are the production implementations.
	wrappingKey := bytes.Repeat([]byte{9}, 32)
	s := &SecureFileStore{Root: t.TempDir(), platform: "windows"}
	s.protect = func(key []byte) ([]byte, error) {
		if len(key) != 32 {
			t.Fatal("DPAPI must protect the generated AES-256 key")
		}
		encoded, err := encrypt(wrappingKey, key)
		return []byte(encoded), err
	}
	s.unprotect = func(data []byte) ([]byte, error) { return decrypt(wrappingKey, string(data)) }
	email := "fixture@example.test"
	plain := []byte("{\"password\":\"legacy-synthetic-password\",\"totpSecret\":\"LEGACY-SEED\"}")
	legacy, err := encrypt(wrappingKey, plain)
	if err != nil {
		t.Fatal(err)
	}
	legacyFile := []byte(base64.StdEncoding.EncodeToString([]byte(legacy)))
	if err = os.WriteFile(s.path(email), legacyFile, 0600); err != nil {
		t.Fatal(err)
	}
	got, err := s.Load(email)
	if err != nil || got.Password != "legacy-synthetic-password" {
		t.Fatalf("legacy DPAPI read failed: %v", err)
	}
	stillLegacy, _ := os.ReadFile(s.path(email))
	if !bytes.Equal(stillLegacy, legacyFile) {
		t.Fatal("read unexpectedly rewrote legacy credentials")
	}
	got.Password = "updated-synthetic-password"
	if err = s.Save(email, got); err != nil {
		t.Fatal(err)
	}
	saved, err := os.ReadFile(s.path(email))
	if err != nil {
		t.Fatal(err)
	}
	var envelope dpapiCredentialEnvelope
	if json.Unmarshal(saved, &envelope) != nil || envelope.Type != "dpapi-current-user+aes-256-gcm" || envelope.Version != 1 {
		t.Fatal("AES-GCM envelope missing")
	}
	for _, secret := range []string{got.Password, got.TOTPSecret, "legacy-synthetic-password"} {
		if bytes.Contains(saved, []byte(secret)) {
			t.Fatal("credential escaped encryption")
		}
	}
	loaded, err := s.Load(email)
	if err != nil || !reflect.DeepEqual(loaded, got) {
		t.Fatalf("hybrid encryption roundtrip failed: %v", err)
	}
	var payload map[string]any
	if json.Unmarshal(envelope.Payload, &payload) != nil {
		t.Fatal("invalid GCM payload")
	}
	ciphertext, err := base64.StdEncoding.DecodeString(payload["data"].(string))
	if err != nil {
		t.Fatal(err)
	}
	ciphertext[0] ^= 1
	payload["data"] = base64.StdEncoding.EncodeToString(ciphertext)
	envelope.Payload, _ = json.Marshal(payload)
	damaged, _ := json.Marshal(envelope)
	if err = os.WriteFile(s.path(email), damaged, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Load(email); err == nil {
		t.Fatal("GCM accepted tampered credentials")
	}
	after, _ := os.ReadFile(s.path(email))
	if !bytes.Equal(after, damaged) {
		t.Fatal("failed read modified encrypted evidence")
	}
}

func TestWindowsAESGCMProtectionFailurePreservesFile(t *testing.T) {
	s := &SecureFileStore{Root: t.TempDir(), platform: "windows", protect: func([]byte) ([]byte, error) { return nil, errors.New("test DPAPI denied") }}
	before := []byte("previous protected credentials")
	if err := os.WriteFile(s.path("fixture@example.test"), before, 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.Save("fixture@example.test", Credentials{Password: "replacement"}); err == nil {
		t.Fatal("failed DPAPI protection accepted")
	}
	after, _ := os.ReadFile(s.path("fixture@example.test"))
	if !bytes.Equal(before, after) {
		t.Fatal("failed DPAPI protection overwrote original file")
	}
}
