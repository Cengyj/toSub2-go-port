//go:build darwin

package credentials

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"
)

func TestMacOSKeychainIntegration(t *testing.T) {
	if os.Getenv("TOSUB2_KEYCHAIN_TEST") != "1" {
		t.Skip("real macOS Keychain integration requires TOSUB2_KEYCHAIN_TEST=1 and an unlocked login Keychain; mock tests run normally")
	}
	t.Setenv("TOSUB2_MAC_CREDENTIAL_ROOT", t.TempDir())
	s, e := NewSecureFileStore(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	email := fmt.Sprintf("tosub2-go-test-%d@example.invalid", time.Now().UnixNano())
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	defer func() {
		if e := s.Delete(email); e != nil {
			t.Errorf("Keychain test cleanup failed: %v", e)
		}
	}()
	if e = s.SaveContext(ctx, email, Credentials{Password: "synthetic-only", TOTPSecret: "SYNTHETIC-TOTP"}); e != nil {
		t.Fatal(e)
	}
	got, e := s.LoadContext(ctx, email)
	if e != nil || got.Password != "synthetic-only" || got.TOTPSecret != "SYNTHETIC-TOTP" {
		t.Fatalf("Keychain integration roundtrip failed: %v", e)
	}
}
