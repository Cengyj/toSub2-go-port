package jobs

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/store"
)

func TestAdmissionFailureDoesNotPublishOrMutateExistingAccount(t *testing.T) {
	for _, kind := range []string{"queue", "retry", "requeue", "auto-repair", "replace-queued"} {
		t.Run(kind, func(t *testing.T) {
			st, err := store.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			m := NewManager(t.Context(), 1, st, nil)
			defer m.cancel()
			vault := &credentialWriteCounter{Store: credentials.NewMemoryStore()}
			m.SetCredentialStore(vault)
			const id, email = "admission", "admission@example.test"
			saved := credentials.Credentials{Password: "synthetic-old-password", TOTPSecret: "SYNTHETICOLDSEED"}
			if err := vault.Save(email, saved); err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			job := &Job{ID: id, Email: email, Status: StatusSucceeded, Operation: "full", OperationStatus: "succeeded", AuthorizationMethod: AuthorizationMethodChatGPT, ResultSaved: true, Attempts: 1, HasPassword: true, HasTOTPKey: true, CreatedAt: now, UpdatedAt: now, FinishedAt: &now, Input: map[string]any{"password": saved.Password, "totpSecret": saved.TOTPSecret, "mailRequestHeaders": map[string]string{"X-Fixture": "before"}}}
			if kind == "retry" {
				job.Status, job.OperationStatus = StatusFailed, "failed"
			}
			head := &Job{ID: "first", Email: "first@example.test", Status: StatusQueued}
			tail := &Job{ID: "last", Email: "last@example.test", Status: StatusQueued}
			m.jobs[id], m.jobs[head.ID], m.jobs[tail.ID] = job, head, tail
			m.pending = []*Job{head, tail}
			if kind == "replace-queued" {
				job.Status, job.OperationStatus = StatusQueued, "queued"
				m.pending = []*Job{head, job, tail}
			}
			preparation := &inputPreparation{job: job, attempts: job.Attempts}
			m.inputPreparing = map[string]*inputPreparation{id: preparation}
			before, _ := m.Get(id)
			beforePending := append([]*Job(nil), m.pending...)
			private, err := store.New(filepath.Join(st.Root, id))
			if err != nil {
				t.Fatal(err)
			}
			for name, content := range map[string]any{
				"sub2api-import-oauth.json": map[string]any{"type": "sub2api-data", "accounts": []any{map[string]any{"credentials": map[string]string{"email": email, "access_token": "synthetic-access-token"}}}},
				"login-checkpoint.json":     map[string]any{"version": 1, "email": email, "stage": "phone"},
			} {
				if err := private.WriteJSON(t.Context(), name, content); err != nil {
					t.Fatal(err)
				}
			}
			evidence := map[string][]byte{}
			for _, name := range []string{"sub2api-import-oauth.json", "login-checkpoint.json"} {
				evidence[name], err = os.ReadFile(private.Path(name))
				if err != nil {
					t.Fatal(err)
				}
			}
			m.persistLocked()
			if m.lastPersistError != nil {
				t.Fatal(m.lastPersistError)
			}
			repair := breakAdmissionMetadata(t, st)
			events, unsubscribe := m.Subscribe(4)
			defer unsubscribe()
			values := map[string]any{"password": "synthetic-new-password", "authorizationMethod": AuthorizationMethodBPS, "mailRequestHeaders": map[string]string{"X-Fixture": "after"}}
			admit := func() (*Job, error) {
				switch kind {
				case "retry":
					return m.RetryWithValues(id, values)
				case "requeue":
					return m.Requeue(id, "relogin")
				case "auto-repair":
					return m.QueueAutoRepair(id, []int{11}, "synthetic-backend")
				default:
					return m.QueueOperation(id, "relogin", values)
				}
			}
			queued, err := admit()
			if !errors.Is(err, ErrAdmissionPersistence) || queued != nil {
				t.Errorf("failed persistence returned success: queued=%t error=%v", queued != nil, err)
			}
			after, _ := m.Get(id)
			if !reflect.DeepEqual(before, after) || m.jobs[id] != job || !reflect.DeepEqual(m.pending, beforePending) || m.inputPreparing[id] != preparation {
				t.Error("failed admission changed the account, its inputs, or queue order")
			}
			if vault.saves != 1 || vault.deletes != 0 {
				t.Error("failed metadata commit wrote credentials")
			}
			if current, err := vault.Load(email); err != nil || !reflect.DeepEqual(current, saved) {
				t.Error("failed admission replaced valid historical credentials")
			}
			select {
			case <-events:
				t.Error("failed admission published an uncommitted queued event")
			default:
			}
			select {
			case <-m.wake:
				t.Error("failed admission woke the scheduler")
			default:
			}
			for name, before := range evidence {
				if after, err := os.ReadFile(private.Path(name)); err != nil || !bytes.Equal(before, after) {
					t.Error("failed admission changed durable authorization or checkpoint evidence")
				}
			}
			repair()
			queued, err = admit()
			if err != nil || queued == nil || queued.Status != StatusQueued || !queued.ResultSaved {
				t.Fatalf("cannot retry admission after disk repair: %v", err)
			}
			expected := []string{head.ID, tail.ID, id}
			if kind == "replace-queued" {
				expected = []string{head.ID, id, tail.ID}
			}
			ids := make([]string, len(m.pending))
			for i, item := range m.pending {
				ids[i] = item.ID
			}
			if !reflect.DeepEqual(ids, expected) {
				t.Fatalf("committed admission lost queue order or repeated an account: %v", ids)
			}
			var metadata struct {
				Jobs []json.RawMessage `json:"jobs"`
			}
			if err := st.ReadJSON(t.Context(), "job-meta.json", &metadata); err != nil {
				t.Fatal(err)
			}
			committed := false
			for _, raw := range metadata.Jobs {
				if got, ok := decodeLegacyJob(raw, ""); ok && got.ID == id {
					committed = got.Status == StatusQueued && got.ResultSaved && got.Operation == queued.Operation && got.AuthorizationMethod == queued.AuthorizationMethod
				}
			}
			if !committed {
				t.Fatal("published admission is missing from durable metadata")
			}
			select {
			case e := <-events:
				if e.JobID != id || e.Status != StatusQueued {
					t.Fatal("committed admission published the wrong state")
				}
			default:
				t.Fatal("committed admission did not publish its queued state")
			}
		})
	}
}

