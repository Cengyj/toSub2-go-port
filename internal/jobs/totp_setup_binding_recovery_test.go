package jobs

import (
	"bytes"
	"errors"
	"os"
	"reflect"
	"testing"

	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/protocol"
)

func setupBindingRecoveryRecord(t *testing.T) map[string]any {
	t.Helper()
	const email, secret = "private-result@example.test", "JBSWY3DPEHPK3PXP"
	uri, err := protocol.BuildTOTPURI(email, secret)
	if err != nil {
		t.Fatal(err)
	}
	return map[string]any{
		"version": 1, "email": email, "secret": secret, "otpauth_uri": uri,
		"operation_id": "setup-operation", "session_id": "setup-session", "activation_succeeded": true,
		"new_totp_factor_id": "new-factor", "enrollment_factor": map[string]any{"id": "new-factor", "factor_type": "totp", "is_recovery": false},
	}
}

func TestSetupRecoveryPromotesMatchingPendingFactorBinding(t *testing.T) {
	for _, live := range []bool{false, true} {
		t.Run(map[bool]string{false: "startup", true: "discovery"}[live], func(t *testing.T) {
			m, vault, id, path := privateRecoveryReadbackFixture(t, "setup-2fa", StatusFailed, false)
			record := setupBindingRecoveryRecord(t)
			before, _ := vault.memoryVault.Load("private-result@example.test")
			before.TOTPSecret, before.TOTPFactorID = "SYNTHETICOLDSEED", "old-factor"
			before.PendingTOTPSecret, before.PendingTOTPURI, before.PendingTOTPFactorID = resultString(record, "secret"), resultString(record, "otpauth_uri"), "new-factor"
			if err := vault.memoryVault.Save("private-result@example.test", before); err != nil {
				t.Fatal(err)
			}
			if err := protocol.WritePrivateJSON(path, record); err != nil {
				t.Fatal(err)
			}
			restorePrivateRecoveryFixture(t, m, live)
			after, err := vault.memoryVault.Load("private-result@example.test")
			want := before
			want.TOTPSecret, want.TOTPFactorID = before.PendingTOTPSecret, "new-factor"
			want.PendingTOTPSecret, want.PendingTOTPURI, want.PendingTOTPFactorID = "", "", ""
			job, ok := m.Get(id)
			if err != nil || !reflect.DeepEqual(after, want) || !ok || job.OperationStatus != "succeeded" || !job.HasTOTPKey {
				t.Fatal("interrupted setup lost its matching factor binding or unrelated credentials", err)
			}
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("committed setup recovery retained already promoted artifact", err)
			}
		})
	}
}

