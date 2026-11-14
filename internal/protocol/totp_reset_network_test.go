package protocol

import (
	"context"

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
