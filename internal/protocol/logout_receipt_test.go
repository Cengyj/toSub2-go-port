package protocol

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"tosub2-go-port/internal/transport"
)

// These are the original endpoint's accepted response forms. They are not
// inferred from a common success schema shared with other security actions.
func TestLogoutPreservesOriginalEndpointCompatibility(t *testing.T) {
	const private = "logout-private-response-canary"
	for _, test := range []struct {
		name, body, outcome string
		status              int
	}{
		{"no-content", "", "succeeded", 204},
		{"explicit-success", `{"success":true}`, "succeeded", 200},
		{"success-with-large-unknown-number", `{"success":true,"vendor":{"count":1e400}}`, "succeeded", 200},
		{"success-with-negative-zero-error", `{"success":true,"error":-0e400}`, "succeeded", 200},
		{"success-with-underflow-error", `{"success":true,"error":1e-400}`, "succeeded", 200},
		{"nonzero-numeric-error", `{"success":true,"error":0.25}`, "unknown", 200},
		{"empty-success-status", " \n", "succeeded", 200},
		{"empty-object", `{}`, "succeeded", 200},
		{"null", `null`, "succeeded", 200},
		{"true", `true`, "succeeded", 200},
		{"false", `false`, "succeeded", 200},
		{"number", `1e400`, "succeeded", 200},
		{"string", `"` + private + `"`, "succeeded", 200},
		{"encoded-object-string", `"{\"success\":false}"`, "succeeded", 200},
		{"plain-text", private, "succeeded", 200},
		{"array", `["` + private + `"]`, "succeeded", 200},
		{"unknown-object-fields", `{"message":"` + private + `","count":1e400}`, "succeeded", 200},
		{"string-success", `{"success":"false"}`, "succeeded", 200},
		{"null-success", `{"success":null}`, "succeeded", 200},
		{"accepted", `null`, "succeeded", 202},
		{"partial-content", `{"success":true}`, "succeeded", 206},
		{"duplicate-success", `{"success":false,"success":true}`, "unknown", 200},
		{"duplicate-error", `{"success":true,"error":"rejected","error":false}`, "unknown", 200},
	} {
		t.Run(test.name, func(t *testing.T) {
			x := newResetFixture(t, true)
			x.override = func(step string, _ transport.Request) (*transport.Response, error, bool) {
				if step == "logout" {
					return response(test.status, test.body), nil, true
				}
				return nil, nil, false
			}
			out, err := x.flow.ResetTOTP(t.Context(), x.path)
			if err != nil || out["logout_all_devices_status"] != test.outcome || out["credential_persisted"] != true || !x.persisted {
				t.Fatalf("logout classification changed saved factor: outcome=%v err=%v", out["logout_all_devices_status"], err)
			}
			if countResetSteps(x.calls, "logout") != 1 || countResetSteps(x.calls, "disable") != 1 || countResetSteps(x.calls, "activate") != 1 {
				t.Fatal("logout outcome replayed a mutation")
			}
			public, _ := json.Marshal(out)
			journal, _ := json.Marshal(readResetResult(t, x.path))
			if strings.Contains(string(public), private) || strings.Contains(string(journal), private) {
				t.Fatal("logout retained response content")
			}
			before := len(x.calls)
			if _, err := x.flow.ResetTOTP(t.Context(), x.path); err == nil || len(x.calls) != before {
				t.Fatal("terminal operation was replayed")
			}
		})
	}
}

