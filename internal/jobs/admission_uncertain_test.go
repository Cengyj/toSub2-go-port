package jobs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/store"
)

func TestAdmissionPersistenceClassifiesUnknownDurabilitySeparately(t *testing.T) {
	for _, test := range []struct {
		err  error
		want error
	}{
		{os.ErrPermission, ErrAdmissionPersistence},
		{fmt.Errorf("fixture storage details: %w", os.ErrPermission), ErrAdmissionPersistence},
		{store.ErrDurabilityUncertain, ErrAdmissionUncertain},
		{fmt.Errorf("fixture storage details: %w: %w", store.ErrDurabilityUncertain, os.ErrPermission), ErrAdmissionUncertain},
	} {
		got := admissionPersistenceError(test.err)
		if !errors.Is(got, test.want) || (errors.Is(got, ErrAdmissionPersistence) && errors.Is(got, ErrAdmissionUncertain)) {
			t.Fatalf("write certainty was classified incorrectly: %v", got)
		}
		if strings.Contains(got.Error(), "fixture storage details") {
			t.Fatal("public admission error exposed internal storage diagnostics")
		}
	}
}

// A post-rename failure leaves a visible queued record. Preserve the existing
// startup contract, and make its implications explicit: only a new preflight
// queue with recoverable credentials can execute automatically. Existing
// authorization artifacts and missing credentials require explicit recovery.
func TestVisibleQueuedMetadataAfterUncertainWriteUsesRecoveryGuards(t *testing.T) {
	for _, kind := range []string{"new-preflight", "missing-credentials", "prior-grant", "prior-checkpoint"} {
		t.Run(kind, func(t *testing.T) {
			st, err := store.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			const id, email = "uncertain-fixture", "uncertain@example.test"
			now := time.Now().UTC()
			job := &Job{ID: id, Email: email, Status: StatusQueued, Stage: "queued", Operation: "relogin", OperationStatus: "queued", AuthorizationMethod: AuthorizationMethodBPS, HasPassword: true, CreatedAt: now, QueuedAt: now, UpdatedAt: now}
			if err := st.WriteJSON(t.Context(), "job-meta.json", map[string]any{"version": 1, "jobs": []any{jobMetadata(job)}}); err != nil {
				t.Fatal(err)
			}
			private, err := store.New(filepath.Join(st.Root, id))
			if err != nil {
				t.Fatal(err)
			}
			if kind == "prior-grant" {
				if err := private.WriteJSON(t.Context(), "sub2api-import-oauth.json", map[string]any{"type": "sub2api-data", "accounts": []any{map[string]any{"credentials": map[string]string{"email": email}}}}); err != nil {
					t.Fatal(err)
				}
			} else if kind == "prior-checkpoint" {
				if err := private.WriteJSON(t.Context(), "login-checkpoint.json", map[string]any{"version": 1, "email": email, "stage": "phone"}); err != nil {
					t.Fatal(err)
				}
			}
			vault := credentials.NewMemoryStore()
			if kind != "missing-credentials" {
				if err := vault.Save(email, credentials.Credentials{Password: "synthetic-password"}); err != nil {
					t.Fatal(err)
				}
			}
			m := NewManager(t.Context(), 1, st, nil)
			defer m.cancel()
			m.SetCredentialStore(vault)
			if err := m.Restore(t.Context()); err != nil {
				t.Fatal(err)
			}
			restored, ok := m.Get(id)
			want := map[string]string{"new-preflight": StatusQueued, "missing-credentials": "reauth_required", "prior-grant": StatusFailed, "prior-checkpoint": "resume_available"}[kind]
			if !ok || restored.Status != want || restored.AuthorizationMethod != AuthorizationMethodBPS || restored.ResultSaved != (kind == "prior-grant") {
				t.Fatalf("visible intent bypassed recovery guard: kind=%s status=%s want=%s", kind, restored.Status, want)
			}
			if (len(m.pending) == 1) != (kind == "new-preflight") || len(m.active) != 0 {
				t.Fatal("startup queued an unsafe or unqualified continuation")
			}
		})
	}
}
