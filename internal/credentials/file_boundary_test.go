package credentials

import (
	"bytes"
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"
)

func TestCredentialLookupRejectsLinkedAndNonregularFilesBeforeKeyAccess(t *testing.T) {
	for _, kind := range []string{"symlink", "directory"} {
		t.Run(kind, func(t *testing.T) {
			key := bytes.Repeat([]byte{7}, 32)
			keychain := &memoryKeychain{values: map[string]string{macKeychainService: base64.StdEncoding.EncodeToString(key)}}
			s := testMacStore(t, keychain)
			outside := filepath.Join(t.TempDir(), "outside.cred")
			writeEncrypted(t, outside, key)
			path := s.path("fixture@example.invalid")
			if kind == "symlink" {
				if err := os.Symlink(outside, path); err != nil {
					t.Skip("symlink fixture unavailable")
				}
			} else if err := os.Mkdir(path, 0700); err != nil {
				t.Fatal(err)
			}
			calls := 0
			s.run = func(ctx context.Context, args []string, input string) ([]byte, error) {
				calls++
				return keychain.run(ctx, args, input)
			}
			if _, err := s.Load("fixture@example.invalid"); err == nil {
				t.Fatal("unsafe credential path was read")
			}
			if err := s.Save("fixture@example.invalid", Credentials{Password: "replacement"}); err == nil {
				t.Fatal("unsafe credential path was replaced")
			}
			if calls != 0 {
				t.Fatal("unsafe path accessed Keychain")
			}
		})
	}
}
