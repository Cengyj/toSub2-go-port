package jobs

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/store"
)

// These combinations follow syncCompletedOutputs/restoredOutputOperationState
// in the original console, including its interrupted private-result fixtures.
func TestLegacyRestoreArtifactAndOperationCombinations(t *testing.T) {
	cases := []struct {
		name, status, operation, result, want string
		output, checkpoint                    bool
	}{
		{name: "old_output_failed_login", status: StatusFailed, operation: "relogin", output: true, want: StatusFailed},
		{name: "old_output_interrupted_login", status: "email_otp", operation: "relogin", output: true, want: StatusFailed},
		{name: "old_output_interrupted_refresh", status: "refreshing", operation: "refresh", output: true, want: StatusFailed},
		{name: "old_output_checkpoint_interrupted_login", status: "phone_otp", operation: "relogin", output: true, checkpoint: true, want: StatusFailed},
		{name: "old_output_no_metadata_state", output: true, checkpoint: true, want: StatusSucceeded},
		{name: "old_output_queued_refresh", status: StatusQueued, operation: "refresh", output: true, want: StatusFailed},
		{name: "old_output_interrupted_password", status: "password_add_starting", operation: "add_password", output: true, want: StatusSucceeded},
		{name: "old_output_password_result", status: "password_add_starting", operation: "add_password", result: "password", output: true, want: StatusSucceeded},
		{name: "old_output_totp_result", status: "totp_starting", operation: "setup_2fa", result: "totp", output: true, want: StatusSucceeded},
		{name: "checkpoint_password_result", status: "password_add_starting", operation: "add_password", result: "password", checkpoint: true, want: "resume_available"},
		{name: "checkpoint_totp_result", status: "totp_starting", operation: "setup_2fa", result: "totp", checkpoint: true, want: "resume_available"},
		{name: "checkpoint_failed_private_result", status: StatusFailed, operation: "setup_2fa", result: "totp", checkpoint: true, want: "resume_available"},
		{name: "checkpoint_queued_login", status: StatusQueued, checkpoint: true, want: "resume_available"},
		{name: "checkpoint_old_totp_stage", status: "totp_starting", checkpoint: true, want: "resume_available"},
		{name: "checkpoint_failed_login", status: StatusFailed, checkpoint: true, want: StatusFailed},
		{name: "checkpoint_canceled_login", status: StatusCanceled, checkpoint: true, want: StatusCanceled},
		{name: "no_checkpoint_interrupted_otp", status: "email_otp", want: StatusFailed},
		{name: "no_checkpoint_old_password_stage", status: "password_add_starting", want: StatusFailed},
		{name: "no_checkpoint_queued_login", status: StatusQueued, want: StatusQueued},
		{name: "no_checkpoint_starting_login", status: "starting", want: StatusQueued},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root, err := store.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			dir, err := store.New(filepath.Join(root.Root, tc.name))
			if err != nil {
				t.Fatal(err)
			}
			email := tc.name + "@example.test"
			write := func(name string, v any) {
				t.Helper()
				if err := dir.WriteJSON(context.Background(), name, v); err != nil {
					t.Fatal(err)
				}
			}
			write("job-meta.json", map[string]any{"email": email, "status": tc.status, "last_operation_type": tc.operation, "prompt": "old prompt", "last_error": "old safe error"})
			if tc.output {
				// The old restore smoke deliberately accepts a valid v1 envelope
				// without asserting that usable credentials remain in its output.
				write("sub2api-import-oauth.json", map[string]any{"type": "sub2api-data", "version": 1, "accounts": []any{map[string]any{"name": "oauth---" + email, "credentials": map[string]any{"email": email}}}})
			}
			if tc.checkpoint {
				write("login-checkpoint.json", map[string]any{"version": 1, "email": email, "stage": "email_verified", "oauth": map[string]any{"phone": "+12025550171"}})
			}
			if tc.result == "password" {
				write("password-add-result.json", map[string]any{"version": 1, "email": email, "password": "Synthetic-Password9!", "added_at": "2026-09-25T12:00:00Z"})
			}
			if tc.result == "totp" {
				write("totp-setup-result.json", map[string]any{"version": 1, "email": email, "secret": "JBSWY3DPEHPK3PXP", "activation_succeeded": true})
			}
			m := NewManager(context.Background(), 1, root, nil)
			vault := credentials.NewMemoryStore()
			m.SetCredentialStore(vault)
			if err := m.Restore(context.Background()); err != nil {
				t.Fatal(err)
			}
			j, ok := m.Get(tc.name)
			if !ok || j.Status != tc.want || j.ResultSaved != tc.output {
				t.Fatalf("got present=%v status=%s downloadable=%v, want %s/%v", ok, j.Status, j.ResultSaved, tc.want, tc.output)
			}
			if tc.want == StatusQueued {
				if len(m.pending) != 1 {
					t.Fatal("queued work not recovered")
				}
			} else if len(m.pending) != 0 {
				t.Fatal("restoration would replay consumed or interrupted work")
			}
			if tc.result != "" {
				c, err := vault.Load(email)
				if err != nil {
					t.Fatal(err)
				}
				if tc.result == "password" && (c.Password != "Synthetic-Password9!" || !j.HasPassword) {
					t.Fatal("password result lost")
				}
				if tc.result == "totp" && (c.TOTPSecret != "JBSWY3DPEHPK3PXP" || !j.HasTOTPKey) {
					t.Fatal("TOTP result lost")
				}
			}
		})
	}
}

func TestLegacyRestorePreservesOperationTimestampAndArtifactTimes(t *testing.T) {
	root, _ := store.New(t.TempDir())
	dir, _ := store.New(filepath.Join(root.Root, "historical"))
	completed := time.Date(2026, 8, 12, 8, 0, 0, 0, time.UTC)
	operation := completed.Add(time.Hour)
	updated := operation.Add(time.Minute)
	if err := dir.WriteJSON(context.Background(), "job-meta.json", map[string]any{"email": "historical@example.test", "status": StatusFailed, "updated_at": updated, "last_operation_at": operation, "last_operation_type": "relogin"}); err != nil {
		t.Fatal(err)
	}
	if err := dir.WriteJSON(context.Background(), "sub2api-import-oauth.json", map[string]any{"type": "sub2api-data", "version": 1, "accounts": []any{map[string]any{"credentials": map[string]any{"email": "historical@example.test"}}}}); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(dir.Path("sub2api-import-oauth.json"), completed, completed); err != nil {
		t.Fatal(err)
	}
	m := NewManager(context.Background(), 1, root, nil)
	if err := m.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	j, _ := m.Get("historical")
	metadata := jobMetadata(j)
	data, _ := json.Marshal(metadata)
	var decoded map[string]any
	_ = json.Unmarshal(data, &decoded)
	if decoded["last_operation_at"] != operation.Format(time.RFC3339) {
		t.Fatal("restoration discarded independent operation timestamp")
	}
	if !j.CreatedAt.Equal(completed) || j.FinishedAt == nil || !j.FinishedAt.Equal(completed) || !j.UpdatedAt.Equal(updated) {
		t.Fatal("restoration replaced historical artifact times with server restart time")
	}
}