func TestNewAdmissionFailureLeavesNoRunnableJobOrCredential(t *testing.T) {
	for _, importOnly := range []bool{false, true} {
		name := "runnable"
		if importOnly {
			name = "import-only"
		}
		t.Run(name, func(t *testing.T) {
			st, err := store.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			m := NewManager(t.Context(), 1, st, nil)
			defer m.cancel()
			vault := &credentialWriteCounter{Store: credentials.NewMemoryStore()}
			m.SetCredentialStore(vault)
			m.persistLocked()
			repair := breakAdmissionMetadata(t, st)
			events, unsubscribe := m.Subscribe(4)
			defer unsubscribe()
			spec := Spec{ID: "new-fixture", Email: "new@example.test", ImportOnly: importOnly, Input: map[string]any{"password": "synthetic-password"}}
			if result, err := m.Submit(spec); err == nil || result != nil {
				t.Error("uncommitted new account returned success")
			}
			if len(m.List()) != 0 || len(m.pending) != 0 || len(events) != 0 || len(m.wake) != 0 || vault.saves != 0 {
				t.Error("failed new admission published account, execution, event, or credentials")
			}
			repair()
			if result, err := m.Submit(spec); err != nil || result == nil {
				t.Fatalf("cannot retry new account after disk repair: %v", err)
			}
			if importOnly && len(m.pending) != 0 {
				t.Fatal("successful import-only admission queued login")
			}
		})
	}
}

func breakAdmissionMetadata(t *testing.T, st *store.Store) func() {
	t.Helper()
	path, backup := st.Path("job-meta.json"), st.Path("metadata-backup.json")
	if err := os.Rename(path, backup); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	return func() {
		t.Helper()
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(backup, path); err != nil {
			t.Fatal(err)
		}
	}
}
