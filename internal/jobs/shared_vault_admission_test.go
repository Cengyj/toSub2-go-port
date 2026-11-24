package jobs

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/privatestate"
	"tosub2-go-port/internal/store"
)

// A candidate can belong to a different output root sharing the credential
// namespace. Import is a local save: it must keep that evidence independently
// of whether the caller reuses, replaces, or explicitly clears active inputs.
func TestSharedVaultImportOnlyPreservesPendingSetupMaterial(t *testing.T) {
	for _, candidate := range []string{"complete", "secret-only", "uri-only", "factor-only"} {
		for _, existing := range []bool{false, true} {
			for _, edit := range []string{"email-only", "replace", "clear", "mail-only"} {
				entry := "new"
				if existing {
					entry = "existing"
				}
				t.Run(candidate+"/"+entry+"/"+edit, func(t *testing.T) {
					st, err := store.New(t.TempDir())
					if err != nil {
						t.Fatal(err)
					}
					vault := credentials.NewMemoryStore()
					m := NewManager(t.Context(), 1, st, nil)
					m.SetCredentialStore(vault)
					t.Cleanup(func() { _ = m.Shutdown(context.Background()) })
					const email = "shared-import@example.test"
					var original *Job
					if existing {
						original, _, err = m.SubmitUniqueEmail(Spec{Email: email, ImportOnly: true, Input: map[string]any{"password": "original-password", "totpSecret": "OLDSEED"}})
						if err != nil {
							t.Fatal(err)
						}
					}
					saved := credentials.Credentials{Password: "original-password", TOTPSecret: "OLDSEED", TOTPFactorID: "active-factor", ProxyURL: "http://proxy.invalid:8080", MailAPIURL: "https://mail.invalid/messages", MailMethod: "POST", MailBody: "original-body", MailHeaders: map[string]string{"X-Fixture": "original"}, PendingPassword: "retained-password-candidate", TOTPResetOperationID: "prior-completed-reset"}
					if candidate == "complete" || candidate == "secret-only" {
						saved.PendingTOTPSecret = "PENDINGSEED"
					}
					if candidate == "complete" || candidate == "uri-only" {
						saved.PendingTOTPURI = "otpauth://totp/shared-fixture?secret=PENDINGSEED"
					}
					if candidate == "complete" || candidate == "factor-only" {
						saved.PendingTOTPFactorID = "pending-factor"
					}
					if err := vault.Save(email, saved); err != nil {
						t.Fatal(err)
					}
					want := saved
					var input map[string]any
					switch edit {
					case "replace":
						input = map[string]any{"password": "  replacement password  ", "totpSecret": "NEWSEED", "proxyUrl": "", "mailApiUrl": "https://mail.invalid/replaced", "mailRequestMethod": "POST", "mailRequestBody": "{\"fixture\":true}", "mailRequestHeaders": map[string]string{"X-Fixture": "replacement"}}
						want.Password, want.TOTPSecret, want.TOTPFactorID, want.ProxyURL = "  replacement password  ", "NEWSEED", "", ""
						want.MailAPIURL, want.MailMethod, want.MailBody, want.MailHeaders = "https://mail.invalid/replaced", "POST", "{\"fixture\":true}", map[string]string{"X-Fixture": "replacement"}
					case "clear":
						input = map[string]any{"password": "", "totpSecret": "", "proxyUrl": "", "mailApiUrl": "", "mailRequestMethod": "GET", "mailRequestBody": "", "mailRequestHeaders": map[string]string{}}
						want.Password, want.TOTPSecret, want.TOTPFactorID, want.ProxyURL = "", "", "", ""
						want.MailAPIURL, want.MailMethod, want.MailBody, want.MailHeaders = "", "GET", "", nil
					case "mail-only":
						input = map[string]any{"mailRequestBody": "changed-body"}
						want.MailBody = "changed-body"
					}
					job, created, err := m.SubmitUniqueEmail(Spec{Email: " SHARED-IMPORT@example.test ", ImportOnly: true, Input: input})
					if err != nil {
						t.Fatal("local import was rejected", err)
					}
					if created == existing || job.Status != StatusImported || job.Attempts != 0 || !job.QueuedAt.IsZero() || existing && job.ID != original.ID {
						t.Fatal("import changed identity, attempts, or scheduling")
					}
					got, err := vault.Load(email)
					if err != nil || !credentials.EqualReadback(want, got) {
						t.Error("import changed pending evidence or violated explicit credential replacement/clear semantics", err)
					}
					// Start the actual scheduler and run an unrelated sentinel. Its
					// completion makes the no-automatic-import assertion bounded.
					var importedRuns atomic.Int32
					m.SetRunner(func(_ context.Context, current *Job, _ func(JobEvent)) error {
						if current.ID == job.ID {
							importedRuns.Add(1)
						}
						return nil
					})
					m.Start()
					sentinel, err := m.Submit(Spec{Email: "sentinel@example.test"})
					if err != nil {
						t.Fatal(err)
					}
					waitForStatus(t, m, sentinel.ID, StatusSucceeded)
					if err := m.Shutdown(context.Background()); err != nil {
						t.Fatal(err)
					}
					if importedRuns.Load() != 0 {
						t.Fatal("save-only import started an authentication runner")
					}
				})
			}
		}
	}
}

