package protocol

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"

	"tosub2-go-port/internal/transport"
)

type setupOutcomeFixture struct {
	t                   *testing.T
	flow                *Flow
	path                string
	calls               []string
	info, prompts       int
	initial, enrollment string
	enrollmentErr       error
	activation          *transport.Response
	activationErr       error
	state               string
	stateStatus         int
	sessionEmail        string
}

func newSetupOutcomeFixture(t *testing.T) *setupOutcomeFixture {
	t.Helper()
	x := &setupOutcomeFixture{t: t, path: filepath.Join(t.TempDir(), "setup.json"), initial: resetInfo(""),
		enrollment: `{"secret":"` + resetFixtureSecret + `","session_id":"setup-session","factor":{"id":"setup-factor","factor_type":"totp","is_recovery":false}}`,
		activation: response(200, `{"success":true}`), state: resetInfo("setup-factor"), stateStatus: 200, sessionEmail: "enroll@example.com"}
	x.flow = enrollmentFlow(t, doerFunc(x.request), FlowOptions{Input: func(context.Context, string, string) (string, error) {
		x.prompts++
		return "", errors.New("unexpected code prompt")
	}})
	x.flow.Client.Challenge = func(context.Context, string) error { t.Fatal("mutation challenge replayed"); return nil }
	return x
}

func (x *setupOutcomeFixture) request(_ context.Context, r transport.Request) (*transport.Response, error) {
	x.t.Helper()
	switch {
	case strings.Contains(r.URL, "?action=enable"):
		x.calls = append(x.calls, "page")
		return response(200, `{"accessToken":"`+resetFixtureToken+`"}`), nil
	case strings.HasSuffix(r.URL, "/api/auth/session"):
		x.calls = append(x.calls, "session")
		return fixtureJSON(map[string]any{"account": map[string]any{"id": "setup-account"}, "user": map[string]any{"email": x.sessionEmail}}), nil
	case strings.HasSuffix(r.URL, "/mfa_info"):
		x.calls = append(x.calls, "info")
		x.info++
		if x.info == 1 {
			return response(200, x.initial), nil
		}
		return response(x.stateStatus, x.state), nil
	case strings.HasSuffix(r.URL, "/mfa/enroll"):
		x.calls = append(x.calls, "enroll")
		if !r.SingleAttempt {
			x.t.Fatal("enrollment transport is replayable")
		}
		return response(200, x.enrollment), x.enrollmentErr
	case strings.HasSuffix(r.URL, "/activate_enrollment"):
		x.calls = append(x.calls, "activate")
		if !r.SingleAttempt {
			x.t.Fatal("activation transport is replayable")
		}
		var material map[string]any
		b, err := os.ReadFile(x.path)
		if err != nil || json.Unmarshal(b, &material) != nil || material["session_id"] != "setup-session" || material["activation_succeeded"] != false {
			x.t.Fatal("activation lacks durable enrollment session")
		}
		return x.activation, x.activationErr
	default:
		x.t.Fatal("unexpected setup request")
		return nil, errors.New("unexpected request")
	}
}

func TestSetupTOTPReceiptCompletesWithoutStatusRead(t *testing.T) {
	for _, body := range []string{`{"success":true}`, `{"success":true,"message":"wording changed","extra":null}`, `{"activated":true}`, `{"success":true,"factor_id":"setup-factor","session_id":"setup-session"}`} {
		t.Run(body, func(t *testing.T) {
			x := newSetupOutcomeFixture(t)
			x.activation = response(200, body)
			x.stateStatus, x.state = 503, `{}` // This unused display refresh cannot revoke success.
			out, err := x.flow.SetupTOTP(t.Context(), x.path)
			if err != nil || out["activation_succeeded"] != true || out["activation_confirmation_source"] != "business_receipt" || out["activation_confirmed_by_read"] == true || x.prompts != 0 {
				t.Fatalf("valid receipt did not complete directly: %v", err)
			}
			if !reflect.DeepEqual(x.calls, []string{"page", "session", "info", "enroll", "activate"}) {
				t.Fatalf("redundant request sequence: %v", x.calls)
			}
			b, err := os.ReadFile(x.path)
			var saved map[string]any
			if err != nil || json.Unmarshal(b, &saved) != nil || saved["activation_succeeded"] != true || saved["secret"] != resetFixtureSecret {
				t.Fatal("confirmed seed was not saved")
			}
		})
	}
}

