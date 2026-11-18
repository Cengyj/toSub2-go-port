package protocol

import (
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"tosub2-go-port/internal/transport"
)

func TestResetTOTPActivationProgressKeepsPendingUnavailable(t *testing.T) {
	x := newResetFixture(t, false)
	seen := false
	x.flow.Options.EventData = func(stage, _, message string, _ map[string]any) {
		if stage != "totp_reset_activate" {
			return
		}
		seen = true
		if !x.pending || x.persisted || countResetSteps(x.calls, "activate") != 0 {
			t.Fatal("pending progress did not precede activation and final credential save")
		}
		for _, phrase := range []string{"已安全暂存", "尚未激活", "暂不可领取"} {
			if !strings.Contains(message, phrase) {
				t.Fatalf("activation progress is missing the pending boundary: %s", phrase)
			}
		}
		if strings.Contains(message, "已安全保存") || strings.Contains(message, resetFixtureSecret) {
			t.Fatal("activation progress claims final save or exposes a secret")
		}
	}
	if _, err := x.flow.ResetTOTP(t.Context(), x.path); err != nil || !seen || !x.persisted {
		t.Fatal("reset did not pass through pending progress and complete")
	}
}

func TestResetTOTPBusinessReceiptsCommitWithoutConfirmationGET(t *testing.T) {
	for _, disabled := range []string{`{"message":"Factor disabled","passkey_credential_manager_sync":null}`, `{"message":"Factor successfully disabled"}`, `{"success":true}`} {
		for _, activated := range []string{`{"success":true,"unrelated":null}`, `{"activated":true}`} {
			x := newResetFixture(t, false)
			x.override = func(step string, _ transport.Request) (*transport.Response, error, bool) {
				switch step {
				case "disable":
					x.remoteFactor = ""
					return response(200, disabled), nil, true
				case "activate":
					x.remoteFactor = "new-factor"
					return response(200, activated), nil, true
				}
				return nil, nil, false
			}
			out, err := x.flow.ResetTOTP(t.Context(), x.path)
			if err != nil || !x.persisted || out["credential_persisted"] != true {
				t.Fatalf("complete business receipts did not reach credential commit: %v", err)
			}
			want := []string{"page", "session", "info1", "disable", "enroll", "activate"}
			if !reflect.DeepEqual(x.calls, want) {
				t.Fatalf("unexpected network request order: %v", x.calls)
			}
			if out["disable_confirmation_source"] != "business_receipt" || out["activation_confirmation_source"] != "business_receipt" || out["activation_confirmed_by_read"] == true || out["activation_state_checked_at"] != nil || !acknowledgedResetActivation(out) {
				t.Fatal("activation acknowledgement was lost or misrepresented as a list read")
			}
			if _, err := ReadTOTPResetCompletion(x.path, x.flow.Options.Email); err != nil {
				t.Fatal("downstream completion rejected receipt evidence", err)
			}
			if err := MarkTOTPResetCredentialPersisted(x.path, x.flow.Options.Email, str(out["operation_id"])); err != nil {
				t.Fatal("local completion still requires a query", err)
			}
		}
	}
}

func TestResetTOTPActivationUnknownReadsOnlyExactNewFactor(t *testing.T) {
	for _, tc := range []struct {
		name, body, liveID string
		lost               bool
		wantSuccess        bool
	}{
		{"response lost", "", "new-factor", true, true},
		{"truncated receipt", `{"success":true}`, "new-factor", true, true},
		{"unknown object", `{}`, "new-factor", false, true},
		{"conflicting response", `{"success":true,"error":{"code":"invalid_code"}}`, "foreign-factor", false, false},
		{"wrong returned binding", `{"success":true,"factor_id":"foreign-factor"}`, "foreign-factor", false, false},
		{"duplicate success", `{"success":false,"success":true}`, "foreign-factor", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x := newResetFixture(t, false)
			x.override = func(step string, _ transport.Request) (*transport.Response, error, bool) {
				if step == "activate" {
					x.remoteFactor = tc.liveID
					if tc.lost {
						return response(200, tc.body), io.ErrUnexpectedEOF, true
					}
					return response(200, tc.body), nil, true
				}
				if step == "info2" {
					return response(200, resetInfo(tc.liveID)), nil, true
				}
				return nil, nil, false
			}
			out, err := x.flow.ResetTOTP(t.Context(), x.path)
			if (err == nil) != tc.wantSuccess || x.persisted != tc.wantSuccess {
				t.Fatalf("wrong reconciliation result: success=%t err=%v", x.persisted, err)
			}
			want := []string{"page", "session", "info1", "disable", "enroll", "activate", "info2"}
			if !reflect.DeepEqual(x.calls, want) {
				t.Fatalf("uncertain mutation repeated or extra reads: %v", x.calls)
			}
			if tc.wantSuccess && (out["activation_confirmation_source"] != "state_read" || out["activation_confirmed_by_read"] != true || !verifiedResetActivation(out)) {
				t.Fatal("reconciled success lacks matching read evidence")
			}
		})
	}
}

