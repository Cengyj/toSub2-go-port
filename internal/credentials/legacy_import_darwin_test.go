//go:build darwin

package credentials

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// This acceptance test uses the real macOS security command and the exported
// importer. Every Keychain operation names one random synthetic account and
// either the legacy service or one unique isolated service. No listing, broad
// deletion, existing-user lookup, or plaintext fixture output is performed.
func TestMacOSKeychainLegacyImportIntegration(t *testing.T) {
	if os.Getenv("TOSUB2_KEYCHAIN_TEST") != "1" {
		t.Skip("native legacy import requires TOSUB2_KEYCHAIN_TEST=1 and an unlocked macOS Keychain; synthetic account only")
	}
	idBytes := make([]byte, 16)
	key := make([]byte, 32)
	if _, err := rand.Read(idBytes); err != nil {
		t.Fatal("native import fixture identifier generation failed")
	}
	if _, err := rand.Read(key); err != nil {
		t.Fatal("native import fixture key generation failed")
	}
	defer clear(key)
	id := hex.EncodeToString(idBytes)
	email := "tosub2-legacy-import-" + id + "@example.invalid"
	service := macIsolatedServicePrefix + "import-test-" + id
	sourceRoot, destinationRoot := t.TempDir(), t.TempDir()
	t.Setenv("TOSUB2_MAC_KEYCHAIN_SERVICE", service)
	t.Setenv("TOSUB2_MAC_CREDENTIAL_ROOT", destinationRoot)
	destination, err := NewSecureFileStore(destinationRoot)
	if err != nil {
		t.Fatal("native import fixture store construction failed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Confirm ownership before allowing any mutation or cleanup. A rare
	// identifier collision fails without touching the pre-existing item.
	for _, fixtureService := range []string{macKeychainService, service} {
		data, err := runSecurity(ctx, []string{"find-generic-password", "-a", email, "-s", fixtureService, "-w"}, "")
		clear(data)
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatal("native import fixture account is not safely absent; refusing modification")
		}
	}
	// Register cleanup before creation so a timed-out add/import also gets
	// exact-account cleanup. Only these previously absent fixture identities
	// are owned by this test; a missing item is a successful cleanup outcome.
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		if err := destination.DeleteContext(cleanupCtx, email); err != nil {
			t.Error("native legacy import isolated credential cleanup failed")
		}
		if _, err := os.Stat(destination.path(email)); !errors.Is(err, os.ErrNotExist) {
			t.Error("native legacy import cleanup left destination ciphertext")
		}
		if _, err := runSecurity(cleanupCtx, []string{"delete-generic-password", "-a", email, "-s", macKeychainService}, ""); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Error("native legacy import source Keychain cleanup failed")
		}
		for _, fixtureService := range []string{macKeychainService, service} {
			data, err := runSecurity(cleanupCtx, []string{"find-generic-password", "-a", email, "-s", fixtureService, "-w"}, "")
			clear(data)
			if !errors.Is(err, os.ErrNotExist) {
				t.Error("native legacy import cleanup could not verify fixture key removal")
			}
		}
	})

	encodedKey := base64.StdEncoding.EncodeToString(key)
	if _, err := runSecurity(ctx, []string{"add-generic-password", "-a", email, "-s", macKeychainService, "-w"}, encodedKey+"\n"+encodedKey+"\n"); err != nil {
		t.Fatal("native legacy source key creation failed")
	}
	value := Credentials{
		Password:    "Synthetic-" + id + "-A1!",
		TOTPSecret:  base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(idBytes),
		ProxyURL:    "http://synthetic:" + id + "@127.0.0.1:18080",
		MailAPIURL:  "https://mail.example.invalid/inbox",
		MailMethod:  "POST",
		MailBody:    "synthetic-request-" + id,
		MailHeaders: map[string]string{"Authorization": "Synthetic " + id},
	}
	before := writeImportFixture(t, sourceRoot, email, ".enc", "node", key, value)
	sourcePath := filepath.Join(sourceRoot, credentialID(email)+".enc")
	opts := LegacyImportOptions{SourceRoot: sourceRoot, Emails: []string{email}}
	result, err := ImportLegacyMacCredentials(ctx, opts, destination)
	if err != nil {
		t.Fatal("native legacy credential import failed")
	}
	assertImportCounts(t, result, LegacyImportResult{Requested: 1, Imported: 1})
	got, err := destination.LoadContext(ctx, email)
	if err != nil || !reflect.DeepEqual(got, value) {
		t.Fatal("native imported credential fields did not survive readback")
	}
	destinationBefore, err := os.ReadFile(destination.path(email))
	if err != nil {
		t.Fatal("native import destination ciphertext is missing")
	}
	for _, secret := range []string{value.Password, value.TOTPSecret, value.ProxyURL, value.MailBody, value.MailHeaders["Authorization"]} {
		if bytes.Contains(destinationBefore, []byte(secret)) {
			t.Fatal("native import persisted plaintext credential material")
		}
	}
	result, err = ImportLegacyMacCredentials(ctx, opts, destination)
	if err != nil {
		t.Fatal("native repeat import failed")
	}
	assertImportCounts(t, result, LegacyImportResult{Requested: 1, SkippedExisting: 1})
	destinationAfter, err := os.ReadFile(destination.path(email))
	if err != nil || !bytes.Equal(destinationBefore, destinationAfter) {
		t.Fatal("native repeat import rewrote existing destination ciphertext")
	}

	retained := value
	retained.Password = "Synthetic-retained-" + id + "-A1!"
	if err := destination.SaveContext(ctx, email, retained); err != nil {
		t.Fatal("native import no-overwrite fixture update failed")
	}
	result, err = ImportLegacyMacCredentials(ctx, opts, destination)
	if err != nil {
		t.Fatal("native import of account with newer credentials failed")
	}
	assertImportCounts(t, result, LegacyImportResult{Requested: 1, SkippedExisting: 1})
	got, err = destination.LoadContext(ctx, email)
	if err != nil || !reflect.DeepEqual(got, retained) {
		t.Fatal("native import overwrote existing destination credentials")
	}
	after, err := os.ReadFile(sourcePath)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("native import changed the legacy source ciphertext")
	}
	info, err := os.Stat(sourcePath)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("native import changed legacy source permissions")
	}
	sourceKey, err := runSecurity(ctx, []string{"find-generic-password", "-a", email, "-s", macKeychainService, "-w"}, "")
	defer clear(sourceKey)
	if err != nil || strings.TrimSpace(string(sourceKey)) != encodedKey {
		t.Fatal("native import changed the legacy source key")
	}
	targetKey, err := runSecurity(ctx, []string{"find-generic-password", "-a", email, "-s", service, "-w"}, "")
	defer clear(targetKey)
	if err != nil || strings.TrimSpace(string(targetKey)) == encodedKey || len(strings.TrimSpace(string(targetKey))) == 0 {
		t.Fatal("native import did not establish an independent isolated key")
	}
}
