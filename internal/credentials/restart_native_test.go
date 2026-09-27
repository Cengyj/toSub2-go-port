//go:build darwin || windows

package credentials

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"runtime"
	"testing"
	"time"

	"tosub2-go-port/internal/privatefile"
)

func nativeRestartCredentials(updated bool) Credentials {
	value := Credentials{Password: "synthetic-restart-password", TOTPSecret: "SYNTHETICRESTARTSEED", ProxyURL: "socks5h://fixture:synthetic-proxy-secret@proxy.example.test:1080", MailMethod: "POST", MailAPIURL: "https://mail.example.test/inbox", MailBody: "synthetic-request-body", MailHeaders: map[string]string{"Authorization": "synthetic-mail-secret"}}
	if updated {
		value.Password = "synthetic-restart-updated-password"
		value.PendingTOTPSecret = "SYNTHETICPENDINGSEED"
	}
	return value
}

// The child uses the real OS credential backend. No Store, key, or plaintext
// is inherited from the parent's address space or passed through arguments.
func TestNativeCredentialProcessHelper(t *testing.T) {
	mode := os.Getenv("TOSUB2_CREDENTIAL_RESTART_HELPER")
	if mode == "" {
		t.Skip("subprocess-only native credential restart entry")
	}
	root, email := os.Getenv("TOSUB2_CREDENTIAL_RESTART_ROOT"), os.Getenv("TOSUB2_CREDENTIAL_RESTART_EMAIL")
	if root == "" || email == "" || (mode != "update" && mode != "verify") {
		t.Fatal("incomplete isolated credential fixture")
	}
	store, err := NewSecureFileStore(root)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	got, err := store.LoadContext(ctx, email)
	if err != nil || !reflect.DeepEqual(got, nativeRestartCredentials(mode == "verify")) {
		t.Fatal("native credential restart read did not preserve account fields")
	}
	if mode == "update" {
		if err := store.SaveContext(ctx, email, nativeRestartCredentials(true)); err != nil {
			t.Fatal("native credential child update failed")
		}
	}
}

func TestNativeCredentialStoreSurvivesProcessRestart(t *testing.T) {
	if runtime.GOOS == "darwin" && os.Getenv("TOSUB2_KEYCHAIN_TEST") != "1" {
		t.Skip("set TOSUB2_KEYCHAIN_TEST=1 for real Keychain reads across fresh processes")
	}
	root := t.TempDir()
	t.Setenv("TOSUB2_MAC_CREDENTIAL_ROOT", root)
	email := fmt.Sprintf("tosub2-restart-%d@example.invalid", time.Now().UnixNano())
	store, err := NewSecureFileStore(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Delete(email); err != nil {
			t.Error("isolated native credential cleanup failed")
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := store.SaveContext(ctx, email, nativeRestartCredentials(false)); err != nil {
		t.Fatal("native credential creation failed")
	}
	for _, mode := range []string{"update", "verify"} {
		child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestNativeCredentialProcessHelper$", "-test.timeout=30s")
		child.Env = append(os.Environ(), "TOSUB2_CREDENTIAL_RESTART_HELPER="+mode, "TOSUB2_CREDENTIAL_RESTART_ROOT="+root, "TOSUB2_CREDENTIAL_RESTART_EMAIL="+email)
		if output, err := child.CombinedOutput(); err != nil {
			// Helper output contains only fixed diagnostics and synthetic fixtures.
			t.Fatalf("native credential %s subprocess: %v\n%s", mode, err, output)
		}
	}
	got, err := store.LoadContext(ctx, email)
	if err != nil || !reflect.DeepEqual(got, nativeRestartCredentials(true)) {
		t.Fatal("parent did not observe child credential update")
	}
	file, err := os.Open(store.path(email))
	if err != nil {
		t.Fatal(err)
	}
	if err := privatefile.Verify(file); err != nil {
		_ = file.Close()
		t.Fatal("restarted credential store lost private permissions")
	}
	_ = file.Close()
	encrypted, err := os.ReadFile(store.path(email))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{got.Password, got.TOTPSecret, got.PendingTOTPSecret, got.ProxyURL, got.MailBody, got.MailHeaders["Authorization"]} {
		if bytes.Contains(encrypted, []byte(secret)) {
			t.Fatal("native restart persisted plaintext credential material")
		}
	}
}
