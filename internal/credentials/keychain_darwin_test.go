//go:build darwin

package credentials

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"testing"
	"time"

	"tosub2-go-port/internal/privatefile"
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

func TestMacOSKeychainDirectMigrationRecoveryIntegration(t *testing.T) {
	if os.Getenv("TOSUB2_KEYCHAIN_TEST") != "1" {
		t.Skip("real direct migration requires TOSUB2_KEYCHAIN_TEST=1; synthetic isolated namespace only")
	}
	root := t.TempDir()
	service := fmt.Sprintf("%smigration-test-%d", macIsolatedServicePrefix, time.Now().UnixNano())
	t.Setenv("TOSUB2_MAC_CREDENTIAL_ROOT", root)
	t.Setenv("TOSUB2_MAC_KEYCHAIN_SERVICE", service)
	store, err := NewSecureFileStore(root)
	if err != nil {
		t.Fatal(err)
	}
	const email = "migration-native@example.invalid"
	t.Cleanup(func() {
		store.run = runSecurity
		if err := store.Delete(email); err != nil {
			t.Errorf("migration integration cleanup failed: %v", err)
		}
	})
	const direct = `{"password":"synthetic-original","totpSecret":"synthetic-original-seed"}`
	if _, err := runSecurity(t.Context(), []string{"add-generic-password", "-a", email, "-s", service, "-w"}, direct+"\n"+direct+"\n"); err != nil {
		t.Fatal(err)
	}
	store.run = func(ctx context.Context, args []string, input string) ([]byte, error) {
		if args[0] == "add-generic-password" && keychainArgument(args, "-s") == service {
			return nil, ErrUnavailable
		}
		return runSecurity(ctx, args, input)
	}
	want := Credentials{TOTPSecret: "synthetic-replacement-seed", MailBody: "synthetic-mail-body"}
	if err := store.Save(email, want); !errors.Is(err, privatefile.ErrDurabilityUncertain) {
		t.Fatalf("native migration did not retain the interrupted commit boundary: %v", err)
	}
	restarted, err := NewSecureFileStore(root)
	if err != nil {
		t.Fatal(err)
	}
	got, err := restarted.Load(email)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("native migration recovery did not read its committed value: %v", err)
	}
	if err := restarted.Save(email, want); err != nil {
		t.Fatal(err)
	}
	got, err = restarted.Load(email)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("native completed migration readback failed: %v", err)
	}
}
