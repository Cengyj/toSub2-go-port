package jobs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/store"
)

type credentialWriteCounter struct {
	credentials.Store
	saves, deletes int
}

func (v *credentialWriteCounter) Save(email string, value credentials.Credentials) error {
	v.saves++
	return v.Store.Save(email, value)
}
func (v *credentialWriteCounter) Delete(email string) error {
	v.deletes++
	return v.Store.Delete(email)
}

func TestEmptyNewCredentialRecordIsNotSavedAndExplicitClearsDeleteIt(t *testing.T) {
	m := NewManager(context.Background(), 1, nil, nil)
	vault := &credentialWriteCounter{Store: credentials.NewMemoryStore()}
	m.SetCredentialStore(vault)
	j, err := m.Submit(Spec{Email: "empty@example.com", Input: map[string]any{"proxyUrl": ""}})
	if err != nil || vault.saves != 0 || vault.deletes != 0 {
		t.Fatal("new empty input created a secure-store record", err)
	}
	if _, err := m.Update(j.ID, map[string]any{"password": "Fixture9!"}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Update(j.ID, map[string]any{"proxyUrl": ""}); err != nil {
		t.Fatal(err)
	}
	stored, err := vault.Load(j.Email)
	if err != nil || stored.Password != "Fixture9!" {
		t.Fatal("empty proxy cleared another saved field")
	}
	if _, err := m.Update(j.ID, map[string]any{"password": ""}); err != nil {
		t.Fatal(err)
	}
	if _, err := vault.Load(j.Email); !errors.Is(err, os.ErrNotExist) || vault.deletes != 1 {
		t.Fatal("all explicit clears retained an empty credential record")
	}
	if err := vault.Save(j.Email, credentials.Credentials{PendingTOTPSecret: "JBSWY3DPEHPK3PXP"}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Update(j.ID, map[string]any{"password": ""}); err != nil {
		t.Fatal(err)
	}
	stored, err = vault.Load(j.Email)
	if err != nil || stored.PendingTOTPSecret == "" {
		t.Fatal("explicit clear lost pending enrollment recovery")
	}
}

func TestCredentialUpdateWaitsForOldSessionBeforeRestart(t *testing.T) {
	m := NewManager(context.Background(), 20, nil, nil)
	started := make(chan string, 2)
	cleanup := make(chan struct{})
	canceled := make(chan struct{})
	m.SetRunner(func(ctx context.Context, job *Job, _ func(JobEvent)) error {
		password, _ := job.Input["password"].(string)
		started <- password
		if password == "old-password" {
			<-ctx.Done()
			close(canceled)
			<-cleanup
		}
		return nil
	})
	m.Start()
	defer func() {
		select {
		case <-cleanup:
		default:
			close(cleanup)
		}
		_ = m.Shutdown(context.Background())
	}()
	j, _, err := m.SubmitUniqueEmail(Spec{Email: "same@example.com", Input: map[string]any{"password": "old-password"}})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("old session did not start")
	}
	if _, created, err := m.SubmitUniqueEmail(Spec{Email: " SAME@EXAMPLE.COM ", Input: map[string]any{"password": "new-password"}}); err != nil || created {
		t.Fatalf("duplicate update failed: created=%v err=%v", created, err)
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("old session not canceled")
	}
	select {
	case <-started:
		t.Fatal("new session overlapped old session cleanup")
	case <-time.After(30 * time.Millisecond):
	}
	close(cleanup)
	select {
	case password := <-started:
		if password != "new-password" {
			t.Fatal("stale login input")
		}
	case <-time.After(time.Second):
		t.Fatal("updated session did not restart")
	}
	got := waitForStatus(t, m, j.ID, StatusSucceeded)
	if got.Attempts != 2 || len(m.List()) != 1 {
		t.Fatal("duplicate job or lost restart attempt")
	}
}

func TestCancelWinsWhenRunnerReturnsNil(t *testing.T) {
	m := NewManager(context.Background(), 1, nil, nil)
	started := make(chan struct{})
	m.SetRunner(func(ctx context.Context, _ *Job, _ func(JobEvent)) error { close(started); <-ctx.Done(); return nil })
	m.Start()
	defer m.Shutdown(context.Background())
	j, _ := m.Submit(Spec{Email: "cancel@example.com"})
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("cancel fixture did not start")
	}
	m.Cancel(j.ID)
	waitForStatus(t, m, j.ID, StatusCanceled)
}

