package credentials

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"os"
	"testing"
)

func TestWindowsSavePreservesExistingUnreadableCredentials(t *testing.T) {
	for _, kind := range []string{"wrong-windows-identity", "corrupt-envelope", "legacy-wrong-identity", "canceled-decryption"} {
		t.Run(kind, func(t *testing.T) {
			key := bytes.Repeat([]byte{8}, 32)
			s := &SecureFileStore{Root: t.TempDir(), platform: "windows"}
			s.protect = func(value []byte) ([]byte, error) { encoded, err := encrypt(key, value); return []byte(encoded), err }
			s.unprotect = func(value []byte) ([]byte, error) { return decrypt(key, string(value)) }
			const email = "preserve-fixture@example.test"
			if err := s.Save(email, Credentials{Password: "Previous-synthetic9!"}); err != nil {
				t.Fatal(err)
			}
			if kind == "corrupt-envelope" {
				if err := os.WriteFile(s.path(email), []byte(`{"version":2,"payload":"corrupt"}`), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "legacy-wrong-identity" {
				if err := os.WriteFile(s.path(email), []byte(base64.StdEncoding.EncodeToString([]byte("legacy-other-user"))), 0600); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.ReadFile(s.path(email))
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			decryptCalls, protectCalls := 0, 0
			s.unprotect = func(value []byte) ([]byte, error) {
				decryptCalls++
				if kind == "canceled-decryption" {
					cancel()
					return decrypt(key, string(value))
				}
				return nil, errors.New("fixture wrong Windows identity")
			}
			s.protect = func([]byte) ([]byte, error) { protectCalls++; return []byte("newly-protected"), nil }
			err = s.SaveContext(ctx, email, Credentials{Password: "Replacement-synthetic9!"})
			if err == nil || protectCalls != 0 || kind != "corrupt-envelope" && decryptCalls != 1 {
				t.Fatalf("save replaced unreadable evidence: decrypt=%d protect=%d err=%v", decryptCalls, protectCalls, err)
			}
			if kind == "canceled-decryption" && !errors.Is(err, context.Canceled) {
				t.Fatal("cancellation was lost")
			}
			after, err := os.ReadFile(s.path(email))
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("failed preflight altered original ciphertext")
			}
		})
	}
}