func TestLogoutUnknownResponseExplainsSafeReasonWithoutReplaying(t *testing.T) {
	const private = "logout-private-response-canary"
	for _, test := range []struct {
		name, body, contentType, reason string
		status                          int
		readError                       error
	}{
		{"large-numeric-error", `{"success":true,"error":1e400}`, "application/json", "成功与错误字段矛盾", 200, nil},
		{"invalid-json", `{"private":"` + private, "application/json", "无效 JSON", 200, nil},
		{"ambiguous-json", `{"success":false,"success":true,"private":"` + private + `"}`, "application/json", "歧义 JSON", 200, nil},
		{"false-success", `{"success":false}`, "application/json", "success=false", 200, nil},
		{"error", `{"error":"` + private + `"}`, "application/json", "包含错误字段", 200, nil},
		{"contradiction", `{"success":true,"error":"` + private + `"}`, "application/json", "成功与错误字段矛盾", 200, nil},
		{"html", `<html>` + private + `</html>`, "text/html; private=" + private, "HTML", 200, nil},
		{"html-without-content-type", ` <HTML>` + private, "", "HTML", 200, nil},
		{"doctype-with-wrong-content-type", `<!DOCTYPE html>` + private, "application/json", "HTML", 200, nil},
		{"script-with-wrong-content-type", `<SCRIPT>` + private, "text/plain", "HTML", 200, nil},
		{"invalid-array", `["` + private, "application/json", "无效 JSON", 200, nil},
		{"incomplete-body", `{"success":true}`, "application/json", "响应处理未完成", 200, io.ErrUnexpectedEOF},
		{"server-error", `{"error":"` + private + `"}`, "application/json", "服务端异常", 503, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			x := newResetFixture(t, true)
			x.override = func(step string, _ transport.Request) (*transport.Response, error, bool) {
				if step != "logout" {
					return nil, nil, false
				}
				r := response(test.status, test.body)
				r.Header.Set("Content-Type", test.contentType)
				return r, test.readError, true
			}
			out, err := x.flow.ResetTOTP(t.Context(), x.path)
			if err != nil || out["logout_all_devices_status"] != "unknown" || out["credential_persisted"] != true || !x.persisted {
				t.Fatalf("unconfirmed logout changed the saved factor: status=%v err=%v", out["logout_all_devices_status"], err)
			}
			message := str(out["logout_all_devices_error"])
			if !strings.Contains(message, fmt.Sprintf("HTTP %d", test.status)) || !strings.Contains(message, test.reason) || strings.Contains(message, private) {
				t.Fatalf("missing or unsafe logout diagnosis: %s", message)
			}
			if test.status >= 500 && strings.Contains(message, "被拒绝") {
				t.Fatal("uncertain server failure was described as a confirmed rejection")
			}
			if countResetSteps(x.calls, "logout") != 1 || countResetSteps(x.calls, "enroll") != 1 || countResetSteps(x.calls, "activate") != 1 {
				t.Fatal("unknown receipt replayed a mutation")
			}
			before := len(x.calls)
			if _, err := x.flow.ResetTOTP(t.Context(), x.path); err == nil || len(x.calls) != before {
				t.Fatal("terminal operation was replayed")
			}
		})
	}
}

func TestLogoutChallengeDoesNotReplayOrExposeResponse(t *testing.T) {
	x := newResetFixture(t, true)
	x.override = func(step string, _ transport.Request) (*transport.Response, error, bool) {
		if step != "logout" {
			return nil, nil, false
		}
		r := response(200, `{"success":true,"private":"logout-private-challenge"}`)
		r.Header.Set("Cf-Mitigated", "challenge")
		return r, errors.Join(ErrBrowserChallenge, errors.New("logout-private-challenge")), true
	}
	out, err := x.flow.ResetTOTP(t.Context(), x.path)
	message := str(out["logout_all_devices_error"])
	if err != nil || out["logout_all_devices_status"] != "unknown" || !x.persisted || !strings.Contains(message, "HTTP 200") || !strings.Contains(message, "挑战响应") || strings.Contains(message, "logout-private-challenge") || countResetSteps(x.calls, "logout") != 1 {
		t.Fatalf("challenge evidence lost or mutation replayed: %s err=%v", message, err)
	}
}

func TestLogoutCompatibleResponseStillRequiresDurableResult(t *testing.T) {
	x := newResetFixture(t, true)
	blocked := errors.New("synthetic logout result save failure")
	journalWriteHook(t, x, func(out map[string]any) error {
		if out["logout_all_devices_status"] == "succeeded" {
			return blocked
		}
		return nil
	})
	successEvents, saveFailureEvents := 0, 0
	x.flow.Options.EventData = nil
	x.flow.Options.Event = func(stage, _, _ string) {
		if stage == "totp_logout_succeeded" {
			successEvents++
		}
		if stage == "totp_logout_save_failed" {
			saveFailureEvents++
		}
	}
	x.override = func(step string, _ transport.Request) (*transport.Response, error, bool) {
		if step == "logout" {
			return response(200, `null`), nil, true
		}
		return nil, nil, false
	}
	out, err := x.flow.ResetTOTP(t.Context(), x.path)
	if !errors.Is(err, blocked) || successEvents != 0 || saveFailureEvents != 1 || !x.persisted || out["credential_persisted"] != true || !verifiedResetActivation(out) {
		t.Fatalf("logout save failure was hidden or erased committed factor: success=%d saveFailure=%d err=%v", successEvents, saveFailureEvents, err)
	}
	saved := readResetResult(t, x.path)
	if saved["logout_all_devices_status"] != "unknown" || saved["credential_persisted"] != true || str(saved["logout_all_devices_attempted_at"]) == "" || countResetSteps(x.calls, "logout") != 1 {
		t.Fatal("unsaved logout result lost its dispatch evidence or credential commit")
	}
	before := len(x.calls)
	if _, err := x.flow.ResetTOTP(t.Context(), x.path); err == nil || len(x.calls) != before {
		t.Fatal("failed local result write replayed the security operation")
	}
}