func TestDeleteAccountsWaitsForCleanupAndRemovesAllEmailArtifacts(t *testing.T) {
	root := t.TempDir()
	st, _ := store.New(root)
	m := NewManager(context.Background(), 1, st, nil)
	vault := credentials.NewMemoryStore()
	m.SetCredentialStore(vault)
	started, cleaning, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	m.SetRunner(func(ctx context.Context, _ *Job, _ func(JobEvent)) error {
		close(started)
		<-ctx.Done()
		close(cleaning)
		<-release
		return ctx.Err()
	})
	j, _ := m.Submit(Spec{ID: "current", Email: "delete@example.com", Input: map[string]any{"password": "fixture-password"}})
	_, _ = m.Submit(Spec{ID: "legacy", Email: "DELETE@example.com"})
	for _, id := range []string{"current", "legacy"} {
		dir := filepath.Join(root, id)
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "login-checkpoint.json"), []byte(`{}`), 0600); err != nil {
			t.Fatal(err)
		}
	}
	m.Start()
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
		_ = m.Shutdown(context.Background())
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("delete fixture did not start")
	}
	done := make(chan error, 1)
	go func() {
		n, err := m.DeleteAccounts(context.Background(), []string{j.ID})
		if err == nil && n != 1 {
			err = errors.New("wrong email deletion count")
		}
		done <- err
	}()
	select {
	case <-cleaning:
	case <-time.After(time.Second):
		t.Fatal("deletion did not cancel active runner")
	}
	if _, err := os.Stat(filepath.Join(root, j.ID)); err != nil {
		t.Fatal("files removed before cleanup")
	}
	if _, _, err := m.SubmitUniqueEmail(Spec{Email: j.Email}); err == nil {
		t.Fatal("deleting email accepted new job")
	}
	if _, err := m.QueueOperation(j.ID, "relogin", nil); err == nil {
		t.Fatal("deleting job requeued")
	}
	if m.Input(j.ID, map[string]any{"phone_otp": "123456"}) {
		t.Fatal("deleting job accepted input")
	}
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("deletion did not finish")
	}
	if len(m.List()) != 0 {
		t.Fatal("legacy duplicate survived deletion")
	}
	for _, id := range []string{"current", "legacy"} {
		if _, err := os.Stat(filepath.Join(root, id)); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("private directory survived deletion")
		}
	}
	if _, err := vault.Load(j.Email); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("secure credentials survived deletion")
	}
	restored := NewManager(context.Background(), 1, st, nil)
	if err := restored.Restore(context.Background()); err != nil || len(restored.List()) != 0 {
		t.Fatal("deleted task was restored", err)
	}
}

func TestUpdatesAreAtomicAndExplicitClearsResetCredentialFlags(t *testing.T) {
	m := NewManager(context.Background(), 1, nil, nil)
	j, _ := m.Submit(Spec{Email: "fixed@example.com", Input: map[string]any{"password": "old", "totpSecret": "JBSWY3DPEHPK3PXP", "proxyUrl": "http://proxy.example:8080"}})
	if _, err := m.Update(j.ID, map[string]any{"email": "other@example.com", "password": "changed"}); err == nil {
		t.Fatal("email identity was changed")
	}
	input, _ := m.InputCopy(j.ID)
	if input["password"] != "old" {
		t.Fatal("invalid update partially mutated input")
	}
	got, err := m.Update(j.ID, map[string]any{"password": "", "totpSecret": "", "proxyUrl": ""})
	if err != nil || got.HasPassword || got.HasTOTPKey || got.ProxyConfigured || got.LoginMode != "email_otp" {
		t.Fatal("explicit credential clearing failed", err)
	}
	if err := m.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if m.Input(j.ID, map[string]any{"email_otp": "123456"}) {
		t.Fatal("shutdown manager accepted input")
	}
}
