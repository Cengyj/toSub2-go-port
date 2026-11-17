package jobs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/privatefile"
	"tosub2-go-port/internal/store"
)

type credentialCommitWarningStore struct {
	*credentials.MemoryStore
	failure error
	writes  int
}

func (s *credentialCommitWarningStore) LoadContext(_ context.Context, email string) (credentials.Credentials, error) {
	return s.MemoryStore.Load(email)
}
func (s *credentialCommitWarningStore) DeleteContext(_ context.Context, email string) error {
	return s.MemoryStore.Delete(email)
}
func (s *credentialCommitWarningStore) SaveContext(_ context.Context, email string, value credentials.Credentials) error {
	s.writes++
	if s.failure == nil || errors.Is(s.failure, privatefile.ErrDurabilityUncertain) {
		if err := s.MemoryStore.Save(email, value); err != nil {
			return err
		}
	}
	return s.failure
}

func TestCredentialWarningDistinguishesCommittedUnknownFromFailedSave(t *testing.T) {
	for _, operation := range []string{"import", "update"} {
		for _, uncertain := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/uncertain=%t", operation, uncertain), func(t *testing.T) {
				root, err := store.New(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				manager := NewManager(t.Context(), 1, root, nil)
				vault := &credentialCommitWarningStore{MemoryStore: credentials.NewMemoryStore()}
				manager.SetCredentialStore(vault)
				const email = "credential-warning@example.invalid"
				var job *Job
				if operation == "update" {
					job, err = manager.Submit(Spec{ID: "credential-warning", Email: email, ImportOnly: true, Input: map[string]any{"password": "synthetic-old", "totpSecret": "synthetic-retained-seed"}})
					if err != nil {
						t.Fatal(err)
					}
				}
				cause := credentials.ErrUnavailable
				if uncertain {
					cause = privatefile.ErrDurabilityUncertain
				}
				vault.failure = fmt.Errorf("synthetic-private-failure: %w", cause)
				vault.writes = 0
				if operation == "import" {
					job, err = manager.Submit(Spec{ID: "credential-warning", Email: email, ImportOnly: true, Input: map[string]any{"password": "synthetic-new"}})
				} else {
					job, err = manager.Update(job.ID, map[string]any{"password": "synthetic-new"})
				}
				if err != nil || job == nil || job.Status != StatusImported || job.OperationStatus != "" || manager.QueuePosition(job.ID) != 0 || vault.writes != 1 {
					t.Fatalf("credential warning changed admission, queued authorization, or replayed the write: %v", err)
				}
				if job.Input["password"] != "synthetic-new" || strings.Contains(job.LastError, "synthetic-") {
					t.Fatal("credential warning discarded current input or disclosed private material")
				}
				if uncertain {
					if !strings.Contains(job.LastError, "待核实") || strings.Contains(job.LastError, "仅保留于内存") {
						t.Fatal("committed but unconfirmed credentials were reported as a definite memory-only failure")
					}
				} else if job.LastError != "系统凭据保存失败，登录资料仅保留于内存" {
					t.Fatal("definite credential failure was changed to an unknown result")
				}
				persisted, loadErr := vault.Load(email)
				if uncertain {
					if loadErr != nil || persisted.Password != "synthetic-new" || operation == "update" && persisted.TOTPSecret != "synthetic-retained-seed" {
						t.Fatal("unconfirmed saved value or omitted existing credential was lost")
					}
				} else if operation == "update" {
					if loadErr != nil || persisted.Password != "synthetic-old" {
						t.Fatal("failed update changed the previously saved credential")
					}
				} else if !errors.Is(loadErr, os.ErrNotExist) {
					t.Fatal("failed first import unexpectedly saved credentials")
				}
			})
		}
	}
}
