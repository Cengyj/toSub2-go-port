package jobs

import (
	"bytes"
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

func TestRestoreCancellationDoesNotPublishEarlierAccounts(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	metadata := []byte(`{"version":1,"jobs":[{"id":"first","email":"first@example.test","status":"queued","queued_at":"2026-01-01T00:00:00Z"},{"id":"second","email":"second@example.test","status":"queued","queued_at":"2026-01-02T00:00:00Z"}]}`)
	metadataPath := filepath.Join(st.Root, "job-meta.json")
	if err := os.WriteFile(metadataPath, metadata, 0600); err != nil {
		t.Fatal(err)
	}
	// Each record requires three credential loads. Block the first call for
	// the next record regardless of the order of map iteration.
	vault := &cancelRestoreVault{blockLoadAt: 4, entered: make(chan struct{})}
	m := NewManager(t.Context(), 1, st, nil)
	m.SetCredentialStore(vault)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- m.Restore(ctx) }()
	select {
	case <-vault.entered:
	case err := <-done:
		t.Fatalf("restore ended before its second account: %v", err)
	case <-time.After(time.Second):
		t.Fatal("restore did not reach the second account")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("restore cancellation = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("restore did not stop after cancellation")
	}
	if len(m.List()) != 0 || len(m.pending) != 0 {
		t.Fatal("canceled restore published an incomplete account set or queue")
	}
	if got, err := os.ReadFile(metadataPath); err != nil || !bytes.Equal(got, metadata) {
		t.Fatal("canceled restore changed durable metadata")
	}
	// A caller may retry initialization on the same idle manager. The saved
	// queue order must still apply when the full batch is finally accepted.
	m.SetCredentialStore(&memoryVault{})
	if err := m.Restore(t.Context()); err != nil {
		t.Fatalf("restore could not be retried after cancellation: %v", err)
	}
	if len(m.List()) != 2 || len(m.pending) != 2 || m.pending[0].ID != "first" || m.pending[1].ID != "second" {
		t.Fatal("retried restore did not retain both accounts in FIFO order")
	}
}

type restoreCommitFaultVault struct {
	memoryVault
	loads        int
	failAt       int
	beforeCommit func() error
}

func (v *restoreCommitFaultVault) Load(email string) (credentials.Credentials, error) {
	v.loads++
	failAt := v.failAt
	if failAt == 0 {
		failAt = 3
	}
	if v.loads == failAt {
		if err := v.beforeCommit(); err != nil {
			return credentials.Credentials{}, err
		}
	}
	return v.memoryVault.Load(email)
}

func TestRestoreMetadataFailureDoesNotPublishRunnableAccounts(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	metadata := []byte(`{"version":1,"jobs":[{"id":"queued","email":"commit@example.test","status":"queued"}]}`)
	path, backup := filepath.Join(st.Root, "job-meta.json"), filepath.Join(st.Root, "original-metadata.json")
	if err := os.WriteFile(path, metadata, 0600); err != nil {
		t.Fatal(err)
	}
	var injectErr error
	vault := &restoreCommitFaultVault{beforeCommit: func() error {
		// A directory replacing the destination makes the atomic rename fail
		// consistently on supported platforms, including privileged test runs.
		injectErr = os.Rename(path, backup)
		if injectErr == nil {
			injectErr = os.Mkdir(path, 0700)
		}
		return injectErr
	}}
	m := NewManager(t.Context(), 1, st, nil)
	m.SetCredentialStore(vault)
	if err := m.Restore(t.Context()); err == nil {
		t.Fatal("restore accepted a failed metadata commit")
	}
	if injectErr != nil || vault.loads != 3 {
		t.Fatalf("fixture did not reach metadata commit: loads=%d, error=%v", vault.loads, injectErr)
	}
	if len(m.List()) != 0 || len(m.pending) != 0 {
		t.Fatal("uncommitted recovery published runnable accounts")
	}
	if m.lastPersistError == nil {
		t.Fatal("metadata failure was not retained for diagnostics")
	}
	if got, err := os.ReadFile(backup); err != nil || !bytes.Equal(got, metadata) {
		t.Fatal("failed recovery modified the prior metadata")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(backup, path); err != nil {
		t.Fatal(err)
	}
	m.SetCredentialStore(&memoryVault{})
	if err := m.Restore(t.Context()); err != nil {
		t.Fatalf("restore retry after disk recovery failed: %v", err)
	}
	if len(m.List()) != 1 || len(m.pending) != 1 || m.lastPersistError != nil {
		t.Fatal("disk recovery did not commit the complete queue")
	}
}

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
