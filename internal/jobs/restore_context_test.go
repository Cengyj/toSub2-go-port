package jobs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/store"
)

type cancelRestoreVault struct {
	blockLoadAt            int32
	blockSave              bool
	entered                chan struct{}
	loads, saves, unscoped atomic.Int32
}

func (v *cancelRestoreVault) Load(string) (credentials.Credentials, error) {
	v.unscoped.Add(1)
	return credentials.Credentials{}, errors.New("recovery used an unscoped credential load")
}
func (v *cancelRestoreVault) Save(string, credentials.Credentials) error {
	v.unscoped.Add(1)
	return errors.New("recovery used an unscoped credential save")
}
func (v *cancelRestoreVault) Delete(string) error {
	v.unscoped.Add(1)
	return errors.New("recovery used an unscoped credential delete")
}
func (v *cancelRestoreVault) LoadContext(ctx context.Context, _ string) (credentials.Credentials, error) {
	if v.loads.Add(1) == v.blockLoadAt {
		close(v.entered)
		<-ctx.Done()
		return credentials.Credentials{}, ctx.Err()
	}
	return credentials.Credentials{}, os.ErrNotExist
}
func (v *cancelRestoreVault) SaveContext(ctx context.Context, _ string, _ credentials.Credentials) error {
	v.saves.Add(1)
	if v.blockSave {
		close(v.entered)
		<-ctx.Done()
		return ctx.Err()
	}
	return nil
}
func (v *cancelRestoreVault) DeleteContext(context.Context, string) error { return nil }

func TestRestoreCancelsEveryCredentialRecoveryCall(t *testing.T) {
	cases := []struct {
		name                     string
		blockLoadAt              int32
		privateName, privateJSON string
	}{
		{name: "initial_load", blockLoadAt: 1},
		{name: "private_result_load", blockLoadAt: 2},
		{name: "final_reload", blockLoadAt: 3},
		{name: "password_result_save", privateName: "password-add-result.json", privateJSON: `{"version":1,"email":"restore@example.com","password":"Fixture-password9!"}`},
		{name: "totp_result_save", privateName: "totp-setup-result.json", privateJSON: `{"version":1,"email":"restore@example.com","secret":"JBSWY3DPEHPK3PXP","activation_succeeded":true}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			st, err := store.New(root)
			if err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join(root, "restore-fixture")
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			metadata := []byte(`{"id":"restore-fixture","email":"restore@example.com","status":"interrupted"}`)
			if err := os.WriteFile(filepath.Join(dir, "job-meta.json"), metadata, 0600); err != nil {
				t.Fatal(err)
			}
			if tc.privateName != "" {
				if err := os.WriteFile(filepath.Join(dir, tc.privateName), []byte(tc.privateJSON), 0600); err != nil {
					t.Fatal(err)
				}
			}
			vault := &cancelRestoreVault{blockLoadAt: tc.blockLoadAt, blockSave: tc.privateName != "", entered: make(chan struct{})}
			m := NewManager(context.Background(), 1, st, nil)
			m.SetCredentialStore(vault)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- m.Restore(ctx) }()
			select {
			case <-vault.entered:
			case err := <-done:
				t.Fatalf("restore did not reach cancellable credential operation: %v", err)
			case <-time.After(time.Second):
				t.Fatal("credential operation was not reached")
			}
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("restore swallowed cancellation: %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("canceled credential restore did not return promptly")
			}
			if vault.unscoped.Load() != 0 {
				t.Fatal("recovery used context-free OS credential methods")
			}
			if tc.blockLoadAt > 0 && vault.loads.Load() != tc.blockLoadAt {
				t.Fatal("restore continued credential calls after cancellation")
			}
			if tc.privateName != "" {
				if vault.saves.Load() != 1 || vault.loads.Load() != 2 {
					t.Fatal("restore continued after canceled private result save")
				}
				if data, err := os.ReadFile(filepath.Join(dir, tc.privateName)); err != nil || string(data) != tc.privateJSON {
					t.Fatal("canceled recovery removed pending private result")
				}
			}
			if len(m.List()) != 0 {
				t.Fatal("canceled restore published a partial job")
			}
			if _, err := os.Stat(filepath.Join(root, "job-meta.json")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("canceled restore overwrote aggregate metadata")
			}
		})
	}
}
