package protocol

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"testing/synctest"

	"tosub2-go-port/internal/transport"
)

func TestResetTOTPWaitsForStateVisibilityInTheSameSession(t *testing.T) {
	for _, delayed := range []string{"disabled", "activated", "both"} {
		t.Run(delayed, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				x := newResetFixture(t, true)
				activated := false
				disabledReads, activeReads := 0, 0
				x.flow.Options.Input = func(context.Context, string, string) (string, error) {
					t.Fatal("status verification attempted another login or OTP")
					return "", nil
				}
				x.override = func(step string, r transport.Request) (*transport.Response, error, bool) {
					if step == "disable" && (delayed == "disabled" || delayed == "both") {
						x.remoteFactor = ""
						return response(200, `{"message":"outcome unavailable"}`), nil, true
					}
					if step == "activate" {
						activated = true
						if delayed == "activated" || delayed == "both" {
							x.remoteFactor = "new-factor"
							return response(200, `{}`), nil, true
						}
					}
					if !strings.HasPrefix(step, "info") || x.info == 1 {
						return nil, nil, false
					}
					if r.Method != "GET" || r.Headers.Get("Cache-Control") != "no-cache, no-store" {
						t.Fatal("state polling was not an uncached read")
					}
					if !activated {
						disabledReads++
						if (delayed == "disabled" || delayed == "both") && disabledReads < 3 {
							return response(200, resetInfo("old-factor")), nil, true
						}
						return response(200, `{"mfa_enabled_v2":false}`), nil, true
					}
					activeReads++
					if (delayed == "activated" || delayed == "both") && activeReads < 3 {
						return response(200, resetInfo("")), nil, true
					}
					return response(200, resetInfo("new-factor")), nil, true
				}
				out, err := x.flow.ResetTOTP(t.Context(), x.path)
				if err != nil || out["credential_persisted"] != true || out["logout_all_devices_status"] != "succeeded" {
					t.Fatalf("state visibility interrupted reset: %v", err)
				}
				wantDisabled, wantActive := 0, 0
				if delayed == "disabled" || delayed == "both" {
					wantDisabled = 3
				}
				if (out["old_factor_removal_acknowledged"] == true) != (wantDisabled == 0) || (out["old_factor_removal_confirmed_by_read"] == true) != (wantDisabled > 0) {
					t.Fatal("removal acknowledgement and live-read evidence were conflated")
				}
				if delayed == "activated" || delayed == "both" {
					wantActive = 3
				}
				if disabledReads != wantDisabled || activeReads != wantActive {
					t.Fatalf("unexpected read counts: %d/%d", disabledReads, activeReads)
				}
				for _, step := range []string{"page", "session", "disable", "enroll", "activate", "logout"} {
					if got := countResetSteps(x.calls, step); got != 1 {
						t.Fatalf("%s repeated %d times", step, got)
					}
				}
			})
		})
	}
}

func TestResetTOTPUncertainRemovalStatusExhaustionStopsBeforeEnrollment(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		x := newResetFixture(t, true)
		reads := 0
		x.override = func(step string, _ transport.Request) (*transport.Response, error, bool) {
			if step == "disable" {
				x.remoteFactor = ""
				return response(200, `{"message":"outcome unavailable"}`), nil, true
			}
			if strings.HasPrefix(step, "info") && x.info > 1 {
				reads++
				return response(200, `{}`), nil, true
			}
			return nil, nil, false
		}
		out, err := x.flow.ResetTOTP(t.Context(), x.path)
		if err == nil || reads != totpStatusChecks || x.pending || x.persisted || !strings.Contains(err.Error(), "尚未创建新密钥") || !strings.Contains(err.Error(), "mfa_enabled_v2=absent") {
			t.Fatalf("unknown state was not bounded or diagnostic: reads=%d err=%v", reads, err)
		}
		saved := readResetResult(t, x.path)
		if saved["old_factor_removal_acknowledged"] == true || saved["old_factor_removal_confirmed_by_read"] == true || saved["reset_totp_last_phase"] != "disable_unconfirmed" || out["old_factor_invalidated"] != true || saved["logout_all_devices_status"] != "skipped" || payload(saved["disable_receipt"])["body_complete"] != true {
			t.Fatal("unconfirmed removal lost its receipt or was promoted to a confirmed state")
		}
		before := len(x.calls)
		if _, err := x.flow.ResetTOTP(t.Context(), x.path); err == nil || len(x.calls) != before {
			t.Fatal("status failure replayed a security mutation")
		}
	})
}

