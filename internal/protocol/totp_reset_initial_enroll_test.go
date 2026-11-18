package protocol

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"

	"tosub2-go-port/internal/resetlog"
	"tosub2-go-port/internal/transport"
)

func TestResetInitialStateLagRecoversBeforeAnyMutation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		x := newResetFixture(t, false)
		var events []string
		x.flow.Options.Event = func(stage, _, _ string) { events = append(events, stage) }
		x.override = func(step string, _ transport.Request) (*transport.Response, error, bool) {
			if step == "info1" {
				return response(200, `{}`), nil, true
			}
			return nil, nil, false
		}
		out, err := x.flow.ResetTOTP(t.Context(), x.path)
		if err != nil || out["credential_persisted"] != true || x.info != 2 {
			t.Fatalf("lag recovery failed: %v", err)
		}
		if countResetSteps(x.calls, "disable") != 1 || countResetSteps(x.calls, "enroll") != 1 {
			t.Fatal("lag retried a mutation")
		}
		positions := map[string]int{}
		for i, event := range events {
			positions[event] = i + 1
		}
		if positions["totp_login_complete"] == 0 || positions["totp_login_complete"] >= positions["totp_reset_read"] || positions["totp_reset_read"] >= positions["totp_reset_disable"] || positions["totp_reset_disabled"] >= positions["totp_reset_activate"] {
			t.Fatal("login/mutation milestone order is misleading", events)
		}
	})
}

func TestResetInitialEnrollmentCompletesWithoutRemovingOrRepeatingEnrollment(t *testing.T) {
	for _, body := range []string{`{}`, `{"mfa_enabled_v2":false}`, `{"mfa_enabled_v2":true}`} {
		t.Run(body, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				x := newResetFixture(t, true)
				x.override = func(step string, _ transport.Request) (*transport.Response, error, bool) {
					if strings.HasPrefix(step, "info") && x.remoteFactor != "new-factor" {
						return response(200, body), nil, true
					}
					if step == "enroll" {
						saved := readResetResult(t, x.path)
						if !x.intent || saved["initial_enroll_dispatch"] != "may_have_been_sent" || saved["initial_enroll_attempted"] != true || saved["disable_dispatch"] != "not_sent" {
							t.Fatal("enrollment sent without durable isolated evidence")
						}
					}
					return nil, nil, false
				}
				out, err := x.flow.ResetTOTP(t.Context(), x.path)
				if err != nil {
					t.Fatal(err)
				}
				wantReads := totpStatusChecks
				if body == `{"mfa_enabled_v2":false}` {
					wantReads = 1
				}
				if x.info != wantReads || !x.persisted || out["credential_persisted"] != true || out["enrollment_source"] != "initial_enroll" {
					t.Fatal("enroll-first did not commit")
				}
				if countResetSteps(x.calls, "disable") != 0 || countResetSteps(x.calls, "enroll") != 1 || countResetSteps(x.calls, "activate") != 1 || countResetSteps(x.calls, "logout") != 1 {
					t.Fatal("unexpected mutation sequence", x.calls)
				}
				if out["old_totp_factor_id"] != nil || out["enroll_attempted"] != false || out["enroll_receipt"] != nil {
					t.Fatal("fabricated a removal or second enrollment")
				}
				if _, err := ReadTOTPResetCompletion(x.path, x.flow.Options.Email); err != nil {
					t.Fatal("local completion cannot read enroll-first evidence", err)
				}
				before := len(x.calls)
				if _, err := x.flow.ResetTOTP(t.Context(), x.path); err == nil || len(x.calls) != before {
					t.Fatal("completed enroll-first was replayed")
				}
			})
		})
	}
}

