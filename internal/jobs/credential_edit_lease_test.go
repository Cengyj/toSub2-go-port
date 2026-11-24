package jobs

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"tosub2-go-port/internal/accountlock"
	"tosub2-go-port/internal/credentials"
)

type credentialDeleteReadbackVault struct {
	*credentials.MemoryStore
	mode    string
	deletes int
	saves   int
}

func (v *credentialDeleteReadbackVault) Save(email string, c credentials.Credentials) error {
	v.saves++
	return v.MemoryStore.Save(email, c)
}

func (v *credentialDeleteReadbackVault) Delete(email string) error {
	v.deletes++
	if v.mode == "retained-empty" {
		return v.MemoryStore.Save(email, credentials.Credentials{})
	}
	return v.MemoryStore.Delete(email)
}

func (v *credentialDeleteReadbackVault) Load(email string) (credentials.Credentials, error) {
	if v.deletes > 0 && v.mode == "read-error" {
		return credentials.Credentials{}, os.ErrPermission
	}
	return v.MemoryStore.Load(email)
}

func TestCredentialEditClearRequiresConfirmedAbsence(t *testing.T) {
	for _, mode := range []string{"removed", "retained-empty", "read-error"} {
		t.Run(mode, func(t *testing.T) {
			const email = "clear-readback@example.test"
			vault := &credentialDeleteReadbackVault{MemoryStore: credentials.NewMemoryStore(), mode: mode}
			m, _ := sharedVaultWriterManager(t, vault, "https://clear-readback.example.test")
			job, err := m.Submit(Spec{Email: email, ImportOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			if err := vault.MemoryStore.Save(email, credentials.Credentials{Password: "synthetic-password"}); err != nil {
				t.Fatal(err)
			}
			updated, err := m.Update(job.ID, map[string]any{"password": ""})
			if err != nil {
				t.Fatal(err)
			}
			if vault.deletes != 1 || vault.saves != 0 {
				t.Fatalf("clear replayed or substituted a write: deletes=%d saves=%d", vault.deletes, vault.saves)
			}
			if mode == "removed" {
				if updated.LastError != "" {
					t.Fatal("confirmed removal reported a warning", updated.LastError)
				}
			} else if !strings.Contains(updated.LastError, "清除后读回未确认") {
				t.Fatal("unconfirmed deletion reported success", updated.LastError)
			}
		})
	}
}

func TestCredentialEditRetainsBorrowedQueryOwnershipThroughReadback(t *testing.T) {
	const origin, email = "https://credential-query-edit.example.test", "query-edit@example.test"
	vault := &sharedWriterGatedVault{MemoryStore: credentials.NewMemoryStore()}
	m, _ := sharedVaultWriterManager(t, vault, origin)
	job, err := m.Submit(Spec{Email: email, ImportOnly: true, Input: map[string]any{"password": "synthetic-password"}})
	if err != nil {
		t.Fatal(err)
	}
	query, err := m.ReserveAccountQuery(t.Context(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer query.Release()
	gate := &sharedWriterSaveGate{entered: make(chan struct{}), proceed: make(chan struct{})}
	defer gate.release()
	vault.mu.Lock()
	vault.gate = gate
	vault.mu.Unlock()
	edited := make(chan error, 1)
	go func() {
		_, err := m.Update(job.ID, map[string]any{"mailRequestBody": "explicit-new-body"})
		edited <- err
	}()
	select {
	case <-gate.entered:
	case err := <-edited:
		t.Fatal("local query could not lend its lease to a superseding edit", err)
	case <-time.After(time.Second):
		t.Fatal("edit did not reach protected storage")
	}
	if query.Context().Err() == nil {
		t.Error("edit left the previous query eligible")
	}
	released := make(chan struct{})
	go func() { query.Release(); close(released) }()
	// Cleanup may start at any point, but its OS lease cannot disappear while
	// the credential merge/write is in flight under borrowed ownership.
	if release, err := accountlock.Acquire(origin, email); !errors.Is(err, accountlock.ErrBusy) {
		if release != nil {
			_ = release()
		}
		t.Error("query cleanup released ownership underneath the edit", err)
	}
	gate.release()
	select {
	case err := <-edited:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("edit failed to finish after storage resumed")
	}
	select {
	case <-released:
	case <-time.After(time.Second):
		t.Fatal("query cleanup did not finish after edit readback")
	}
	saved, err := vault.Load(email)
	if err != nil || saved.Password != "synthetic-password" || saved.MailBody != "explicit-new-body" {
		t.Fatal("edit did not preserve omitted credentials", err)
	}
	release, err := accountlock.Acquire(origin, email)
	if err != nil {
		t.Fatal("completed edit leaked account ownership", err)
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
}
