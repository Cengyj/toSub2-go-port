package jobs

import (
	"reflect"
	"testing"
	"time"

	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/store"
)

func TestAccountUpdateFailurePreservesInputsAndDoesNotRestart(t *testing.T) {
	for _, kind := range []string{"direct", "duplicate-active", "duplicate-import"} {
		t.Run(kind, func(t *testing.T) {
			st, err := store.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			m := NewManager(t.Context(), 1, st, nil)
			defer m.cancel()
			vault := &credentialWriteCounter{Store: credentials.NewMemoryStore()}
			m.SetCredentialStore(vault)
			now := time.Now().UTC()
			job := &Job{ID: "update-fixture", Email: "update@example.test", Status: StatusSucceeded, Operation: "full", LastOperationType: "initial_authorization", LastOperationAt: now, AuthorizationMethod: AuthorizationMethodChatGPT, ResultSaved: true, HasPassword: true, Input: map[string]any{"password": "synthetic-old-password"}}
			m.jobs[job.ID] = job
			if err := vault.Save(job.Email, credentials.Credentials{Password: "synthetic-old-password"}); err != nil {
				t.Fatal(err)
			}
			canceled := false
			if kind == "duplicate-active" {
				job.Status = "phone"
				m.active[job.ID] = func() { canceled = true }
			}
			before, _ := m.Get(job.ID)
			m.persistLocked()
			repair := breakAdmissionMetadata(t, st)
			values := map[string]any{"password": "synthetic-new-password"}
			update := func() (*Job, error) {
				if kind == "direct" {
					return m.Update(job.ID, values)
				}
				result, created, err := m.SubmitUniqueEmail(Spec{Email: job.Email, ImportOnly: kind == "duplicate-import", Input: values})
				if created {
					t.Error("duplicate account update created a new account")
				}
				return result, err
			}
			if result, err := update(); err == nil || result != nil {
				t.Error("failed persistence accepted account changes")
			}
			after, _ := m.Get(job.ID)
			if !reflect.DeepEqual(before, after) || canceled || m.restart[job.ID] || vault.saves != 1 {
				t.Error("failed update changed the account, credentials, or live session")
			}
			repair()
			if result, err := update(); err != nil || result == nil || result.Input["password"] != "synthetic-new-password" {
				t.Fatalf("cannot retry account update after disk repair: %v", err)
			}
			if kind == "duplicate-active" {
				if !canceled || !m.restart[job.ID] {
					t.Fatal("committed credentials did not retain existing restart semantics")
				}
			} else if canceled || len(m.pending) != 0 {
				t.Fatal("account-only update scheduled login")
			}
		})
	}
}
