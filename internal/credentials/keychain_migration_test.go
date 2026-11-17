package credentials

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"tosub2-go-port/internal/privatefile"
)

func TestDirectKeychainMigrationFailurePreservesOriginalCredentials(t *testing.T) {
	for _, fail := range []string{"cancel", "file-write"} {
		t.Run(fail, func(t *testing.T) {
			keychain := &memoryKeychain{values: map[string]string{macKeychainService: `{"password":"synthetic-original","totpSecret":"synthetic-seed"}`}}
			store := testMacStore(t, keychain)
			const email = "migration-failure@example.invalid"
			original, err := store.Load(email)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			store.run = func(ctx context.Context, args []string, input string) ([]byte, error) {
				value, err := keychain.run(ctx, args, input)
				if err == nil && args[0] == "add-generic-password" {
					if fail == "cancel" {
						cancel()
					} else {
						// Make the destination unwritable only after key creation.
						if err := os.Remove(store.Root); err != nil {
							t.Fatal(err)
						}
						if err := os.WriteFile(store.Root, []byte("synthetic blocker"), 0600); err != nil {
							t.Fatal(err)
						}
					}
				}
				return value, err
			}
			err = store.SaveContext(ctx, email, Credentials{Password: "synthetic-replacement"})
			if err == nil || fail == "cancel" && !errors.Is(err, context.Canceled) {
				t.Fatalf("migration failure was hidden: %v", err)
			}
			if fail == "file-write" {
				if err := os.Remove(store.Root); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(store.Root, 0700); err != nil {
					t.Fatal(err)
				}
			}
			// A fresh store must still recover the original value after restart.
			restarted := &SecureFileStore{Root: store.Root, platform: "darwin", run: keychain.run}
			got, err := restarted.Load(email)
			if err != nil || !reflect.DeepEqual(got, original) {
				t.Fatalf("failed migration lost the original credentials: %v", err)
			}
		})
	}
}

func keychainArgument(args []string, name string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == name {
			return args[i+1]
		}
	}
	return ""
}

func TestDirectKeychainMigrationRecoversCommittedFileBeforePrimarySwitch(t *testing.T) {
	for _, committed := range []bool{false, true} {
		t.Run(map[bool]string{false: "switch-refused", true: "switch-response-lost"}[committed], func(t *testing.T) {
			const original = `{"password":"synthetic-old-password","totpSecret":"synthetic-old-seed"}`
			keychain := &memoryKeychain{values: map[string]string{macKeychainService: original}}
			store := testMacStore(t, keychain)
			const email = "migration-restart@example.invalid"
			store.run = func(ctx context.Context, args []string, input string) ([]byte, error) {
				if args[0] == "add-generic-password" && keychainArgument(args, "-s") == macKeychainService {
					if committed {
						if _, err := keychain.run(ctx, args, input); err != nil {
							return nil, err
						}
					}
					return nil, ErrUnavailable
				}
				return keychain.run(ctx, args, input)
			}
			// Explicit empty password/proxy must not be restored from the old item.
			want := Credentials{TOTPSecret: "synthetic-new-seed", MailBody: "synthetic-mail-body"}
			if err := store.Save(email, want); !errors.Is(err, privatefile.ErrDurabilityUncertain) {
				t.Fatalf("committed migration was not classified as uncertain: %v", err)
			}
			staged := keychain.values[store.migrationService()]
			if staged == "" || !committed && keychain.values[macKeychainService] != original {
				t.Fatal("interrupted migration lost protected recovery material")
			}
			restarted := &SecureFileStore{Root: store.Root, platform: "darwin", run: keychain.run}
			got, err := restarted.Load(email)
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("restarted reader lost the committed value or resurrected old fields: %v", err)
			}
			data, err := os.ReadFile(store.path(email))
			if err != nil || bytes.Contains(data, []byte(want.TOTPSecret)) || bytes.Contains(data, []byte(want.MailBody)) {
				t.Fatal("migration file missing or contains plaintext")
			}
			if err := restarted.Save(email, want); err != nil {
				t.Fatal(err)
			}
			if keychain.values[macKeychainService] != staged || keychain.values[store.migrationService()] != staged {
				t.Fatal("migration retry replaced the recovery key")
			}
			if err := restarted.Delete(email); err != nil {
				t.Fatal(err)
			}
			if len(keychain.values) != 0 {
				t.Fatal("credential deletion left migration keys")
			}
		})
	}
}

