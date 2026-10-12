package protocol

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"syscall"
	"testing"

	"tosub2-go-port/internal/transport"
)

func unsentResetNetworkError(t *testing.T) error {
	t.Helper()
	client := transport.NewClient(nil)
	defer client.Close()
	client.HTTP.Transport.(*http.Transport).DialContext = func(context.Context, string, string) (net.Conn, error) {
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ENETUNREACH}
	}
	_, err := client.Do(t.Context(), transport.Request{Method: http.MethodPost, URL: "http://127.0.0.1/", SingleAttempt: true})
	if !transport.IsRequestDefinitelyUnsent(err) {
		t.Fatal("fixture did not prove request was unsent")
	}
	return err
}

func TestResetTOTPMutationsRetryOnlyBeforeDispatch(t *testing.T) {
	unsent := unsentResetNetworkError(t)
	for _, target := range []string{"disable", "enroll", "activate", "logout"} {
		t.Run(target, func(t *testing.T) {
			x := newResetFixture(t, true)
			x.flow.Client.RetryDelay = 0
			failed := 0
			x.override = func(step string, _ transport.Request) (*transport.Response, error, bool) {
				if step == target && failed < 2 {
					failed++
					return nil, unsent, true
				}
				return nil, nil, false
			}
			out, err := x.flow.ResetTOTP(t.Context(), x.path)
			if err != nil || failed != 2 || out["credential_persisted"] != true || out["logout_all_devices_status"] != "succeeded" {
				t.Fatalf("unsent network retry failed: count=%d err=%v", failed, err)
			}
		})
	}
}

func TestResetTOTPUnsentRemovalRestoresSafeRetryButUnknownRemovalDoesNot(t *testing.T) {
	unsent := unsentResetNetworkError(t)
	for _, scenario := range []string{"unsent", "unknown", "abort-failed"} {
		t.Run(scenario, func(t *testing.T) {
			x := newResetFixture(t, false)
			x.flow.Client.RetryDelay = 0
			aborts, requests := 0, 0
			x.flow.Options.TOTPResetAbortBeforeSend = func(ctx context.Context) error {
				aborts++
				if ctx.Err() != nil || ValidateTOTPResetStart(x.path, x.flow.Options.Email) != nil {
					t.Fatal("abort did not have a safe persisted initial journal")
				}
				if scenario == "abort-failed" {
					return errors.New("fixture persistence failure")
				}
				x.intent = false
				return nil
			}
			x.override = func(step string, _ transport.Request) (*transport.Response, error, bool) {
				if step == "disable" {
					requests++
					if scenario == "unknown" {
						return nil, io.EOF, true
					}
					return nil, unsent, true
				}
				return nil, nil, false
			}
			out, err := x.flow.ResetTOTP(t.Context(), x.path)
			if err == nil || x.pending || x.persisted {
				t.Fatal("failed removal continued to enrollment")
			}
			if scenario == "unsent" {
				if requests != 4 || aborts != 1 || out["reset_totp_phase"] != "initial" || out["old_factor_invalidated"] != false || ValidateTOTPResetStart(x.path, x.flow.Options.Email) != nil || !strings.Contains(err.Error(), "可以再次执行") {
					t.Fatalf("unsent failure stayed locked: requests=%d aborts=%d err=%v", requests, aborts, err)
				}
				// The next explicit click may start a new operation; the failed
				// operation itself never automatically restarts the reset flow.
				x.override = nil
				x.info = 0
				if _, err := x.flow.ResetTOTP(t.Context(), x.path); err != nil {
					t.Fatal("explicit second reset failed", err)
				}
			} else {
				wantRequests, wantAborts := 4, 1
				if scenario == "unknown" {
					wantRequests, wantAborts = 1, 0
				}
				if requests != wantRequests || aborts != wantAborts || out["reset_totp_phase"] != "recovery_required" || ValidateTOTPResetStart(x.path, x.flow.Options.Email) == nil {
					t.Fatal("ambiguous removal or failed persistence lost quarantine")
				}
			}
		})
	}
}

func TestTOTPResetHTTPRefusalRemainsRefusalWhenBodyIsInterrupted(t *testing.T) {
	for _, target := range []string{"disable", "logout"} {
		t.Run(target, func(t *testing.T) {
			x := newResetFixture(t, true)
			calls := 0
			x.override = func(step string, _ transport.Request) (*transport.Response, error, bool) {
				if step == target {
					calls++
					return response(400, ""), io.ErrUnexpectedEOF, true
				}
				return nil, nil, false
			}
			out, err := x.flow.ResetTOTP(t.Context(), x.path)
			if calls != 1 {
				t.Fatal("HTTP refusal was replayed", calls)
			}
			if target == "disable" {
				if err == nil || !strings.Contains(err.Error(), "HTTP 400") {
					t.Fatal("HTTP refusal became a network failure", err)
				}
			} else if err != nil || out["logout_all_devices_status"] != "failed" || !strings.Contains(str(out["logout_all_devices_error"]), "HTTP 400") {
				t.Fatal("logout refusal became unknown", err)
			}
		})
	}
}