func TestResetTOTPActivationVerificationFailurePreservesNewCredentialWithoutRepeatingMutations(t *testing.T) {
	for _, scenario := range []struct {
		name  string
		body  string
		reads int
	}{
		{"unknown", `{}`, totpStatusChecks},
		{"contradictory", `{"mfa_enabled_v2":false,"factors":{"totp":[{"id":"new-factor","factor_type":"totp","is_recovery":false}]}}`, totpStatusChecks},
		{"enabled-without-factor", `{"mfa_enabled_v2":true,"factors":{"totp":[]}}`, totpStatusChecks},
		{"old-factor-still-visible", resetInfo("old-factor"), totpStatusChecks},
		{"unrelated-factor", resetInfo("unrelated-factor"), 1},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				x := newResetFixture(t, true)
				activated, reads := false, 0
				x.flow.Options.Input = func(context.Context, string, string) (string, error) {
					t.Fatal("activation verification attempted another login or OTP")
					return "", nil
				}
				x.override = func(step string, r transport.Request) (*transport.Response, error, bool) {
					if step == "activate" {
						activated, x.remoteFactor = true, "new-factor"
						return response(200, `{}`), nil, true
					}
					if !activated || !strings.HasPrefix(step, "info") {
						return nil, nil, false
					}
					if r.Method != "GET" || r.Headers.Get("Cache-Control") != "no-cache, no-store" {
						t.Fatal("activation verification was not an uncached read")
					}
					reads++
					return response(200, scenario.body), nil, true
				}
				out, err := x.flow.ResetTOTP(t.Context(), x.path)
				if err == nil || reads != scenario.reads || !x.pending || x.persisted || !strings.Contains(err.Error(), "新密钥和登记材料已保留") {
					t.Fatalf("unverified activation continued or lost pending material: reads=%d err=%v", reads, err)
				}
				if strings.Contains(err.Error(), resetFixtureSecret) {
					t.Fatal("activation diagnostic exposed the new credential")
				}
				saved := readResetResult(t, x.path)
				for _, result := range []map[string]any{out, saved} {
					if result["activation_succeeded"] != false || result["activation_verified"] != false || result["credential_persisted"] != false || result["secret"] != resetFixtureSecret || result["new_totp_factor_id"] != "new-factor" {
						t.Fatal("unknown activation or pending credential was not retained")
					}
					if result["reset_totp_status"] != "unknown" || result["reset_totp_phase"] != "manual_review_required" || result["reset_totp_last_phase"] != "activation_unconfirmed" || result["logout_all_devices_status"] != "skipped" {
						t.Fatal("unverified activation was not quarantined at its actual phase")
					}
				}
				for _, step := range []string{"page", "session", "disable", "enroll", "activate"} {
					if got := countResetSteps(x.calls, step); got != 1 {
						t.Fatalf("%s repeated %d times", step, got)
					}
				}
				if countResetSteps(x.calls, "logout") != 0 {
					t.Fatal("unverified activation logged out the recovery session")
				}
				before := len(x.calls)
				if _, err := x.flow.ResetTOTP(t.Context(), x.path); err == nil || len(x.calls) != before {
					t.Fatal("unverified activation allowed a reset replay")
				}
			})
		})
	}
}

func TestResetTOTPStatusRefusalAndCancellationDoNotReauthenticate(t *testing.T) {
	for _, scenario := range []string{"401", "403", "cancel"} {
		t.Run(scenario, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				x := newResetFixture(t, false)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				reads := 0
				x.flow.Options.Event = func(stage, _, _ string) {
					if stage == "totp_reset_status_pending" && scenario == "cancel" {
						cancel()
					}
				}
				x.override = func(step string, _ transport.Request) (*transport.Response, error, bool) {
					if step == "disable" {
						x.remoteFactor = ""
						return response(200, `{"message":"outcome unavailable"}`), nil, true
					}
					if strings.HasPrefix(step, "info") && x.info > 1 {
						reads++
						if scenario == "cancel" {
							return response(200, resetInfo("old-factor")), nil, true
						}
						code := 401
						if scenario == "403" {
							code = 403
						}
						return response(code, `{"error":"private response"}`), nil, true
					}
					return nil, nil, false
				}
				_, err := x.flow.ResetTOTP(ctx, x.path)
				if err == nil || reads != 1 || x.pending || x.persisted || countResetSteps(x.calls, "page") != 1 || countResetSteps(x.calls, "disable") != 1 || countResetSteps(x.calls, "enroll") != 0 {
					t.Fatalf("refusal/cancellation resumed unsafe work: %s %v", fmt.Sprint(x.calls), err)
				}
			})
		})
	}
}

func countResetSteps(calls []string, wanted string) int {
	count := 0
	for _, step := range calls {
		if step == wanted {
			count++
		}
	}
	return count
}