func TestDirectKeychainMigrationNeverReplacesConcurrentStagingKey(t *testing.T) {
	keychain := &memoryKeychain{values: map[string]string{macKeychainService: `{"password":"synthetic-original"}`}}
	store := testMacStore(t, keychain)
	winner := bytes.Repeat([]byte{7}, 32)
	winnerText := base64.StdEncoding.EncodeToString(winner)
	store.run = func(ctx context.Context, args []string, input string) ([]byte, error) {
		if args[0] == "add-generic-password" && keychainArgument(args, "-s") == store.migrationService() {
			if strings.Contains(strings.Join(args, " "), "-U") {
				t.Fatal("migration overwrote a competing process's key")
			}
			keychain.values[store.migrationService()] = winnerText
			return nil, errors.New("synthetic duplicate item")
		}
		return keychain.run(ctx, args, input)
	}
	want := Credentials{Password: "synthetic-replacement"}
	if err := store.Save("migration-race@example.invalid", want); err != nil {
		t.Fatal(err)
	}
	if keychain.values[macKeychainService] != winnerText {
		t.Fatal("migration ignored the competing process's stable key")
	}
	got, err := store.Load("migration-race@example.invalid")
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("concurrent migration did not produce a readable value: %v", err)
	}
}

func TestDirectKeychainInterruptedMigrationRequiresOriginalStagingKey(t *testing.T) {
	for _, damaged := range []string{"", "invalid", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{8}, 32))} {
		t.Run(damaged, func(t *testing.T) {
			const original = `{"password":"synthetic-original"}`
			keychain := &memoryKeychain{values: map[string]string{macKeychainService: original}}
			store := testMacStore(t, keychain)
			const email = "migration-key-loss@example.invalid"
			writeEncrypted(t, store.path(email), bytes.Repeat([]byte{7}, 32))
			if damaged != "" {
				keychain.values[store.migrationService()] = damaged
			}
			before, _ := os.ReadFile(store.path(email))
			if _, err := store.Load(email); err == nil || errors.Is(err, os.ErrNotExist) {
				t.Fatalf("unreadable migrated value fell back to old credentials or appeared absent: %v", err)
			}
			if err := store.Save(email, Credentials{Password: "must-not-replace"}); err == nil {
				t.Fatal("unreadable migrated value was replaced")
			}
			after, _ := os.ReadFile(store.path(email))
			if !bytes.Equal(before, after) || keychain.writes != 0 || keychain.values[macKeychainService] != original {
				t.Fatal("unreadable migration replaced key or ciphertext")
			}
		})
	}
}

func TestCredentialMigrationCannotAliasAnotherConfiguredNamespace(t *testing.T) {
	for _, operation := range []string{"delete", "migrate"} {
		t.Run(operation, func(t *testing.T) {
			keychain := &memoryKeychain{values: map[string]string{}}
			const firstService = macIsolatedServicePrefix + "namespace-owner"
			const secondService = firstService + ".credential-migration-v1"
			for _, service := range []string{firstService, secondService} {
				if err := validateMacKeychainService(service); err != nil {
					t.Fatal("fixture must use two supported namespace configurations")
				}
			}
			first := &SecureFileStore{Root: t.TempDir(), platform: "darwin", keychainService: firstService, run: keychain.run}
			second := &SecureFileStore{Root: t.TempDir(), platform: "darwin", keychainService: secondService, run: keychain.run}
			const email = "same-account@example.invalid"
			secondValue := Credentials{Password: "synthetic-second-namespace-password"}
			if err := second.Save(email, secondValue); err != nil {
				t.Fatal(err)
			}
			secondKey := keychain.values[secondService]
			if operation == "migrate" {
				keychain.values[firstService] = `{"password":"synthetic-first-namespace-password"}`
			}
			firstValue := Credentials{Password: "synthetic-first-namespace-updated"}
			if err := first.Save(email, firstValue); err != nil {
				t.Fatal(err)
			}
			if keychain.values[firstService] == secondKey {
				t.Fatal("migration reused another configured namespace's credential key")
			}
			if operation == "delete" {
				if err := first.Delete(email); err != nil {
					t.Fatal(err)
				}
			}
			got, err := second.Load(email)
			if err != nil || !reflect.DeepEqual(got, secondValue) || keychain.values[secondService] != secondKey {
				t.Fatalf("credential operation damaged another configured namespace: %v", err)
			}
		})
	}
}
