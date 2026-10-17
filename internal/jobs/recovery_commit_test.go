package jobs

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/store"
)

func TestPrivateRecoveryEvidenceSurvivesMetadataFailure(t *testing.T) {
	for _, kind := range []string{"password", "setup-2fa", "reset-2fa"} {
		for _, live := range []bool{false, true} {
			name := kind + map[bool]string{false: "/startup", true: "/discovery"}[live]
			t.Run(name, func(t *testing.T) {
				st, err := store.New(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				const id, email = "recover", "recover@example.test"
				metadata := map[string]any{"id": id, "email": email, "operation": kind, "status": "failed"}
				result := map[string]any{"version": 1, "email": email}
				filename, value := "totp-setup-result.json", "SYNTHETICNEWSEED"
				saved := credentials.Credentials{TOTPSecret: "SYNTHETICOLDSEED", Password: "Synthetic-old-password9!"}
				failAt := 3
				if kind == "password" {
					metadata["operation"] = "add-password"
					filename, value = "password-add-result.json", "Synthetic-new-password9!"
					result["password"] = value
				} else {
					result["secret"], result["activation_succeeded"] = value, true
					if kind == "reset-2fa" {
						result["reset_totp_requested"], result["activation_verified"], result["old_factor_invalidated"] = true, true, true
						result["reset_totp_status"], result["reset_totp_phase"] = "activated", "activation_verified"
						saved.TOTPResetPending, failAt = true, 4
					}
				}
				private, err := store.New(filepath.Join(st.Root, id))
				if err != nil {
					t.Fatal(err)
				}
				if err := private.WriteJSON(t.Context(), "job-meta.json", metadata); err != nil {
					t.Fatal(err)
				}
				if err := private.WriteJSON(t.Context(), filename, result); err != nil {
					t.Fatal(err)
				}
				original, err := os.ReadFile(private.Path(filename))
				if err != nil {
					t.Fatal(err)
				}
				if err := st.WriteJSON(t.Context(), "job-meta.json", map[string]any{"version": 1, "jobs": []any{metadata}}); err != nil {
					t.Fatal(err)
				}
				path, backup := st.Path("job-meta.json"), st.Path("original-metadata.json")
				var injectErr error
				vault := &restoreCommitFaultVault{failAt: failAt, beforeCommit: func() error {
					injectErr = os.Rename(path, backup)
					if injectErr == nil {
						injectErr = os.Mkdir(path, 0700)
					}
					return injectErr
				}}
				if err := vault.Save(email, saved); err != nil {
					t.Fatal(err)
				}
				m := NewManager(t.Context(), 1, st, nil)
				m.SetCredentialStore(vault)
				recover := func() error {
					if live {
						return m.SyncOutputs(t.Context(), true)
					}
					return m.Restore(t.Context())
				}
				if err := recover(); err == nil || injectErr != nil {
					t.Fatalf("metadata failure not reached: restore=%v, injection=%v", err, injectErr)
				}
				if len(m.List()) != 0 || len(m.pending) != 0 {
					t.Fatal("failed recovery published uncommitted account state")
				}
				if data, err := os.ReadFile(private.Path(filename)); err != nil || !bytes.Equal(data, original) {
					t.Fatal("metadata failure discarded private completion evidence")
				}
				current, err := vault.Load(email)
				if err != nil || (kind == "password" && current.Password != value) || (kind != "password" && (current.TOTPSecret != value || current.TOTPResetPending)) {
					t.Fatal("memory rollback changed the successfully saved credential")
				}
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(backup, path); err != nil {
					t.Fatal(err)
				}
				if err := recover(); err != nil {
					t.Fatalf("cannot resume local recovery after disk repair: %v", err)
				}
				job, ok := m.Get(id)
				if !ok || job.OperationStatus != "succeeded" || job.TOTPRecoveryPending || job.TOTPCredentialInvalidated {
					t.Fatal("local retry did not restore confirmed operation outcome")
				}
				if _, err := os.Stat(private.Path(filename)); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("committed recovery did not remove its private result")
				}
				public, err := os.ReadFile(path)
				var decoded any
				if err != nil || json.Unmarshal(public, &decoded) != nil || bytes.Contains(public, []byte(value)) {
					t.Fatal("recovery commit lost valid metadata or exposed its private value")
				}
			})
		}
	}
}
