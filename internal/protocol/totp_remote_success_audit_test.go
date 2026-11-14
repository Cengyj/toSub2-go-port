package protocol

import (
	"context"
	"errors"

	"testing"

	"tosub2-go-port/internal/transport"
)

// The remote factor changes BEFORE the response is interpreted. Known semantic
// variants advance directly; unknown outcomes reconcile without replaying writes.

// Inject a local atomic-replacement failure AFTER the remote mutation.
// Retain the preceding durable bytes separately for explicit inspection.

func TestAuditResetTOTPDisabledAccountThenEnrollmentOrActivationFails(t *testing.T) {
	for _, tc := range []struct {
		name, step string
		status     int
		body       string
		wantSeed   bool
	}{
		{"enroll_http_refusal", "enroll", 403, `{"error":"forbidden"}`, false},
		{"enroll_valid_seed_missing_recovery_flag", "enroll", 200, `{"secret":"` + resetFixtureSecret + `","session_id":"new-session","factor":{"id":"new-factor","factor_type":"totp"}}`, true},
		{"activate_http_refusal", "activate", 400, `{"error":"invalid_code"}`, true},
		{"activate_explicit_false", "activate", 200, `{"success":false}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x := newResetFixture(t, false)
			remoteFactor, remotePending := "old-factor", false
			x.override = func(step string, _ transport.Request) (*transport.Response, error, bool) {
				if step == "disable" {
					remoteFactor = ""
				}
				if step == "enroll" && (tc.step != "enroll" || tc.status == 200) {
					remotePending = true
				}
				if step == tc.step {
					return response(tc.status, tc.body), nil, true
				}
				return nil, nil, false
			}
			out, err := x.flow.ResetTOTP(t.Context(), x.path)
			if err == nil || remoteFactor != "" || out["reset_totp_phase"] != "manual_review_required" || x.persisted {
				t.Fatal("disabled account failure not reproduced")
			}
			saved := readResetResult(t, x.path)
			if (str(saved["secret"]) == resetFixtureSecret) != tc.wantSeed {
				t.Fatal("unexpected seed preservation")
			}
			if tc.name == "enroll_valid_seed_missing_recovery_flag" && (!remotePending || saved["secret"] != resetFixtureSecret || saved["session_id"] != "new-session" || x.pending || saved["enrollment_validated"] != false) {
				t.Fatal("malformed enrollment did not preserve recognizable material while blocking activation")
			}
			t.Logf("remote_factor=disabled remote_pending=%t local_seed_saved=%t status=%v second_start_blocked=%t", remotePending, tc.wantSeed, saved["reset_totp_status"], ValidateTOTPResetStart(x.path, x.flow.Options.Email) != nil)
		})
	}
}

func TestAuditResetTOTPRemoteActivationThenVaultCommitFails(t *testing.T) {
	x := newResetFixture(t, true)
	remoteFactor := "old-factor"
	x.override = func(step string, _ transport.Request) (*transport.Response, error, bool) {
		if step == "disable" {
			remoteFactor = ""
		}
		if step == "activate" {
			remoteFactor = "new-factor"
		}
		return nil, nil, false
	}
	x.flow.Options.TOTPResetPersist = func(context.Context, string, string) error {
		return errors.New("injected protected-vault failure")
	}
	out, err := x.flow.ResetTOTP(t.Context(), x.path)
	saved := readResetResult(t, x.path)
	if err == nil || remoteFactor != "new-factor" || saved["secret"] != resetFixtureSecret || saved["activation_verified"] != true || out["credential_persisted"] != false || saved["reset_totp_status"] != "unknown" || saved["logout_all_devices_status"] != "skipped" {
		t.Fatal("active remote factor and durable seed were not retained after vault failure")
	}
	if ValidateTOTPResetStart(x.path, x.flow.Options.Email) == nil {
		t.Fatal("uncommitted active factor allowed reset replay")
	}
	t.Log("remote_factor=new-factor journal_seed_saved=true activation_verified=true vault_commit=false logout=false second_start_blocked=true")
}