func TestSetupTOTPUnknownActivationReconcilesOnlyMatchingFactor(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		err        error
	}{
		{"lost response", "", io.EOF},
		{"truncated JSON", `{"success":`, nil},
		{"unknown layout", `{"data":{"success":true}}`, nil},
		{"unknown success type", `{"success":"true"}`, nil},
		{"duplicate success", `{"success":false,"success":true}`, nil},
		{"contradictory receipt", `{"success":true,"error":{"code":"invalid_code"}}`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x := newSetupOutcomeFixture(t)
			x.activation, x.activationErr = response(200, tc.body), tc.err
			out, err := x.flow.SetupTOTP(t.Context(), x.path)
			if err != nil || out["activation_succeeded"] != true || out["activation_confirmation_source"] != "state_read" || out["activation_confirmed_by_read"] != true || x.prompts != 0 {
				t.Fatalf("unknown activation was not safely reconciled: %v", err)
			}
			if !reflect.DeepEqual(x.calls, []string{"page", "session", "info", "enroll", "activate", "info"}) {
				t.Fatalf("mutation replay or wrong reconciliation sequence: %v", x.calls)
			}
		})
	}
}

func TestSetupTOTPUnknownInitialStateNeverEnrolls(t *testing.T) {
	for _, body := range []string{`{}`, `null`, `{"mfa_enabled_v2":true,"mfa_enabled_v2":false}`, `{"mfa_enabled_v2":null}`, `{"mfa_enabled_v2":false,"factors":{"totp":[{"id":"existing"}]}}`, `{"mfa_enabled_v2":false,"error":"refused"}`} {
		t.Run(body, func(t *testing.T) {
			x := newSetupOutcomeFixture(t)
			x.initial = body
			if _, err := x.flow.SetupTOTP(t.Context(), x.path); err == nil || x.prompts != 0 || !reflect.DeepEqual(x.calls, []string{"page", "session", "info"}) {
				t.Fatalf("unknown state authorized enrollment: %v", x.calls)
			}
		})
	}
}

func TestSetupTOTPFailedAndUnboundActivationNeverRetriesCode(t *testing.T) {
	for _, name := range []string{"explicit false", "generic 400", "missing factor binding", "different factor", "different account", "unexplained status"} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				x := newSetupOutcomeFixture(t)
				x.activation = response(200, `{}`)
				want := []string{"page", "session", "info", "enroll", "activate"}
				switch name {
				case "explicit false":
					x.activation = response(200, `{"success":false}`)
				case "generic 400":
					x.activation = response(400, `{}`)
				case "missing factor binding":
					x.enrollment = `{"secret":"` + resetFixtureSecret + `","session_id":"setup-session"}`
				case "different factor":
					x.state = resetInfo("other-factor")
					want = append(want, "info")
				case "different account":
					x.sessionEmail = "other@example.com"
					want = []string{"page", "session"}
				case "unexplained status":
					x.state = `{}`
					want = append(want, "info", "info", "info", "info")
				}
				out, err := x.flow.SetupTOTP(t.Context(), x.path)
				if err == nil || out["activation_succeeded"] == true || x.prompts != 0 || !reflect.DeepEqual(x.calls, want) {
					t.Fatalf("unsafe activation fallback: calls=%v err=%v", x.calls, err)
				}
			})
		})
	}
}

