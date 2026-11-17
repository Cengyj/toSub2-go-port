package credentials

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"
)

// Stay outside the user-configurable credentials.local.* namespace: appending
// a suffix to a valid service would alias another valid configured service.
const migrationKeychainPrefix = "com.local.chatgpt-onboarding.credential-migration-v1."

func (s *SecureFileStore) migrationService() string {
	return fmt.Sprintf("%s%x", migrationKeychainPrefix, sha256.Sum256([]byte(s.keychainServices()[0])))
}

// A legacy direct-Keychain value is the only durable credential copy until the
// first encrypted file has committed. Keep its encryption key in a separate
// protected item during that transition, without changing the file envelope.
// Keep the auxiliary key until Delete: an in-flight reader may already have
// observed the old direct value while another process finishes the migration.
func (s *SecureFileStore) migrationKey(ctx context.Context, email string, create bool) ([]byte, error) {
	read := func() ([]byte, error) {
		raw, err := s.runner()(ctx, []string{"find-generic-password", "-a", normalizedEmail(email), "-s", s.migrationService(), "-w"}, "")
		if err != nil {
			return nil, err
		}
		key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw)))
		if err != nil || len(key) != 32 {
			return nil, ErrUnavailable
		}
		return key, nil
	}
	key, err := read()
	if errors.Is(err, os.ErrNotExist) && create {
		key = make([]byte, 32)
		if _, err = rand.Read(key); err != nil {
			return nil, errors.New("credential migration key generation failed")
		}
		encoded := base64.StdEncoding.EncodeToString(key)
		// No -U: another process may have created the key after our read. Always
		// read back the winning item instead of replacing it and orphaning data.
		_, err = s.runner()(ctx, []string{"add-generic-password", "-a", normalizedEmail(email), "-s", s.migrationService(), "-w"}, encoded+"\n"+encoded+"\n")
		clear(key)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		key, err = read()
	}
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("macOS credential migration key unavailable; existing data preserved: %w", ErrUnavailable)
	}
	return key, nil
}
