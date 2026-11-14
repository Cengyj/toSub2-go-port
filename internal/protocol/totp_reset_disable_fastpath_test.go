package protocol

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"

	"tosub2-go-port/internal/transport"
)

// These public response shapes match HAR entries 959 and 960. No captured
// account identifiers, cookies, tokens or authentication material are fixtures.
const harDisabledTOTPState = `{"mfa_enabled":false,"mfa_enabled_v2":false,"factors":{"totp":[]},"native_default_factor_id":null}`

func TestResetTOTPConfirmedRemovalContinuesWithoutStatusRead(t *testing.T) {
	x := newResetFixture(t, false)
	removed, enrolling, redundantReads := false, false, 0
	x.override = func(step string, _ transport.Request) (*transport.Response, error, bool) {
		switch {
		case step == "disable":
			removed, x.remoteFactor = true, ""
			return response(200, `{"message":"Factor disabled","passkey_credential_manager_sync":null}`), nil, true
		case removed && !enrolling && strings.HasPrefix(step, "info"):
			redundantReads++
			return response(500, `{"error":"temporary status failure"}`), nil, true
		case step == "enroll":
			enrolling = true
			saved := readResetResult(t, x.path)
			receipt := payload(saved["disable_receipt"])
			if saved["old_factor_removal_acknowledged"] != true || saved["old_factor_removal_confirmed_by_read"] == true || receipt["body_complete"] != true || receipt["http_status"] != float64(200) {
				t.Fatal("enrollment did not follow a durable acknowledgement, or invented a read confirmation")
			}
		}
		return nil, nil, false
	}
	out, err := x.flow.ResetTOTP(t.Context(), x.path)
	if err != nil || redundantReads != 0 || out["credential_persisted"] != true {
		t.Fatalf("successful removal was interrupted: redundant reads=%d err=%v", redundantReads, err)
	}
	want := []string{"page", "session", "info1", "disable", "enroll", "activate"}
	if !reflect.DeepEqual(x.calls, want) {
		t.Fatalf("unexpected request order: %v", x.calls)
	}
}

func TestResetTOTPUncertainRemovalChecksHARStateAfterTransientFailures(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		x := newResetFixture(t, false)
		removed, enrolling, reads := false, false, 0
		x.override = func(step string, r transport.Request) (*transport.Response, error, bool) {
			switch {
			case step == "disable":
				removed, x.remoteFactor = true, ""
				return response(500, `{"error":"outcome unavailable"}`), nil, true
			case removed && !enrolling && strings.HasPrefix(step, "info"):
				reads++
				if r.Method != http.MethodGet || r.Headers.Get("Cache-Control") != "no-cache, no-store" {
					t.Fatal("reconciliation did not use an uncached status GET")
				}
				if reads < 3 {
					return response(503, `{"error":"temporary"}`), nil, true
				}
				return response(200, harDisabledTOTPState), nil, true
			case step == "enroll":
				enrolling = true
				saved := readResetResult(t, x.path)
				if saved["old_factor_removal_acknowledged"] == true || saved["old_factor_removal_confirmed_by_read"] != true {
					t.Fatal("uncertain removal was not reconciled by the disabled-state response")
				}
			}
			return nil, nil, false
		}
		out, err := x.flow.ResetTOTP(t.Context(), x.path)
		if err != nil || reads != 3 || out["credential_persisted"] != true {
			t.Fatalf("status reconciliation failed: reads=%d err=%v", reads, err)
		}
		for _, step := range []string{"disable", "enroll", "activate"} {
			if countResetSteps(x.calls, step) != 1 {
				t.Fatalf("mutation repeated: %s", step)
			}
		}
	})
}

func TestResetTOTPConfirmedRemovalSaveFailureDoesNotEnroll(t *testing.T) {
	x := newResetFixture(t, false)
	blocked := false
	journalWriteHook(t, x, func(out map[string]any) error {
		if out["failure_site"] == "disable_response_journal" && out["old_factor_removal_acknowledged"] == true {
			blocked = true
			return errors.New("synthetic acknowledgement save failure")
		}
		return nil
	})
	out, err := x.flow.ResetTOTP(t.Context(), x.path)
	if err == nil || !blocked || out["old_factor_removal_acknowledged"] != true {
		t.Fatalf("acknowledgement save failure was not exercised: err=%v", err)
	}
	for _, step := range []string{"enroll", "activate", "logout"} {
		if countResetSteps(x.calls, step) != 0 {
			t.Fatalf("%s ran after acknowledgement save failed", step)
		}
	}
	saved := readResetResult(t, x.path)
	if saved["old_factor_removal_acknowledged"] == true || payload(saved["disable_receipt"])["body_complete"] != true {
		t.Fatal("last durable removal receipt was lost or unsaved acknowledgement was claimed")
	}
}

func TestTOTPStatusHTTPRetriesAreBoundedAndExcludeRefusals(t *testing.T) {
	for _, status := range []int{400, 401, 403, 429, 500, 501, 502, 503, 504} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				x := newResetFixture(t, false)
				reads := 0
				x.override = func(step string, r transport.Request) (*transport.Response, error, bool) {
					if !strings.HasPrefix(step, "info") || r.Method != http.MethodGet {
						t.Fatal("status retry made an authentication or mutation request")
					}
					reads++
					return response(status, `{"error":"fixture"}`), nil, true
				}
				_, err := x.flow.waitForTOTPResetState(t.Context(), resetFixtureToken, "current-account", totpStateDisabled, "", "old-factor")
				want := 1
				if status == 500 || status == 502 || status == 503 || status == 504 {
					want = totpStatusChecks
				}
				var failure *totpResetError
				if reads != want || !errors.As(err, &failure) || failure.httpStatus != status {
					t.Fatalf("unexpected HTTP budget: status=%d reads=%d err=%v", status, reads, err)
				}
			})
		})
	}
}

func TestTOTPStatusHTTPRetryStopsOnCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		x := newResetFixture(t, false)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		reads := 0
		x.override = func(string, transport.Request) (*transport.Response, error, bool) {
			reads++
			return response(500, "{}"), nil, true
		}
		x.flow.Options.Event = func(stage, _, _ string) {
			if stage == "totp_reset_status_pending" {
				cancel()
			}
		}
		_, err := x.flow.waitForTOTPResetState(ctx, resetFixtureToken, "current-account", totpStateDisabled, "", "old-factor")
		if reads != 1 || !errors.Is(err, context.Canceled) {
			t.Fatalf("status retry continued after cancellation: reads=%d err=%v", reads, err)
		}
	})
}