func TestSetupTOTPIncompleteEnrollmentNeverActivates(t *testing.T) {
	for _, body := range []string{
		`{"secret":"` + resetFixtureSecret + `"}`,
		`{"session_id":"setup-session"}`,
		`{"secret":"` + resetFixtureSecret + `","session_id":"setup-session","factor":{"id":"setup-factor"}}`,
		`{"secret":"` + resetFixtureSecret + `","session_id":"setup-session","success":false}`,
		`{"secret":"` + resetFixtureSecret + `","session_id":"setup-session","error":"rejected"}`,
		`{"secret":"` + resetFixtureSecret + `","session_id":"setup-session","success":"true"}`,
		`{"secret":"` + resetFixtureSecret + `","session_id":"setup-session","factor":null}`,
		`{"secret":"` + resetFixtureSecret + `","session_id":"other-session","session_id":"setup-session"}`,
	} {
		t.Run("material", func(t *testing.T) {
			x := newSetupOutcomeFixture(t)
			x.enrollment = body
			if _, err := x.flow.SetupTOTP(t.Context(), x.path); err == nil || x.prompts != 0 || !reflect.DeepEqual(x.calls, []string{"page", "session", "info", "enroll"}) {
				t.Fatalf("invalid material was activated: %v", x.calls)
			}
		})
	}
}

func TestSetupTOTPLostEnrollmentResponseDoesNotReenroll(t *testing.T) {
	x := newSetupOutcomeFixture(t)
	x.enrollment, x.enrollmentErr = "", io.EOF
	out, err := x.flow.SetupTOTP(t.Context(), x.path)
	if err == nil || !strings.Contains(err.Error(), "TOTP_ENROLL_UNCONFIRMED") || out["activation_succeeded"] == true || x.prompts != 0 {
		t.Fatalf("lost enrollment was not reported as unknown: %v", err)
	}
	if !reflect.DeepEqual(x.calls, []string{"page", "session", "info", "enroll"}) {
		t.Fatalf("lost enrollment triggered repeat mutation: %v", x.calls)
	}
}

func TestSetupTOTPCompleteActivationSurvivesLateTransportError(t *testing.T) {
	x := newSetupOutcomeFixture(t)
	x.activation.BodyComplete = true
	x.activationErr = io.EOF
	x.stateStatus = 503
	out, err := x.flow.SetupTOTP(t.Context(), x.path)
	if err != nil || out["activation_succeeded"] != true || out["activation_confirmation_source"] != "business_receipt" || !reflect.DeepEqual(x.calls, []string{"page", "session", "info", "enroll", "activate"}) {
		t.Fatalf("complete response lost to a late transport error: calls=%v err=%v", x.calls, err)
	}
}

func TestSetupTOTPCodeErrorWithMismatchedBindingNeverRetries(t *testing.T) {
	x := newSetupOutcomeFixture(t)
	x.activation = response(400, `{"error":{"code":"invalid_code"},"session_id":"different-session"}`)
	_, err := x.flow.SetupTOTP(t.Context(), x.path)
	if err == nil || x.prompts != 0 || !reflect.DeepEqual(x.calls, []string{"page", "session", "info", "enroll", "activate"}) {
		t.Fatalf("unbound error authorized another code attempt: calls=%v err=%v", x.calls, err)
	}
}

func TestSetupTOTPConfirmedActivationDoesNotDependOnLoginPersistence(t *testing.T) {
	x := newSetupOutcomeFixture(t)
	x.stateStatus = 503
	result, err := x.flow.SetupTOTP(t.Context(), x.path)
	if err != nil || result["activation_succeeded"] != true || result["activation_confirmation_source"] != "business_receipt" {
		t.Fatal("confirmed activation was lost to ordinary checkpoint persistence", err)
	}
	if !reflect.DeepEqual(x.calls, []string{"page", "session", "info", "enroll", "activate"}) || x.prompts != 0 {
		t.Fatal("checkpoint failure retried a mutation or queried confirmation", x.calls)
	}
	if result["session_persistence_warning"] != nil {
		t.Fatal("removed login persistence affected success")
	}
}
