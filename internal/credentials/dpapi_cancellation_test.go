package credentials

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"testing"
)

func TestWindowsDPAPILoadCancellationDiscardsDecryptedCredentials(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		name := "AESGCM"
		if legacy {
			name = "LegacyDPAPI"
		}
		t.Run(name, func(t *testing.T) {
			// Mock only the synchronous Windows OS boundary: cancellation can
			// happen while CryptUnprotectData is executing. Its returned secrets
			// must not escape a canceled LoadContext call.
			wrappingKey := bytes.Repeat([]byte{7}, 32)
			s := &SecureFileStore{Root: t.TempDir(), platform: "windows"}
			s.protect = func(data []byte) ([]byte, error) {
				encoded, err := encrypt(wrappingKey, data)
				return []byte(encoded), err
			}
			email := "cancel-fixture@example.test"
			credentials := Credentials{Password: "synthetic-cancel-password", TOTPSecret: "SYNTHETIC-SEED"}
			if legacy {
				plain, err := json.Marshal(credentials)
				if err != nil {
					t.Fatal(err)
				}
				protected, err := s.protect(plain)
				if err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(s.path(email), []byte(base64.StdEncoding.EncodeToString(protected)), 0600); err != nil {
					t.Fatal(err)
				}
			} else if err := s.Save(email, credentials); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(s.path(email))
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			called := false
			s.unprotect = func(data []byte) ([]byte, error) {
				called = true
				plain, err := decrypt(wrappingKey, string(data))
				cancel()
				return plain, err
			}
			got, err := s.LoadContext(ctx, email)
			if !called {
				t.Fatal("test did not reach DPAPI boundary")
			}
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled load must return context.Canceled, got %v", err)
			}
			if !reflect.DeepEqual(got, Credentials{}) {
				t.Fatal("canceled load returned decrypted credentials")
			}
			after, err := os.ReadFile(s.path(email))
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("canceled load modified the encrypted file")
			}
		})
	}
}
