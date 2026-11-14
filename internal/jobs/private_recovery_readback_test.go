package jobs

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"tosub2-go-port/internal/accountlock"
	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/store"
)

type privateRecoveryReadbackVault struct {
	memoryVault
	fault        string
	faultAt      int
	saves        int
	failNextLoad bool
}

func (v *privateRecoveryReadbackVault) Save(email string, value credentials.Credentials) error {
	v.saves++
	if v.saves == v.faultAt {
		switch v.fault {
		case "drop":
			return nil
		case "partial":
			value.MailBody = "synthetic altered sibling field"
		case "readback":
			v.failNextLoad = true
		}
	}
	return v.memoryVault.Save(email, value)
}

func (v *privateRecoveryReadbackVault) Load(email string) (credentials.Credentials, error) {
	if v.failNextLoad {
		v.failNextLoad = false
		return credentials.Credentials{}, errors.New("synthetic readback failure")
	}
	return v.memoryVault.Load(email)
}

func privateRecoveryReadbackFixture(t *testing.T, kind, status string, sibling bool) (*Manager, *privateRecoveryReadbackVault, string, string) {
	t.Helper()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const id, email = "private-result", "private-result@example.test"
	metadata := map[string]any{"id": id, "email": email, "operation": kind, "status": status, "operationStatus": "succeeded"}
	private, err := store.New(filepath.Join(st.Root, id))
	if err != nil {
		t.Fatal(err)
	}
	if err := private.WriteJSON(t.Context(), "job-meta.json", metadata); err != nil {
		t.Fatal(err)
	}
	if err := st.WriteJSON(t.Context(), "job-meta.json", map[string]any{"version": 1, "jobs": []any{metadata}}); err != nil {
		t.Fatal(err)
	}
	initial := credentials.Credentials{
		Password: "Synthetic-old-password9!", PendingPassword: "Synthetic-new-password9!",
		PendingTOTPSecret: "SYNTHETICNEWSEED", PendingTOTPURI: "synthetic-pending-uri",
		MailAPIURL: "https://mail.example.test/", MailMethod: "POST", MailBody: "synthetic unchanged sibling field",
		MailHeaders: map[string]string{"X-Synthetic": "fixture"}, ProxyURL: "http://proxy.example.test:8080",
	}
	vault := &privateRecoveryReadbackVault{faultAt: 1}
	if err := vault.memoryVault.Save(email, initial); err != nil {
		t.Fatal(err)
	}
	filename := "totp-setup-result.json"
	if kind == "add-password" || sibling {
		if err := private.WriteJSON(t.Context(), "password-add-result.json", map[string]any{"version": 1, "email": email, "password": "Synthetic-new-password9!"}); err != nil {
			t.Fatal(err)
		}
	}
	if kind == "add-password" {
		filename = "password-add-result.json"
	} else if err := private.WriteJSON(t.Context(), filename, map[string]any{"version": 1, "email": email, "secret": "SYNTHETICNEWSEED", "activation_succeeded": true}); err != nil {
		t.Fatal(err)
	}
	m := NewManager(t.Context(), 1, st, nil)
	m.SetCredentialStore(vault)
	return m, vault, id, private.Path(filename)
}

func restorePrivateRecoveryFixture(t *testing.T, m *Manager, live bool) {
	t.Helper()
	var err error
	if live {
		err = m.SyncOutputs(t.Context(), true)
	} else {
		err = m.Restore(t.Context())
	}
	if err != nil {
		t.Fatal(err)
	}
}

