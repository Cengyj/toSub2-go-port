package credentials

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestMacKeychainLoadCancellationDiscardsCredentials(t *testing.T) {
	for _, direct := range []bool{false, true} {
		name := "AESGCM"
		if direct {
			name = "LegacyDirectJSON"
		}
		t.Run(name, func(t *testing.T) {
			keychain := &memoryKeychain{values: map[string]string{}}
			store := testMacStore(t, keychain)
			const email = "cancel-fixture@example.test"
			if direct {
				keychain.values[macKeychainService] = `{"password":"synthetic-cancel-password","totpSecret":"SYNTHETIC-SEED"}`
			} else if err := store.Save(email, Credentials{Password: "synthetic-cancel-password", TOTPSecret: "SYNTHETIC-SEED"}); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			called := false
			store.run = func(ctx context.Context, args []string, input string) ([]byte, error) {
				value, err := keychain.run(ctx, args, input)
				if err == nil {
					called = true
					cancel()
				}
				return value, err
			}
			value, err := store.LoadContext(ctx, email)
			if !called || !errors.Is(err, context.Canceled) {
				t.Fatalf("Keychain result outlived canceled read: called=%t error=%v", called, err)
			}
			if !reflect.DeepEqual(value, Credentials{}) {
				t.Fatal("canceled Keychain read returned credential material")
			}
		})
	}
}
