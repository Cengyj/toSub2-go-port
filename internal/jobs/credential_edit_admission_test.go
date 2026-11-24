package jobs

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"tosub2-go-port/internal/accountlock"
	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/protocol"
)

var errCredentialAdmissionInputRejected = errors.New("synthetic input was rejected")

func credentialAdmissionAction(m *Manager, entry, id, email, operation string, values map[string]any) (*Job, error) {
	switch entry {
	case "submit":
		return m.Submit(Spec{ID: "new-admission", Email: email, Operation: operation, Input: values})
	case "update":
		return m.Update(id, values)
	case "retry":
		return m.RetryWithValues(id, values)
	case "queue":
		return m.QueueOperation(id, operation, values)
	case "input":
		if !m.Input(id, values) {
			return nil, errCredentialAdmissionInputRejected
		}
		job, _ := m.Get(id)
		return job, nil
	default:
		panic("unknown synthetic credential admission entry")
	}
}

func credentialAdmissionAccount(t *testing.T, m *Manager, email string, imported bool, operation string) *Job {
	t.Helper()
	job, err := m.Submit(Spec{ID: "existing-admission", Email: email, Operation: operation, ImportOnly: imported})
	if err != nil {
		t.Fatal(err)
	}
	if !imported {
		if canceled, err := m.CancelWithError(job.ID); err != nil || !canceled {
			t.Fatal("could not prepare an ended operation", err)
		}
		job, _ = m.Get(job.ID)
	}
	return job
}

func credentialAdmissionOriginal() credentials.Credentials {
	return credentials.Credentials{Password: "synthetic-original-password", TOTPSecret: "SYNTHETICACTIVESEED", TOTPFactorID: "synthetic-active-factor", MailBody: "original-mail-body"}
}

func credentialAdmissionQuarantine() credentials.Credentials {
	c := credentialAdmissionOriginal()
	c.TOTPResetPending, c.TOTPResetOperationID = true, "another-output-root-reset"
	c.PendingTOTPSecret, c.PendingTOTPURI, c.PendingTOTPFactorID = "SYNTHETICPENDINGSEED", "synthetic-pending-uri", "synthetic-pending-factor"
	return c
}