// A refresh artifact permits its own local recovery route. Merely editing
// credentials is not that route and cannot use the artifact to bypass a reset
// quarantine. This protects the admission refactor's operation classification.
func TestSharedVaultUpdateCannotBorrowRefreshRecoveryExemption(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	vault := credentials.NewMemoryStore()
	m := NewManager(t.Context(), 1, st, nil)
	m.SetCredentialStore(vault)
	t.Cleanup(func() { _ = m.Shutdown(context.Background()) })
	job, err := m.Submit(Spec{ID: "update-quarantine", Email: "update-quarantine@example.test", ImportOnly: true, Input: map[string]any{"password": "original-password", "totpSecret": "OLDSEED"}})
	if err != nil {
		t.Fatal(err)
	}
	saved, err := vault.Load(job.Email)
	if err != nil {
		t.Fatal(err)
	}
	saved.TOTPResetPending, saved.TOTPResetOperationID = true, "another-root-reset"
	saved.PendingTOTPSecret, saved.PendingTOTPURI, saved.PendingTOTPFactorID = "PENDINGSEED", "synthetic-uri", "pending-factor"
	if err := vault.Save(job.Email, saved); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(st.Root, job.ID, refreshRecoveryFilename)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	// Existence is deliberately only a routing hint, even for an unreadable
	// or malformed artifact. An edit must not read or consume its contents.
	artifact := []byte("synthetic-unvalidated-refresh-recovery\n")
	if err := os.WriteFile(path, artifact, 0600); err != nil {
		t.Fatal(err)
	}
	metaPath := filepath.Join(st.Root, "job-meta.json")
	beforeMeta, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatal(err)
	}
	beforeJob, _ := m.Get(job.ID)
	for _, operation := range []string{"", "relogin", "setup-2fa"} {
		values := map[string]any{"password": "replacement-password", "totpSecret": "REPLACEMENTSEED"}
		if operation != "" {
			values["operation"] = operation
		}
		if _, err := m.Update(job.ID, values); !errors.Is(err, ErrTOTPRecoveryPending) {
			t.Error("input edit borrowed a local refresh exemption", err)
		}
	}
	got, err := vault.Load(job.Email)
	if err != nil || !credentials.EqualReadback(saved, got) {
		t.Error("rejected edit changed shared recovery material", err)
	}
	afterJob, _ := m.Get(job.ID)
	if !reflect.DeepEqual(beforeJob, afterJob) {
		t.Error("rejected edit changed account or operation metadata")
	}
	afterMeta, err := os.ReadFile(metaPath)
	if err != nil || !bytes.Equal(beforeMeta, afterMeta) {
		t.Error("rejected edit rewrote persistent metadata", err)
	}
	afterArtifact, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(artifact, afterArtifact) {
		t.Error("rejected edit changed the local refresh artifact", err)
	}
}