func TestSetupRecoveryRetainsConflictingCredentialAndEvidence(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*credentials.Credentials, map[string]any)
	}{
		{"newer-active-without-pending", func(c *credentials.Credentials, _ map[string]any) {
			c.PendingTOTPSecret, c.PendingTOTPURI, c.PendingTOTPFactorID = "", "", ""
		}},
		{"different-pending-seed", func(c *credentials.Credentials, _ map[string]any) { c.PendingTOTPSecret = "OTHERPENDINGSEED" }},
		{"different-pending-uri", func(c *credentials.Credentials, _ map[string]any) { c.PendingTOTPURI = "other-uri" }},
		{"different-pending-factor", func(c *credentials.Credentials, _ map[string]any) { c.PendingTOTPFactorID = "other-factor" }},
		{"same-seed-different-active-factor", func(c *credentials.Credentials, record map[string]any) { c.TOTPSecret = resultString(record, "secret") }},
		{"orphan-pending-factor", func(c *credentials.Credentials, _ map[string]any) { c.PendingTOTPSecret, c.PendingTOTPURI = "", "" }},
		{"missing-session", func(_ *credentials.Credentials, record map[string]any) { delete(record, "session_id") }},
		{"missing-operation", func(_ *credentials.Credentials, record map[string]any) { delete(record, "operation_id") }},
		{"partial-factor-material", func(_ *credentials.Credentials, record map[string]any) { delete(record, "enrollment_factor") }},
		{"legacy-stale-artifact", func(c *credentials.Credentials, record map[string]any) {
			c.PendingTOTPSecret, c.PendingTOTPURI, c.PendingTOTPFactorID = "", "", ""
			delete(record, "new_totp_factor_id")
			delete(record, "enrollment_factor")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, vault, id, path := privateRecoveryReadbackFixture(t, "setup-2fa", StatusFailed, false)
			record := setupBindingRecoveryRecord(t)
			before, _ := vault.memoryVault.Load("private-result@example.test")
			before.TOTPSecret, before.TOTPFactorID = "NEWERACTIVESEED", "active-factor"
			before.PendingTOTPSecret, before.PendingTOTPURI, before.PendingTOTPFactorID = resultString(record, "secret"), resultString(record, "otpauth_uri"), "new-factor"
			tc.change(&before, record)
			if err := vault.memoryVault.Save("private-result@example.test", before); err != nil {
				t.Fatal(err)
			}
			if err := protocol.WritePrivateJSON(path, record); err != nil {
				t.Fatal(err)
			}
			original, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			restorePrivateRecoveryFixture(t, m, false)
			after, err := vault.memoryVault.Load("private-result@example.test")
			job, ok := m.Get(id)
			retained, readErr := os.ReadFile(path)
			if err != nil || !reflect.DeepEqual(before, after) || vault.saves != 0 || !ok || job.OperationStatus != "failed" || job.TOTPSetupError == "" || job.totpSetupCleanup || readErr != nil || !bytes.Equal(original, retained) {
				t.Fatal("conflicting setup overwrote credentials, reported success, or lost evidence", err, readErr)
			}
		})
	}
}

func TestSetupRecoveryBindingCompatibility(t *testing.T) {
	for _, tc := range []struct {
		name       string
		legacy     bool
		activeSame bool
		pending    bool
		activeID   string
		pendingID  string
		wantID     string
		conflict   bool
	}{
		{name: "committed-binding", activeSame: true, activeID: "new-factor", wantID: "new-factor"},
		{name: "legacy-committed-binding", legacy: true, activeSame: true, activeID: "saved-factor", wantID: "saved-factor"},
		{name: "legacy-new-seed-clears-old-binding", legacy: true, pending: true, activeID: "old-factor", pendingID: "new-factor"},
		{name: "legacy-empty-vault", legacy: true},
		{name: "missing-pending-binding-remains-unbound", pending: true},
		{name: "same-seed-conflicting-bindings", legacy: true, activeSame: true, pending: true, activeID: "saved-factor", pendingID: "other-factor", conflict: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			record := setupBindingRecoveryRecord(t)
			if tc.legacy {
				delete(record, "new_totp_factor_id")
				delete(record, "enrollment_factor")
				delete(record, "otpauth_uri")
			}
			before := credentials.Credentials{TOTPFactorID: tc.activeID}
			if tc.activeSame {
				before.TOTPSecret = resultString(record, "secret")
			} else if tc.activeID != "" {
				before.TOTPSecret = "DIFFERENTOLDSEED"
			}
			if tc.pending {
				before.PendingTOTPSecret, before.PendingTOTPURI, before.PendingTOTPFactorID = resultString(record, "secret"), resultString(record, "otpauth_uri"), tc.pendingID
			}
			after, err := recoverSetupTOTPCredentials(before, record)
			if tc.conflict {
				if err == nil || !reflect.DeepEqual(before, after) {
					t.Fatal("conflicting binding was overwritten")
				}
				return
			}
			if err != nil || after.TOTPSecret != resultString(record, "secret") || after.TOTPFactorID != tc.wantID || after.PendingTOTPSecret != "" || after.PendingTOTPURI != "" || after.PendingTOTPFactorID != "" {
				t.Fatal("legacy compatibility lost material or transferred an unrelated factor ID", err)
			}
		})
	}
}