func TestPrivateRecoveryRequiresFullCredentialReadback(t *testing.T) {
	for _, kind := range []string{"setup-2fa", "add-password"} {
		for _, fault := range []string{"drop", "partial", "readback"} {
			for _, live := range []bool{false, true} {
				name := kind + "/" + fault + map[bool]string{false: "/startup", true: "/discovery"}[live]
				t.Run(name, func(t *testing.T) {
					m, vault, id, path := privateRecoveryReadbackFixture(t, kind, StatusQueued, false)
					vault.fault = fault
					original, err := os.ReadFile(path)
					if err != nil {
						t.Fatal(err)
					}
					before, _ := vault.memoryVault.Load("private-result@example.test")
					restorePrivateRecoveryFixture(t, m, live)
					job, ok := m.Get(id)
					retained, readErr := os.ReadFile(path)
					if !ok || job.Status != StatusFailed || job.OperationStatus != "failed" || job.OperationError == "" || job.passwordCleanup || job.totpSetupCleanup || len(m.pending) != 0 || vault.saves != 1 {
						t.Fatal("failed local save published success, queued a replay, or marked private evidence for cleanup")
					}
					if readErr != nil || !bytes.Equal(retained, original) {
						t.Fatal("failed credential readback discarded the only private result", readErr)
					}
					if kind == "setup-2fa" && (job.HasTOTPKey || job.TOTPSetupError == "") || kind == "add-password" && (job.HasPassword || job.PasswordAddError == "") {
						t.Fatal("final credential load overturned the failed write/readback outcome")
					}
					after, _ := vault.memoryVault.Load(job.Email)
					if fault == "drop" && !reflect.DeepEqual(before, after) {
						t.Fatal("dropped save lost pending or unrelated credential evidence")
					}
					// Once storage works again, the retained business result completes
					// locally and is removed only after verified save and metadata commit.
					vault.fault = ""
					restarted := NewManager(t.Context(), 1, m.store, nil)
					restarted.SetCredentialStore(vault)
					restorePrivateRecoveryFixture(t, restarted, false)
					completed, ok := restarted.Get(id)
					if !ok || completed.OperationStatus != "succeeded" {
						t.Fatal("verified local retry did not complete")
					}
					if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
						t.Fatal("verified local recovery retained already committed result", err)
					}
				})
			}
		}
	}
}

func TestPrivateRecoveryPasswordSuccessDoesNotMaskSetupSaveFailure(t *testing.T) {
	m, vault, id, path := privateRecoveryReadbackFixture(t, "setup-2fa", StatusFailed, true)
	vault.fault, vault.faultAt = "drop", 2
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	restorePrivateRecoveryFixture(t, m, false)
	job, _ := m.Get(id)
	after, _ := vault.memoryVault.Load(job.Email)
	retained, readErr := os.ReadFile(path)
	if job.OperationStatus != "failed" || job.HasTOTPKey || !job.HasPassword || vault.saves != 2 || after.Password != "Synthetic-new-password9!" || after.PendingTOTPSecret != "SYNTHETICNEWSEED" || readErr != nil || !bytes.Equal(retained, original) {
		t.Fatal("sibling password success hid setup save failure or discarded pending setup evidence")
	}
}

func TestPrivateRecoveryUsesConfiguredOriginAccountLease(t *testing.T) {
	for _, live := range []bool{false, true} {
		t.Run(map[bool]string{false: "startup", true: "discovery"}[live], func(t *testing.T) {
			m, vault, id, path := privateRecoveryReadbackFixture(t, "setup-2fa", StatusFailed, true)
			const origin, email = "https://configured-recovery.example.test", "private-result@example.test"
			release, err := accountlock.Acquire(origin, email)
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			acquisitions := 0
			m.SetAccountOperationLease(func(got string) (func() error, error) {
				acquisitions++
				if got != email {
					t.Fatal("recovery requested a lease for another account")
				}
				return accountlock.Acquire(origin, got)
			})
			original, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			before, _ := vault.memoryVault.Load(email)
			restorePrivateRecoveryFixture(t, m, live)
			job, _ := m.Get(id)
			after, _ := vault.memoryVault.Load(email)
			retained, readErr := os.ReadFile(path)
			if acquisitions != 1 || vault.saves != 0 || !reflect.DeepEqual(before, after) || job.OperationStatus == "succeeded" || job.passwordCleanup || job.totpSetupCleanup || readErr != nil || !bytes.Equal(retained, original) {
				t.Fatal("startup or discovery bypassed the configured-origin account lock")
			}
			if _, err := os.Stat(filepath.Join(m.store.Root, id, "password-add-result.json")); err != nil {
				t.Fatal("lock conflict discarded sibling password result", err)
			}
		})
	}
}