func TestResetInitialEnrollmentConflictRefreshesThenRemovesExactlyOnce(t *testing.T) {
	for _, status := range []int{200, 400, 409, 422} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				x := newResetFixture(t, false)
				enrolls := 0
				x.flow.Options.TOTPFactorID = "old-factor" // Independent binding, not the conflict GET's default.
				x.override = func(step string, _ transport.Request) (*transport.Response, error, bool) {
					if strings.HasPrefix(step, "info") && x.info <= totpStatusChecks+1 {
						return response(200, `{}`), nil, true
					}
					if step == "enroll" {
						enrolls++
						if enrolls == 1 {
							return response(status, `{"error":{"code":"totp_already_enabled"}}`), nil, true
						}
					}
					if step == "disable" {
						saved := readResetResult(t, x.path)
						if !resetInitialEnrollmentConflict(saved) || saved["enroll_attempted"] != false || saved["old_totp_factor_id"] != "old-factor" {
							t.Fatal("removal lacks explicit rejection and refreshed target")
						}
					}
					return nil, nil, false
				}
				out, err := x.flow.ResetTOTP(t.Context(), x.path)
				if err != nil || !x.persisted {
					t.Fatal("conflict fallback failed", err)
				}
				if x.info != totpStatusChecks+2 || enrolls != 2 || countResetSteps(x.calls, "disable") != 1 || countResetSteps(x.calls, "activate") != 1 {
					t.Fatal("unexpected conflict sequence", x.calls)
				}
				if !resetInitialEnrollmentConflict(out) || str(out["initial_enroll_attempt_id"]) == str(out["enroll_attempt_id"]) || out["initial_enroll_receipt"] == nil || out["enroll_receipt"] == nil {
					t.Fatal("first attempt evidence overwritten")
				}
			})
		})
	}
}

