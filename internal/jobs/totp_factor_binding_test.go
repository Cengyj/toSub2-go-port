package jobs

import (
	"testing"

	"tosub2-go-port/internal/credentials"
)

func TestCredentialInputPreservesOnlyMatchingFactorBindings(t *testing.T) {
	for _, tc := range []struct {
		name               string
		delta              map[string]any
		pending            bool
		wantSecret, wantID string
	}{
		{"unrelated update", map[string]any{"password": "new-synthetic-password"}, false, "OLDSEED", "old-factor"},
		{"identical seed", map[string]any{"totpSecret": "OLDSEED"}, false, "OLDSEED", "old-factor"},
		{"changed seed", map[string]any{"totpSecret": "NEWSEED", "totpFactorId": "untrusted-factor"}, false, "NEWSEED", ""},
		{"cleared seed", map[string]any{"totpSecret": ""}, false, "", ""},
		{"quarantine", map[string]any{"totpSecret": "NEWSEED"}, true, "OLDSEED", "old-factor"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := NewManager(t.Context(), 1, nil, nil)
			v := &memoryVault{}
			m.SetCredentialStore(v)
			j := &Job{ID: "binding", Email: "binding@example.test", Input: map[string]any{}}
			m.jobs[j.ID] = j
			old := credentials.Credentials{Password: "synthetic-password", TOTPSecret: "OLDSEED", TOTPFactorID: "old-factor", PendingTOTPSecret: "PENDINGSEED", PendingTOTPURI: "pending-uri", PendingTOTPFactorID: "pending-factor", TOTPResetPending: tc.pending}
			if err := v.Save(j.Email, old); err != nil {
				t.Fatal(err)
			}
			m.saveSecretsLocked(j, tc.delta)
			got, err := v.Load(j.Email)
			if err != nil || got.TOTPSecret != tc.wantSecret || got.TOTPFactorID != tc.wantID || got.PendingTOTPFactorID != old.PendingTOTPFactorID || got.PendingTOTPSecret != old.PendingTOTPSecret {
				t.Fatal("credential edit lost or transferred a factor binding", err)
			}
		})
	}
}

func TestCompletionRejectsMismatchedFactorBinding(t *testing.T) {
	for _, pending := range []bool{false, true} {
		for _, id := range []string{"", "matching-factor", "other-factor"} {
			result := map[string]any{"operation_id": "op", "secret": "SYNTHETICSEED", "otpauth_uri": "synthetic-uri", "new_totp_factor_id": "matching-factor"}
			c := credentials.Credentials{TOTPResetOperationID: "op", TOTPResetPending: pending}
			if pending {
				c.PendingTOTPSecret, c.PendingTOTPURI, c.PendingTOTPFactorID = "SYNTHETICSEED", "synthetic-uri", id
			} else {
				c.TOTPSecret, c.TOTPFactorID = "SYNTHETICSEED", id
			}
			if got := totpCompletionMatchesVault(result, c); got != (id != "other-factor") {
				t.Fatalf("binding match=%v pending=%v id=%q", got, pending, id)
			}
			if !pending {
				c.PendingTOTPFactorID = "orphan-pending-factor"
				if totpCompletionMatchesVault(result, c) {
					t.Fatal("orphan pending binding accepted as complete")
				}
			}
		}
	}
}

func TestLegacySetupRecoveryDoesNotTransferOldFactorBinding(t *testing.T) {
	for _, sameSeed := range []bool{false, true} {
		m, v, _, _ := privateRecoveryReadbackFixture(t, "setup-2fa", StatusFailed, false)
		c, _ := v.Load("private-result@example.test")
		c.TOTPSecret, c.TOTPFactorID, c.PendingTOTPFactorID = "OLDSEED", "old-factor", "pending-factor"
		if sameSeed {
			c.TOTPSecret = "SYNTHETICNEWSEED"
			c.PendingTOTPFactorID = "old-factor"
		}
		if err := v.memoryVault.Save("private-result@example.test", c); err != nil {
			t.Fatal(err)
		}
		restorePrivateRecoveryFixture(t, m, false)
		got, err := v.Load("private-result@example.test")
		if err != nil || got.TOTPSecret != "SYNTHETICNEWSEED" || got.PendingTOTPFactorID != "" || (sameSeed && got.TOTPFactorID != "old-factor") || (!sameSeed && got.TOTPFactorID != "") {
			t.Fatal("legacy recovery lost or transferred binding", err)
		}
	}
}
