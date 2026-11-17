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
)

type passwordRecoveryLoadHook struct {
	credentials.Store
	load func()
}

func (v *passwordRecoveryLoadHook) Load(email string) (credentials.Credentials, error) {
	if v.load != nil {
		v.load()
	}
	return v.Store.Load(email)
}

func TestPasswordOnlyRecoveryRespectsAccountLease(t *testing.T) {
	for _, live := range []bool{false, true} {
		t.Run(map[bool]string{false: "startup", true: "discovery"}[live], func(t *testing.T) {
			m, vault, id, path := privateRecoveryReadbackFixture(t, "add-password", StatusFailed, false)
			const origin, email = "https://password-recovery-lock.example.test", "private-result@example.test"
			release, err := accountlock.Acquire(origin, email)
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			acquisitions := 0
			m.SetAccountOperationLease(func(got string) (func() error, error) {
				acquisitions++
				if got != email {
					t.Fatal("password recovery requested another account's lease")
				}
				return accountlock.Acquire(origin, got)
			})
			before, _ := vault.memoryVault.Load(email)
			original, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			restorePrivateRecoveryFixture(t, m, live)
			job, _ := m.Get(id)
			after, _ := vault.memoryVault.Load(email)
			retained, readErr := os.ReadFile(path)
			if acquisitions != 1 || vault.saves != 0 || !reflect.DeepEqual(before, after) || job.OperationStatus == "succeeded" || job.passwordCleanup || readErr != nil || !bytes.Equal(retained, original) {
				t.Fatal("password-only recovery bypassed the live account lease or discarded recovery evidence")
			}
			if job.TOTPRecoveryPending || job.TOTPCredentialInvalidated || job.PasswordAddError == "" {
				t.Fatal("password lock conflict misclassified TOTP or lost its password diagnostic")
			}
			if err := release(); err != nil {
				t.Fatal(err)
			}
			restarted := NewManager(t.Context(), 1, m.store, nil)
			restarted.SetCredentialStore(vault)
			restarted.SetAccountOperationLease(m.accountOperationLease)
			restorePrivateRecoveryFixture(t, restarted, false)
			completed, _ := restarted.Get(id)
			if completed.OperationStatus != "succeeded" || vault.saves != 1 {
				t.Fatal("released account lease did not allow verified local recovery")
			}
		})
	}
}

func TestPasswordRecoveryPreservesChangedEvidence(t *testing.T) {
	for _, changeAt := range []string{"lease", "load", "new-totp", "missing-lease"} {
		t.Run(changeAt, func(t *testing.T) {
			m, vault, id, path := privateRecoveryReadbackFixture(t, "add-password", StatusFailed, false)
			const email = "private-result@example.test"
			before, _ := vault.memoryVault.Load(email)
			original, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			changed := []byte(`{"version":1,"email":"private-result@example.test","password":"Synthetic-later-password9!"}`)
			changePassword := func() {
				if err := os.WriteFile(path, changed, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			released := false
			acquire := func(string) (func() error, error) {
				switch changeAt {
				case "lease":
					changePassword()
				case "new-totp":
					if err := os.WriteFile(filepath.Join(m.store.Root, id, "totp-setup-result.json"), []byte(`{"version":1,"email":"private-result@example.test","reset_totp_requested":true}`), 0o600); err != nil {
						t.Fatal(err)
					}
				case "missing-lease":
					return nil, nil
				}
				return func() error { released = true; return nil }, nil
			}
			var protected credentials.Store = vault
			if changeAt == "load" {
				protected = &passwordRecoveryLoadHook{Store: vault, load: changePassword}
			}
			job := &Job{ID: id, Email: email, Operation: "add-password"}
			recovered, err := recoverPrivateResult(t.Context(), m.store.Root, job, protected, acquire)
			after, _ := vault.memoryVault.Load(email)
			if err == nil || recovered.password || job.passwordCleanup || vault.saves != 0 || !reflect.DeepEqual(before, after) {
				t.Fatal("recovery wrote credentials or scheduled cleanup after losing its lease/evidence")
			}
			if released != (changeAt != "missing-lease") {
				t.Fatal("recovery leaked its account lease")
			}
			want := original
			if changeAt == "lease" || changeAt == "load" {
				want = changed
			}
			if retained, err := os.ReadFile(path); err != nil || !bytes.Equal(retained, want) {
				t.Fatal("changed private result was removed or replaced", err)
			}
			if changeAt == "new-totp" {
				if !job.TOTPRecoveryPending || !recovered.totpReadOnly {
					t.Fatal("new reset evidence failed to quarantine recovery")
				}
			} else if !recovered.passwordError || job.TOTPRecoveryPending || errors.Is(err, os.ErrNotExist) {
				t.Fatal("password recovery failure lost its classification")
			}
		})
	}
}

func TestPrivateRecoveryDefersNewTOTPResultUntilLeasedPass(t *testing.T) {
	m, vault, id, path := privateRecoveryReadbackFixture(t, "setup-2fa", StatusFailed, false)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	const email = "private-result@example.test"
	before, _ := vault.memoryVault.Load(email)
	newResult := []byte(`{"version":1,"email":"private-result@example.test","secret":"SYNTHETICLATERSEED","activation_succeeded":true}`)
	appeared := false
	protected := &passwordRecoveryLoadHook{Store: vault, load: func() {
		if !appeared {
			appeared = true
			if err := os.WriteFile(path, newResult, 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}}
	acquisitions := 0
	acquire := func(string) (func() error, error) {
		acquisitions++
		return func() error { return nil }, nil
	}
	job := &Job{ID: id, Email: email, Operation: "setup-2fa"}
	recovered, err := recoverPrivateResult(t.Context(), m.store.Root, job, protected, acquire)
	after, _ := vault.memoryVault.Load(email)
	if err != nil || recovered.totp || job.totpSetupCleanup || vault.saves != 0 || !reflect.DeepEqual(before, after) {
		t.Fatalf("recovery applied a newly appeared result without a verified lease: acquisitions=%d saves=%d err=%v", acquisitions, vault.saves, err)
	}
	if retained, err := os.ReadFile(path); err != nil || !bytes.Equal(retained, newResult) {
		t.Fatal("newly appeared result lost its next recovery opportunity", err)
	}
	recovered, err = recoverPrivateResult(t.Context(), m.store.Root, job, vault, acquire)
	if err != nil || !recovered.totp || !job.totpSetupCleanup || vault.saves != 1 || acquisitions != 1 {
		t.Fatal("next leased pass could not complete the retained result", err)
	}
}