func TestResetTOTPConfirmedActivationLocalSaveRetryDoesNotQuery(t *testing.T) {
	x := newResetFixture(t, false)
	x.flow.Options.TOTPResetPersist = func(context.Context, string, string) error { return errors.New("synthetic local save failure") }
	out, err := x.flow.ResetTOTP(t.Context(), x.path)
	if err == nil || !verifiedResetActivation(out) || out["credential_persisted"] == true {
		t.Fatal("expected confirmed activation awaiting local save")
	}
	before := len(x.calls)
	x.flow.Options.TOTPResetPersist = func(context.Context, string, string) error { x.persisted = true; return nil }
	x.override = func(string, transport.Request) (*transport.Response, error, bool) {
		t.Fatal("local save retried a network request")
		return nil, nil, true
	}
	out, err = ReadTOTPResetCompletion(x.path, x.flow.Options.Email)
	if err == nil {
		err = x.flow.commitConfirmedTOTPReset(t.Context(), x.path, out)
	}
	if err != nil || !x.persisted || out["credential_persisted"] != true || len(x.calls) != before {
		t.Fatalf("receipt could not finish local commit: %v", err)
	}
}

func TestResetTOTPMissingEnrollmentMaterialNeverReplaysOrActivates(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		lost       bool
	}{
		{"response lost", "", true},
		{"session missing", `{"secret":"` + resetFixtureSecret + `","factor":{"id":"new-factor","factor_type":"totp","is_recovery":false}}`, false},
		{"complete material refused", `{"success":false,"secret":"` + resetFixtureSecret + `","session_id":"new-session","factor":{"id":"new-factor","factor_type":"totp","is_recovery":false}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x := newResetFixture(t, false)
			x.override = func(step string, _ transport.Request) (*transport.Response, error, bool) {
				if step != "enroll" {
					return nil, nil, false
				}
				if tc.lost {
					return nil, io.ErrUnexpectedEOF, true
				}
				return response(200, tc.body), nil, true
			}
			out, err := x.flow.ResetTOTP(t.Context(), x.path)
			if err == nil || x.persisted || out["enrollment_validated"] == true {
				t.Fatal("missing or refused material accepted")
			}
			want := []string{"page", "session", "info1", "disable", "enroll"}
			if !reflect.DeepEqual(x.calls, want) {
				t.Fatalf("invalid enrollment advanced or repeated: %v", x.calls)
			}
		})
	}
}

func TestResetTOTPCompleteReceiptsSurviveLateTransportError(t *testing.T) {
	for _, step := range []string{"disable", "enroll", "activate"} {
		t.Run(step, func(t *testing.T) {
			x := newResetFixture(t, false)
			x.override = func(current string, _ transport.Request) (*transport.Response, error, bool) {
				if current != step {
					return nil, nil, false
				}
				body := map[string]string{
					"disable":  `{"message":"Factor disabled"}`,
					"enroll":   `{"secret":"` + resetFixtureSecret + `","session_id":"new-session","factor":{"id":"new-factor","factor_type":"totp","is_recovery":false}}`,
					"activate": `{"success":true}`,
				}[step]
				if step == "disable" {
					x.remoteFactor = ""
				} else if step == "activate" {
					x.remoteFactor = "new-factor"
				}
				r := response(200, body)
				r.BodyComplete = true
				return r, io.EOF, true
			}
			out, err := x.flow.ResetTOTP(t.Context(), x.path)
			if err != nil || out["credential_persisted"] != true || !reflect.DeepEqual(x.calls, []string{"page", "session", "info1", "disable", "enroll", "activate"}) {
				t.Fatalf("complete response lost to a late transport error: calls=%v err=%v", x.calls, err)
			}
		})
	}
}

func TestResetTOTPCompleteReceiptSurvivesTransientLocalSaveFailure(t *testing.T) {
	for _, step := range []string{"disable", "enroll", "activation"} {
		t.Run(step, func(t *testing.T) {
			x := newResetFixture(t, false)
			failed := false
			journalWriteHook(t, x, func(out map[string]any) error {
				if !failed && payload(out[step+"_receipt"])["body_complete"] == true {
					failed = true
					return errors.New("synthetic transient local publication error")
				}
				return nil
			})
			out, err := x.flow.ResetTOTP(t.Context(), x.path)
			if err != nil || !failed || out["credential_persisted"] != true || !reflect.DeepEqual(x.calls, []string{"page", "session", "info1", "disable", "enroll", "activate"}) {
				t.Fatalf("local publication failure invalidated receipt: calls=%v err=%v", x.calls, err)
			}
			if out["activation_confirmation_source"] != "business_receipt" || out["activation_confirmed_by_read"] == true || !acknowledgedResetActivation(out) {
				t.Fatal("republished receipt lost or reported as a matching read")
			}
		})
	}
}