// A cleanup failure occurs after the protected write and admission commit.
// Every public entry must keep that accepted result, expose a safe warning,
// and avoid repeating the write or enqueueing the operation a second time.
func TestCredentialEditReleaseFailurePreservesAdmission(t *testing.T) {
	for _, entry := range []string{"submit", "update", "retry", "queue", "input"} {
		t.Run(entry, func(t *testing.T) {
			const origin, email = "https://credential-release.synthetic.invalid", "release-warning@example.test"
			vault := &sharedWriterCountingVault{MemoryStore: credentials.NewMemoryStore()}
			m, st := sharedVaultWriterManager(t, vault, origin)
			id := ""
			if entry != "submit" {
				id = credentialAdmissionAccount(t, m, email, true, "").ID
			}
			original := credentialAdmissionOriginal()
			if err := vault.MemoryStore.Save(email, original); err != nil {
				t.Fatal(err)
			}
			acquisitions, releases := 0, 0
			m.SetAccountOperationLease(func(got string) (func() error, error) {
				acquisitions++
				release, err := accountlock.Acquire(origin, got)
				if err != nil {
					return nil, err
				}
				return func() error {
					releases++
					if err := release(); err != nil {
						return err
					}
					return errors.New("untrusted cleanup detail: token=synthetic-release-secret")
				}, nil
			})
			result, err := credentialAdmissionAction(m, entry, id, email, "relogin", map[string]any{"password": "synthetic-edited-password", "mailRequestBody": "edited-mail-body"})
			if err != nil || result == nil {
				t.Fatal("cleanup failure falsely rejected a confirmed write or admission", err)
			}
			const warningCode = "ACCOUNT_CREDENTIAL_LOCK_RELEASE:"
			if !strings.Contains(result.LastError, warningCode) || strings.Contains(result.LastError, "synthetic-release-secret") {
				t.Error("returned result omitted the safe independent cleanup warning")
			}
			current, ok := m.Get(result.ID)
			if !ok || current.LastError != result.LastError || current.OperationError != "" || current.Error != "" {
				t.Error("cleanup changed the business result or disappeared from the current account")
			}
			wantStatus, wantQueued := StatusQueued, 1
			if entry == "update" {
				wantStatus, wantQueued = StatusImported, 0
			}
			if result.Status != wantStatus || m.ActivitySnapshot().Queued != wantQueued || result.Attempts != 0 {
				t.Error("cleanup failure revoked or duplicated task admission")
			}
			want := original
			want.Password, want.MailBody = "synthetic-edited-password", "edited-mail-body"
			got, loadErr := vault.Load(email)
			if loadErr != nil || !credentials.EqualReadback(want, got) || vault.saves.Load() != 1 {
				t.Errorf("cleanup replayed or lost the credential write: saves=%d err=%v", vault.saves.Load(), loadErr)
			}
			if acquisitions != 1 || releases != 1 {
				t.Errorf("unexpected ownership lifecycle: acquisitions=%d releases=%d", acquisitions, releases)
			}
			data, err := os.ReadFile(filepath.Join(st.Root, "job-meta.json"))
			if err != nil {
				t.Fatal(err)
			}
			var metadata struct {
				Jobs []struct{ ID, Status string }
			}
			if err := json.Unmarshal(data, &metadata); err != nil || len(metadata.Jobs) != 1 || metadata.Jobs[0].ID != result.ID || metadata.Jobs[0].Status != wantStatus {
				t.Fatal("cleanup failure lost the already committed operation metadata", err)
			}
			if bytes.Contains(data, []byte("synthetic-edited-password")) || bytes.Contains(data, []byte("synthetic-release-secret")) {
				t.Error("ordinary metadata exposed credential or untrusted cleanup details")
			}
			release, err := accountlock.Acquire(origin, email)
			if err != nil {
				t.Fatal("synthetic release failure leaked its actual OS lock", err)
			}
			if err := release(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// Publish peer recovery evidence at the real account-lock adapter boundary,
// after any preliminary eligibility check. No private manager state is changed
// by the fixture: the manager must observe the new shared-vault state itself.
func TestCredentialEditRechecksSharedQuarantineAfterLease(t *testing.T) {
	for _, entry := range []string{"retry", "queue", "input"} {
		for _, mode := range []string{"ended-explicit-edit", "imported-explicit-edit", "imported-resolve"} {
			if entry == "input" && mode == "imported-resolve" {
				continue // This entry has no implicit imported-credential resolution.
			}
			t.Run(entry+"/"+mode, func(t *testing.T) {
				const origin, email = "https://credential-admission.synthetic.invalid", "post-lease-quarantine@example.test"
				vault := &sharedWriterCountingVault{MemoryStore: credentials.NewMemoryStore()}
				m, st := sharedVaultWriterManager(t, vault, origin)
				job := credentialAdmissionAccount(t, m, email, mode != "ended-explicit-edit", "full")
				if err := vault.MemoryStore.Save(email, credentialAdmissionOriginal()); err != nil {
					t.Fatal(err)
				}
				values := map[string]any{"password": "synthetic-edited-password", "totpSecret": "SYNTHETICEDITEDSEED", "mailRequestBody": "edited-mail-body"}
				if mode == "imported-resolve" {
					values = nil
				}
				assertCredentialAdmissionRejectsNewQuarantine(t, m, vault, st.Root, origin, job, entry, "relogin", values)
			})
		}
	}
}

func assertCredentialAdmissionRejectsNewQuarantine(t *testing.T, m *Manager, vault *sharedWriterCountingVault, root, origin string, job *Job, entry, operation string, values map[string]any) {
	t.Helper()
	beforeJobs := m.List()
	metaPath := filepath.Join(root, "job-meta.json")
	beforeMeta, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatal(err)
	}
	quarantine := credentialAdmissionQuarantine()
	acquisitions, releases := 0, 0
	m.SetAccountOperationLease(func(email string) (func() error, error) {
		acquisitions++
		release, err := accountlock.Acquire(origin, email)
		if err != nil {
			return nil, err
		}
		if err := vault.MemoryStore.Save(email, quarantine); err != nil {
			_ = release()
			return nil, err
		}
		return func() error { releases++; return release() }, nil
	})
	result, err := credentialAdmissionAction(m, entry, job.ID, job.Email, operation, values)
	wantErr := ErrTOTPRecoveryPending
	if entry == "input" {
		wantErr = errCredentialAdmissionInputRejected
	}
	if result != nil || !errors.Is(err, wantErr) {
		t.Error("new shared quarantine did not reject the credential edit before admission", err)
	}
	if acquisitions != 1 || releases != 1 {
		t.Errorf("fixture did not cross and clean up the ownership boundary once: acquisitions=%d releases=%d", acquisitions, releases)
	}
	if vault.saves.Load() != 0 {
		t.Errorf("rejected admission wrote the credential vault %d times", vault.saves.Load())
	}
	got, loadErr := vault.Load(job.Email)
	if loadErr != nil || !credentials.EqualReadback(quarantine, got) {
		t.Error("rejected admission changed peer recovery evidence", loadErr)
	}
	if !reflect.DeepEqual(beforeJobs, m.List()) || m.ActivitySnapshot().Queued != 0 {
		t.Error("rejected admission mutated account/input/operation state or queued work")
	}
	afterMeta, readErr := os.ReadFile(metaPath)
	if readErr != nil || !bytes.Equal(beforeMeta, afterMeta) {
		t.Error("rejected admission committed metadata", readErr)
	}
}

func TestCredentialEditCannotBorrowLocalRecoveryExemption(t *testing.T) {
	for _, evidence := range []string{"issued-grant", "refresh-routing-hint"} {
		for _, entry := range []string{"retry", "queue", "input"} {
			for _, mode := range []string{"ended-explicit-edit", "imported-resolve"} {
				if entry == "input" && mode == "imported-resolve" {
					continue
				}
				t.Run(evidence+"/"+entry+"/"+mode, func(t *testing.T) {
					const origin, email = "https://credential-local-recovery.synthetic.invalid", "local-recovery@example.test"
					vault := &sharedWriterCountingVault{MemoryStore: credentials.NewMemoryStore()}
					m, st := sharedVaultWriterManager(t, vault, origin)
					job := credentialAdmissionAccount(t, m, email, mode == "imported-resolve", "resume")
					if err := vault.MemoryStore.Save(email, credentialAdmissionOriginal()); err != nil {
						t.Fatal(err)
					}
					path := OAuthGrantPath(st.Root, job)
					if evidence == "issued-grant" {
						if err := protocol.WriteSavedOAuthGrant(path, issuedGrantFixture(email, "chatgpt")); err != nil {
							t.Fatal(err)
						}
					} else {
						// Existence routes to local recovery; the production runner
						// still owns validation and must not use this as a grant.
						path = filepath.Join(st.Root, job.ID, refreshRecoveryFilename)
						if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
							t.Fatal(err)
						}
						if err := os.WriteFile(path, []byte("synthetic-unvalidated-refresh-routing-hint\n"), 0600); err != nil {
							t.Fatal(err)
						}
					}
					artifact, err := os.ReadFile(path)
					if err != nil {
						t.Fatal(err)
					}
					values := map[string]any{"password": "synthetic-edited-password", "totpSecret": "SYNTHETICEDITEDSEED"}
					if mode == "imported-resolve" {
						values = nil
					}
					assertCredentialAdmissionRejectsNewQuarantine(t, m, vault, st.Root, origin, job, entry, "resume", values)
					after, err := os.ReadFile(path)
					if err != nil || !bytes.Equal(artifact, after) {
						t.Error("rejected credential edit changed or consumed its local recovery artifact", err)
					}
				})
			}
		}
	}
}

func TestCredentialAdmissionKeepsPureIssuedGrantCompletion(t *testing.T) {
	for _, entry := range []string{"retry", "queue"} {
		t.Run(entry, func(t *testing.T) {
			const origin, email = "https://credential-pure-recovery.synthetic.invalid", "pure-local-recovery@example.test"
			vault := &sharedWriterCountingVault{MemoryStore: credentials.NewMemoryStore()}
			m, st := sharedVaultWriterManager(t, vault, origin)
			job := credentialAdmissionAccount(t, m, email, false, "relogin")
			path := OAuthGrantPath(st.Root, job)
			if err := protocol.WriteSavedOAuthGrant(path, issuedGrantFixture(email, "chatgpt")); err != nil {
				t.Fatal(err)
			}
			artifact, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			quarantine := credentialAdmissionQuarantine()
			if err := vault.MemoryStore.Save(email, quarantine); err != nil {
				t.Fatal(err)
			}
			acquisitions := 0
			m.SetAccountOperationLease(func(string) (func() error, error) {
				acquisitions++
				return nil, accountlock.ErrBusy
			})
			result, err := credentialAdmissionAction(m, entry, job.ID, email, "resume", nil)
			if err != nil || result == nil || result.Operation != "resume" || result.Status != StatusQueued || result.OAuthResultID != job.OAuthResultID {
				t.Fatal("pure completion of the issued grant lost its independent local route", err)
			}
			if acquisitions != 0 || vault.saves.Load() != 0 || m.ActivitySnapshot().Queued != 1 || result.Attempts != 0 {
				t.Error("pure grant completion attempted credential editing or duplicate admission")
			}
			got, loadErr := vault.Load(email)
			if loadErr != nil || !credentials.EqualReadback(quarantine, got) {
				t.Error("pure grant completion changed shared recovery evidence", loadErr)
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(artifact, after) {
				t.Error("admitting local completion changed its issued grant", err)
			}
		})
	}
}
