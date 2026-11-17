package jobs

import (
	"path/filepath"
	"reflect"
	"testing"

	"tosub2-go-port/internal/accountlock"
	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/store"
)

func TestLegacyMailboxRecoveryRespectsAccountLease(t *testing.T) {
	for _, live := range []bool{false, true} {
		t.Run(map[bool]string{false: "startup", true: "discovery"}[live], func(t *testing.T) {
			const origin, email = "https://mailbox-recovery-lock.example.test", "mailbox-recovery@example.test"
			const mailbox = "https://mail.example.test/api?token=synthetic-mailbox-key"
			st, err := store.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			dir, err := store.New(filepath.Join(st.Root, "mailbox-recovery"))
			if err != nil {
				t.Fatal(err)
			}
			if err := dir.WriteJSON(t.Context(), "job-meta.json", map[string]any{"email": email, "status": "queued", "mail_api_url": mailbox}); err != nil {
				t.Fatal(err)
			}
			vault := &privateRecoveryReadbackVault{}
			before := credentials.Credentials{Password: "Synthetic-password9!", TOTPSecret: "SYNTHETICSEED"}
			if err := vault.memoryVault.Save(email, before); err != nil {
				t.Fatal(err)
			}
			release, err := accountlock.Acquire(origin, email)
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			m := NewManager(t.Context(), 1, st, nil)
			m.SetCredentialStore(vault)
			acquisitions := 0
			m.SetAccountOperationLease(func(email string) (func() error, error) {
				acquisitions++
				return accountlock.Acquire(origin, email)
			})
			restorePrivateRecoveryFixture(t, m, live)
			after, _ := vault.Load(email)
			job, _ := m.Get("mailbox-recovery")
			if acquisitions != 1 || vault.saves != 0 || !reflect.DeepEqual(before, after) || job.MailAPIError == "" {
				t.Fatalf("legacy mailbox migration bypassed the live account lease: acquisitions=%d saves=%d", acquisitions, vault.saves)
			}
			if job.MailAPIURL != mailbox {
				t.Fatal("lease conflict discarded the legacy mailbox input")
			}
			if err := release(); err != nil {
				t.Fatal(err)
			}
			restarted := NewManager(t.Context(), 1, st, nil)
			restarted.SetCredentialStore(vault)
			restarted.SetAccountOperationLease(m.accountOperationLease)
			restorePrivateRecoveryFixture(t, restarted, false)
			completed, _ := vault.Load(email)
			before.MailAPIURL = mailbox
			if vault.saves != 1 || !reflect.DeepEqual(before, completed) {
				t.Fatal("released lease did not safely migrate the retained mailbox")
			}
		})
	}
}

func TestLegacyMailboxRecoveryUsesCredentialSnapshotUnderLease(t *testing.T) {
	for _, update := range []string{"password", "mailbox", "pending-reset", "new-account-reset", "missing-lease", "dropped-write"} {
		t.Run(update, func(t *testing.T) {
			const email, mailbox = "mailbox-newer@example.test", "https://legacy-mail.example.test/api?token=synthetic-key"
			st, err := store.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			dir, err := store.New(filepath.Join(st.Root, "mailbox-newer"))
			if err != nil {
				t.Fatal(err)
			}
			if err := dir.WriteJSON(t.Context(), "job-meta.json", map[string]any{"email": email, "status": "queued", "mail_api_url": mailbox}); err != nil {
				t.Fatal(err)
			}
			vault := &privateRecoveryReadbackVault{faultAt: 1}
			if update != "new-account-reset" {
				if err := vault.memoryVault.Save(email, credentials.Credentials{Password: "Synthetic-old-password9!"}); err != nil {
					t.Fatal(err)
				}
			}
			newer := credentials.Credentials{Password: "Synthetic-new-password9!", TOTPSecret: "SYNTHETICNEWSEED", MailHeaders: map[string]string{"X-Synthetic": "newer-header"}}
			if update == "mailbox" {
				newer.MailAPIURL, newer.MailMethod, newer.MailBody = "https://new-mail.example.test/api", "POST", "synthetic-new-body"
			}
			if update == "pending-reset" || update == "new-account-reset" {
				newer.TOTPResetPending, newer.TOTPResetOperationID = true, "synthetic-reset-operation"
			}
			if update == "dropped-write" {
				vault.fault = "drop"
			}
			m := NewManager(t.Context(), 1, st, nil)
			m.SetCredentialStore(vault)
			acquisitions, releases := 0, 0
			m.SetAccountOperationLease(func(string) (func() error, error) {
				acquisitions++
				if err := vault.memoryVault.Save(email, newer); err != nil {
					t.Fatal(err)
				}
				if update == "missing-lease" {
					return nil, nil
				}
				return func() error { releases++; return nil }, nil
			})
			restorePrivateRecoveryFixture(t, m, false)
			got, _ := vault.Load(email)
			want := newer
			if update == "password" {
				want.MailAPIURL = mailbox
			}
			if !reflect.DeepEqual(got, want) || acquisitions != 1 || releases != map[bool]int{true: 0, false: 1}[update == "missing-lease"] {
				t.Fatal("mailbox migration replaced newer credential state or leaked its lease")
			}
			job, _ := m.Get("mailbox-newer")
			if update == "password" || update == "mailbox" {
				if job.MailAPIURL != want.MailAPIURL || job.MailAPIError != "" {
					t.Fatal("restored mailbox does not use the current protected record")
				}
			} else if job.MailAPIError == "" || job.MailAPIURL != mailbox {
				t.Fatal("migration failure hid its diagnostic or discarded legacy input")
			}
			if (update == "pending-reset" || update == "new-account-reset") && (!job.TOTPRecoveryPending || vault.saves != 0) {
				t.Fatal("mailbox migration wrote past a newer factor quarantine")
			}
			if len(m.pending) != 0 {
				t.Fatal("mailbox migration replayed a historical queued operation")
			}
		})
	}
}