func TestResetInitialEnrollmentUncertainOrGenericFailureNeverRemoves(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		body    string
		network bool
	}{
		{"generic-conflict", 409, `{"error":"conflict"}`, false},
		{"server-error", 500, `{"error":"totp_already_enabled"}`, false},
		{"unauthorized", 401, `{"error":"totp_already_enabled"}`, false},
		{"access-denied", 403, `{"error":"totp_already_enabled"}`, false},
		{"lost-response", 0, "", true},
		{"empty-success", 200, `{}`, false},
		{"material-with-conflict", 200, `{"error":"totp_already_enabled","secret":"JBSWY3DPEHPK3PXP"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				x := newResetFixture(t, false)
				x.override = func(step string, _ transport.Request) (*transport.Response, error, bool) {
					if strings.HasPrefix(step, "info") {
						return response(200, `{}`), nil, true
					}
					if step == "enroll" {
						if tc.network {
							return nil, errors.New("connection lost"), true
						}
						return response(tc.status, tc.body), nil, true
					}
					return nil, nil, false
				}
				out, err := x.flow.ResetTOTP(t.Context(), x.path)
				if err == nil || out["old_factor_invalidated"] != true || countResetSteps(x.calls, "enroll") != 1 || countResetSteps(x.calls, "disable") != 0 || countResetSteps(x.calls, "activate") != 0 || x.info != totpStatusChecks {
					t.Fatal("uncertain enrollment proceeded", x.calls, err)
				}
				if tc.name == "material-with-conflict" && out["secret"] != resetFixtureSecret {
					t.Fatal("recognized material was discarded")
				}
				before := len(x.calls)
				if _, err := x.flow.ResetTOTP(t.Context(), x.path); err == nil || len(x.calls) != before {
					t.Fatal("uncertain initial enrollment replayed")
				}
			})
		})
	}
}

func TestResetInitialEnrollmentConflictWithoutTargetNeverRemoves(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		x := newResetFixture(t, false)
		x.override = func(step string, _ transport.Request) (*transport.Response, error, bool) {
			if strings.HasPrefix(step, "info") {
				return response(200, `{}`), nil, true
			}
			if step == "enroll" {
				return response(409, `{"error":"totp_already_enabled"}`), nil, true
			}
			return nil, nil, false
		}
		_, err := x.flow.ResetTOTP(t.Context(), x.path)
		if err == nil || !strings.Contains(err.Error(), "未发送移除请求") || !strings.Contains(err.Error(), "账号安全设置") || countResetSteps(x.calls, "disable") != 0 || countResetSteps(x.calls, "enroll") != 1 || x.info != totpStatusChecks*2 {
			t.Fatal("conflict without target continued", x.calls, err)
		}
	})
}

func TestResetSecondEnrollmentCannotReplayOrDropFirstReceipt(t *testing.T) {
	for _, mode := range []string{"canceled-before-second", "lost-second", "partial-second"} {
		t.Run(mode, func(t *testing.T) {
			x := newResetFixture(t, true)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			x.flow.Options.EventData = func(_, _, _ string, data map[string]any) {
				if d, ok := data[resetlog.DataKey].(*resetlog.Detail); ok && d.Reason == "CONFLICT_STATE_DISABLED" && mode == "canceled-before-second" {
					cancel()
				}
			}
			enrolls := 0
			x.override = func(step string, _ transport.Request) (*transport.Response, error, bool) {
				if step != "enroll" {
					return nil, nil, false
				}
				enrolls++
				if enrolls == 1 {
					return response(409, `{"error":{"code":"totp_already_enrolled"}}`), nil, true
				}
				if mode == "lost-second" {
					return nil, errors.New("synthetic disconnected write"), true
				}
				return response(200, `{"session_id":"partial-only"}`), nil, true
			}
			_, err := x.flow.ResetTOTP(ctx, x.path)
			want := 2
			if mode == "canceled-before-second" {
				want = 1
				if !errors.Is(err, context.Canceled) {
					t.Fatal("cancel cause lost")
				}
			}
			if err == nil || enrolls != want || x.info != 2 || countResetSteps(x.calls, "disable") != 1 || countResetSteps(x.calls, "activate") != 0 || countResetSteps(x.calls, "logout") != 0 {
				t.Fatal("incomplete retry advanced or replayed", x.calls)
			}
			stored, err := ReadTOTPResetIdentity(x.path, x.flow.Options.Email)
			if err != nil || !resetEnrollmentConflict(stored, "enroll") || stored["enrollment_source"] != "retry_enroll" {
				t.Fatal("first refusal or attempt identity lost", err)
			}
			d := InterruptedResetLog(stored, x.flow.Options.Email, 1)
			if d == nil || d.JournalWrites == nil {
				t.Fatal("unreadable interrupted evidence")
			}
			w := d.JournalWrites.Enroll
			if w.Received+w.Possible != want || (mode == "lost-second") != (w.Possible == 1) {
				t.Fatal("interrupted writes forged a receipt or forgot an attempt", w)
			}
			before := len(x.calls)
			if _, err := x.flow.ResetTOTP(t.Context(), x.path); err == nil || len(x.calls) != before {
				t.Fatal("restart replayed a write")
			}
			if _, err := ReadTOTPResetCompletion(x.path, x.flow.Options.Email); err == nil {
				t.Fatal("lost or partial enrollment authorized local completion")
			}
			delete(stored, "enroll_receipt")
			if validateResetMutationEvidence(stored) == nil {
				t.Fatal("a claimed retry outlived its bound first refusal")
			}
		})
	}
}

func TestResetInitialEnrollmentStorageFailurePreventsRequest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		x := newResetFixture(t, false)
		x.flow.Options.TOTPResetBeforeDisable = func(context.Context) error { return errors.New("storage failed") }
		x.override = func(step string, _ transport.Request) (*transport.Response, error, bool) {
			if strings.HasPrefix(step, "info") {
				return response(200, `{}`), nil, true
			}
			return nil, nil, false
		}
		_, err := x.flow.ResetTOTP(t.Context(), x.path)
		if err == nil || countResetSteps(x.calls, "enroll") != 0 || countResetSteps(x.calls, "disable") != 0 {
			t.Fatal("storage failure allowed mutation")
		}
	})
}

func TestResetInitialEnrollmentActivationRequiresUniqueNewFactor(t *testing.T) {
	for _, body := range []string{`{}`, resetInfo("old-factor"),
		`{"mfa_enabled_v2":true,"factors":{"totp":[{"id":"new-factor","factor_type":"totp","is_recovery":false},{"id":"new-factor","factor_type":"totp","is_recovery":false}]}}`,
		`{"mfa_enabled_v2":true,"factors":{"totp":[{"id":"new-factor","factor_type":"totp","is_recovery":true},{"id":"other","factor_type":"totp","is_recovery":false}]}}`,
		`{"mfa_enabled_v2":true,"factors":{"totp":[{"id":"new-factor","factor_type":"totp","is_recovery":false},{"id":7,"factor_type":"totp","is_recovery":false}]}}`} {
		t.Run(body, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				x := newResetFixture(t, false)
				x.override = func(step string, _ transport.Request) (*transport.Response, error, bool) {
					if step == "activate" {
						x.remoteFactor = "new-factor"
						return response(200, `{}`), nil, true
					}
					if strings.HasPrefix(step, "info") {
						if x.remoteFactor == "new-factor" {
							return response(200, body), nil, true
						}
						return response(200, `{}`), nil, true
					}
					return nil, nil, false
				}
				out, err := x.flow.ResetTOTP(t.Context(), x.path)
				if err == nil || out["activation_succeeded"] == true || out["activation_verified"] != false || x.persisted || !x.pending || out["secret"] != resetFixtureSecret {
					t.Fatal("unconfirmed unique factor reported reset success", err)
				}
				if countResetSteps(x.calls, "disable") != 0 || countResetSteps(x.calls, "enroll") != 1 || countResetSteps(x.calls, "activate") != 1 {
					t.Fatal("unique-factor check replayed mutation", x.calls)
				}
				if _, err := ReadTOTPResetCompletion(x.path, x.flow.Options.Email); err == nil {
					t.Fatal("local completion ignored uniqueness")
				}
			})
		})
	}
}

func TestResetMultiFactorVisibilityIsNotRemovalAuthority(t *testing.T) {
	const mixed = `{"mfa_enabled_v2":true,"factors":{"totp":[{"id":"old-factor","factor_type":"totp","is_recovery":false},{"id":"new-factor","factor_type":"totp","is_recovery":false}]}}`
	for _, initial := range []bool{true, false} {
		synctest.Test(t, func(t *testing.T) {
			x := newResetFixture(t, false)
			x.override = func(step string, _ transport.Request) (*transport.Response, error, bool) {
				if step == "activate" {
					x.remoteFactor = "new-factor"
					return response(200, `{}`), nil, true
				}
				if strings.HasPrefix(step, "info") {
					if initial || x.remoteFactor == "new-factor" {
						return response(200, mixed), nil, true
					}
					return response(200, `{}`), nil, true
				}
				return nil, nil, false
			}
			out, err := x.flow.ResetTOTP(t.Context(), x.path)
			if countResetSteps(x.calls, "disable") != 0 {
				t.Fatal("multi-factor read authorized a removal")
			}
			if initial {
				if err == nil || !strings.Contains(err.Error(), "未移除任何验证器，请在账号安全设置中核对") || x.persisted || countResetSteps(x.calls, "enroll") != 0 {
					t.Fatal("ambiguous initial target accepted")
				}
				return
			}
			if err != nil || !x.persisted || out["activation_confirmation_source"] != "state_read" || out["activation_verified"] != true || countResetSteps(x.calls, "enroll") != 1 || countResetSteps(x.calls, "activate") != 1 {
				t.Fatal("unique bound member not confirmed", err)
			}
		})
	}
}

func TestResetInitialEnrollmentEvidenceCannotUseLegacyOrBeOverwritten(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		x := newResetFixture(t, false)
		x.override = func(step string, _ transport.Request) (*transport.Response, error, bool) {
			if strings.HasPrefix(step, "info") && x.remoteFactor != "new-factor" {
				return response(200, `{}`), nil, true
			}
			return nil, nil, false
		}
		_, err := x.flow.ResetTOTP(t.Context(), x.path)
		if err != nil {
			t.Fatal(err)
		}
		saved := readResetResult(t, x.path)
		delete(saved, "mutation_schema")
		if err := validateResetMutationEvidence(saved); err == nil {
			t.Fatal("new enrollment evidence bypassed validation as legacy")
		}
		for _, key := range []string{"initial_enroll_attempt_id", "initial_enroll_attempted_at", "initial_enroll_receipt", "initial_enroll_result"} {
			mixed := map[string]any{"version": 1, "email": x.flow.Options.Email, "operation_id": "mixed-operation", "operation_scope": "current_session", "reset_totp_requested": true, "mutation_schema": 1, "disable_dispatch": "not_sent", "enroll_dispatch": "not_sent", "activation_dispatch": "not_sent", "initial_enroll_dispatch": "not_sent", "initial_enroll_attempted": false, "enroll_attempted": false, "activation_attempted": false, "reset_totp_strategy": "enroll_first", "initial_totp_state": "unknown", "reset_totp_phase": "initial", "activation_succeeded": false, "activation_verified": false, "old_factor_invalidated": false, "credential_persisted": false}
			mixed[key] = saved[key]
			if err := x.flow.saveTOTPResetJournal(x.path, mixed); err != nil {
				t.Fatal(err)
			}
			if err := ValidateTOTPResetStart(x.path, x.flow.Options.Email); err == nil {
				t.Fatal("new evidence would be overwritten", key)
			}
		}
	})
}