func TestSharedVaultRetryAfterCanceledSetupUsesOrdinaryIntent(t *testing.T) {
	m := NewManager(t.Context(), 1, nil, nil)
	vault := credentials.NewMemoryStore()
	m.SetCredentialStore(vault)
	t.Cleanup(func() { _ = m.Shutdown(context.Background()) })
	job, err := m.Submit(Spec{Email: "retry-setup@example.test", ImportOnly: true, Input: map[string]any{"password": "original-password", "totpSecret": "ACTIVESEED"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.QueueOperation(job.ID, "setup-2fa", nil); err != nil {
		t.Fatal(err)
	}
	if canceled, err := m.CancelWithError(job.ID); err != nil || !canceled {
		t.Fatal("cannot cancel prior synthetic setup", err)
	}
	before, _ := m.Get(job.ID)
	if before.Operation != "setup-2fa" {
		t.Fatal("fixture lost its prior setup operation")
	}
	saved, err := vault.Load(job.Email)
	if err != nil {
		t.Fatal(err)
	}
	saved.PendingTOTPSecret, saved.PendingTOTPURI, saved.PendingTOTPFactorID = "PENDINGSEED", "synthetic-uri", "pending-factor"
	if err := vault.Save(job.Email, saved); err != nil {
		t.Fatal(err)
	}
	queued, err := m.RetryWithValues(job.ID, nil)
	if err != nil || queued.Operation != "full" || queued.ResetTOTP {
		t.Fatal("ordinary Retry inherited a preceding setup intent", err)
	}
	runs := make(chan *Job, 1)
	m.SetRunner(func(_ context.Context, current *Job, _ func(JobEvent)) error {
		runs <- current
		return nil
	})
	m.Start()
	select {
	case run := <-runs:
		if run.Operation != "full" || run.Input["totpSecret"] != "ACTIVESEED" {
			t.Error("retry runner received a security-change intent or pending seed")
		}
	case <-time.After(time.Second):
		t.Fatal("accepted retry did not reach the runner")
	}
	waitForStatus(t, m, job.ID, StatusSucceeded)
	got, err := vault.Load(job.Email)
	if err != nil || !credentials.EqualReadback(saved, got) {
		t.Error("ordinary retry admission changed pending evidence", err)
	}
}

func TestSharedVaultBeginResetRejectsCandidatePublishedAfterIntent(t *testing.T) {
	for _, candidate := range []string{"none", "complete", "secret-only", "uri-only", "factor-only"} {
		t.Run(candidate, func(t *testing.T) {
			m, vault, job, peer := resetManagerFixture(t)
			t.Cleanup(func() { _ = m.Shutdown(context.Background()) })
			if _, err := m.QueueOperation(job.ID, "reset-2fa", nil); err != nil {
				t.Fatal(err)
			}
			registerCompletionFixture(t, m.store.Root)
			intentPath := filepath.Join(m.store.Root, job.ID, "totp-setup-result.json")
			intent := enrollFirstBeginJournal(job, "disabled")
			if err := privatestate.WriteJSON(t.Context(), intentPath, "totp-reset", job.Email, "enroll-first-operation", intent); err != nil {
				t.Fatal(err)
			}
			beforeIntent, err := os.ReadFile(intentPath)
			if err != nil {
				t.Fatal(err)
			}
			// The local intent was saved without any shared candidate. Publish
			// the peer's evidence only now, immediately before acknowledgement.
			saved, err := vault.Load(job.Email)
			if err != nil {
				t.Fatal(err)
			}
			if candidate == "complete" || candidate == "secret-only" {
				saved.PendingTOTPSecret = "OTHERROOTSEED"
			}
			if candidate == "complete" || candidate == "uri-only" {
				saved.PendingTOTPURI = "other-root-uri"
			}
			if candidate == "complete" || candidate == "factor-only" {
				saved.PendingTOTPFactorID = "other-root-factor"
			}
			if err := vault.Save(job.Email, saved); err != nil {
				t.Fatal(err)
			}
			counting := &unboundRestoreCountingVault{memoryVault: vault}
			m.SetCredentialStore(counting)
			beforeJob, _ := m.Get(job.ID)
			beforePeer, _ := m.Get(peer.ID)
			metaPath := filepath.Join(m.store.Root, "job-meta.json")
			beforeMeta, err := os.ReadFile(metaPath)
			if err != nil {
				t.Fatal(err)
			}
			err = m.BeginTOTPReset(t.Context(), job.ID, "enroll-first-operation")
			if candidate == "none" {
				if err != nil {
					t.Fatal("valid unchanged intent was refused", err)
				}
				committed, loadErr := vault.Load(job.Email)
				if loadErr != nil || !committed.TOTPResetPending || committed.TOTPResetOperationID != "enroll-first-operation" || counting.saves != 1 {
					t.Fatal("positive control did not commit its own reset quarantine", loadErr)
				}
				return
			}
			if !errors.Is(err, ErrTOTPSetupVerificationPending) || counting.saves != 0 {
				t.Error("new candidate did not stop the reset acknowledgement before vault writes", err)
			}
			retained, loadErr := vault.Load(job.Email)
			if loadErr != nil || !credentials.EqualReadback(saved, retained) {
				t.Error("reset acknowledgement changed shared recovery material", loadErr)
			}
			afterJob, _ := m.Get(job.ID)
			afterPeer, _ := m.Get(peer.ID)
			if !reflect.DeepEqual(beforeJob, afterJob) || !reflect.DeepEqual(beforePeer, afterPeer) {
				t.Error("refused acknowledgement changed account quarantine metadata")
			}
			for _, check := range []struct {
				path string
				data []byte
			}{{intentPath, beforeIntent}, {metaPath, beforeMeta}} {
				after, readErr := os.ReadFile(check.path)
				if readErr != nil || !bytes.Equal(check.data, after) {
					t.Error("refused acknowledgement rewrote persisted intent or metadata", readErr)
				}
			}
		})
	}
}

func TestSharedVaultDirectSetupSubmitRejectsButImportOnlyRemainsLocal(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	vault := credentials.NewMemoryStore()
	m := NewManager(t.Context(), 1, st, nil)
	m.SetCredentialStore(vault)
	t.Cleanup(func() { _ = m.Shutdown(context.Background()) })
	const email = "direct-setup@example.test"
	saved := credentials.Credentials{Password: "original-password", TOTPSecret: "ACTIVESEED", TOTPFactorID: "active-factor", PendingTOTPSecret: "PENDINGSEED", PendingTOTPURI: "pending-uri", PendingTOTPFactorID: "pending-factor"}
	if err := vault.Save(email, saved); err != nil {
		t.Fatal(err)
	}
	spec := Spec{ID: "direct-setup", Email: email, Operation: "setup-totp"}
	if _, err := m.Submit(spec); !errors.Is(err, ErrTOTPSetupVerificationPending) {
		t.Error("direct setup submission did not reject shared pending material", err)
	}
	if len(m.List()) != 0 {
		t.Fatal("rejected direct submission left an account or operation")
	}
	if _, err := os.Stat(filepath.Join(st.Root, "job-meta.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("rejected direct submission wrote metadata", err)
	}
	retained, err := vault.Load(email)
	if err != nil || !credentials.EqualReadback(saved, retained) {
		t.Error("rejected direct submission changed shared material", err)
	}
	spec.ImportOnly = true
	imported, err := m.Submit(spec)
	if err != nil || imported.Operation != "" || imported.Status != StatusImported || imported.Attempts != 0 || !imported.QueuedAt.IsZero() {
		t.Fatal("save-only submission was classified as a remote setup", err)
	}
	retained, err = vault.Load(email)
	if err != nil || !credentials.EqualReadback(saved, retained) {
		t.Error("save-only submission changed shared material", err)
	}
}
