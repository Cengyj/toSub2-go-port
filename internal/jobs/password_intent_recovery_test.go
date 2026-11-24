package jobs

import (
	"bytes"
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/privatestate"
	"tosub2-go-port/internal/protocol"
)

func passwordIntentRecoveryFixture(t *testing.T, state string) (*Manager, *privateRecoveryReadbackVault, string, string, protocol.PasswordAddRecovery) {
	t.Helper()
	m, vault, id, path := privateRecoveryReadbackFixture(t, "add-password", StatusQueued, false)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	protector, err := credentials.NewMemoryStateProtector()
	if err != nil {
		t.Fatal(err)
	}
	if err := privatestate.Register(m.store.Root, protector); err != nil {
		t.Fatal(err)
	}
	record := protocol.PasswordAddRecovery{Version: 1, OperationID: "synthetic-password-operation", Email: "private-result@example.test", Password: "Synthetic-new-password9!", State: state}
	if state == "succeeded" {
		record.AddedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	writePasswordRecoveryIntent(t, path, record)
	return m, vault, id, path, record
}

func writePasswordRecoveryIntent(t *testing.T, path string, record protocol.PasswordAddRecovery) {
	t.Helper()
	if err := privatestate.WriteJSON(t.Context(), protocol.PasswordAddIntentPath(path), "password-add", record.Email, record.OperationID, record); err != nil {
		t.Fatal(err)
	}
}

func TestPasswordIntentOnlySuccessRecoversAfterMetadataCommit(t *testing.T) {
	for _, metadataFailure := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "metadata-failure"}[metadataFailure], func(t *testing.T) {
			m, vault, id, path, record := passwordIntentRecoveryFixture(t, "succeeded")
			original, _ := os.ReadFile(protocol.PasswordAddIntentPath(path))
			if bytes.Contains(original, []byte(record.Password)) {
				t.Fatal("intent fixture was not protected")
			}
			if metadataFailure {
				m.metadataWriter = func(context.Context, string, any) error { return errors.New("synthetic commit failure") }
				if err := m.Restore(t.Context()); err == nil {
					t.Fatal("failed metadata commit reported successful recovery")
				}
				retained, err := os.ReadFile(protocol.PasswordAddIntentPath(path))
				if err != nil || !bytes.Equal(original, retained) {
					t.Fatal("metadata failure discarded the sole successful receipt")
				}
				m.metadataWriter = nil
			}
			if err := m.Restore(t.Context()); err != nil {
				t.Fatal(err)
			}
			job, ok := m.Get(id)
			current, _ := vault.Load(record.Email)
			if !ok || job.OperationStatus != "succeeded" || job.PasswordAddOutcome != PasswordOutcomeAdded || !job.HasPassword || current.Password != record.Password || current.PendingPassword != "" || len(m.pending) != 0 {
				t.Fatal("intent-only success failed to complete locally")
			}
			for _, evidence := range []string{path, protocol.PasswordAddIntentPath(path)} {
				if _, err := os.Stat(evidence); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("confirmed recovery retained already committed evidence", err)
				}
			}
		})
	}
}

func TestPasswordIntentUnknownNeverCommitsCandidateOrQueuesReplay(t *testing.T) {
	m, vault, id, path, record := passwordIntentRecoveryFixture(t, "may_have_been_sent")
	before, _ := vault.memoryVault.Load(record.Email)
	if err := m.Restore(t.Context()); err != nil {
		t.Fatal(err)
	}
	job, _ := m.Get(id)
	after, _ := vault.memoryVault.Load(record.Email)
	if job.Status != StatusFailed || job.OperationStatus != "failed" || !strings.Contains(job.PasswordAddError, "PASSWORD_ADD_RESULT_UNKNOWN") || strings.Contains(job.Prompt, "重试安全保存") || vault.saves != 0 || !reflect.DeepEqual(before, after) || len(m.pending) != 0 || job.passwordCleanup {
		t.Fatal("unknown dispatch changed credentials, queued replay or offered local success")
	}
	if _, err := os.Stat(protocol.PasswordAddIntentPath(path)); err != nil {
		t.Fatal("unknown dispatch lost evidence", err)
	}
}

func TestPasswordIntentRecoveryRejectsSupersededCredentialAndEvidence(t *testing.T) {
	for _, scenario := range []string{"credential", "intent", "result"} {
		t.Run(scenario, func(t *testing.T) {
			m, vault, id, path, record := passwordIntentRecoveryFixture(t, "succeeded")
			if scenario == "credential" {
				current, _ := vault.memoryVault.Load(record.Email)
				current.Password, current.PendingPassword = "Synthetic-newer-client-password9!", ""
				if err := vault.memoryVault.Save(record.Email, current); err != nil {
					t.Fatal(err)
				}
			}
			before, _ := vault.memoryVault.Load(record.Email)
			acquire := func(string) (func() error, error) {
				if scenario == "intent" {
					record.OperationID = "later-synthetic-operation"
					writePasswordRecoveryIntent(t, path, record)
				}
				if scenario == "result" {
					if err := protocol.WritePrivateJSON(path, map[string]any{"version": 1, "email": record.Email, "password": record.Password, "operation_id": record.OperationID, "added_at": record.AddedAt}); err != nil {
						t.Fatal(err)
					}
				}
				return func() error { return nil }, nil
			}
			job := &Job{ID: id, Email: record.Email, Operation: "add-password"}
			recovered, err := recoverPrivateResult(t.Context(), m.store.Root, job, vault, acquire)
			after, _ := vault.memoryVault.Load(record.Email)
			if err == nil || !recovered.passwordError || recovered.password || job.passwordCleanup || vault.saves != 0 || !reflect.DeepEqual(before, after) {
				t.Fatal("superseded evidence overwrote current credentials")
			}
		})
	}
}
