package protocol

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"tosub2-go-port/internal/resetlog"
	"tosub2-go-port/internal/transport"
)

func captureResetDiagnostic(t *testing.T, f *Flow) *[]resetlog.Detail {
	t.Helper()
	var records []resetlog.Detail
	f.Options.EventData = func(stage, status, message string, data map[string]any) {
		if detail, ok := data[resetlog.DataKey].(*resetlog.Detail); ok {
			safe := detail.Safe()
			if safe == nil || stage != "totp_reset_diagnostic" || status != "" || message != safe.Message() {
				t.Fatal("diagnostic changed operation status or differed from its safe public message")
			}
			records = append(records, *safe)
		}
	}
	return &records
}

func TestResetTOTPRequestLogNamesActualActionsWithoutChangingWorkflow(t *testing.T) {
	x := newResetFixture(t, true)
	records := captureResetDiagnostic(t, x.flow)
	out, err := x.flow.ResetTOTP(t.Context(), x.path)
	if err != nil || out["activation_verified"] != true || out["credential_persisted"] != true || out["logout_all_devices_status"] != "succeeded" {
		t.Fatal("request presentation changed the reset workflow", err)
	}
	if !reflect.DeepEqual(x.calls, []string{"page", "session", "info1", "disable", "enroll", "activate", "logout"}) {
		t.Fatal("diagnostics added, reordered or repeated an upstream request", x.calls)
	}
	wantActions := map[string]string{"login": "读取登录页面", "identity": "核对账号身份", "initial_state": "读取 2FA 设置", "disable": "移除旧 2FA", "enroll": "登记新 2FA", "activation": "激活新 2FA", "logout": "退出所有设备"}
	seen := map[string]bool{}
	intents := map[string]resetlog.Detail{}
	for _, detail := range *records {
		if detail.Source != "transport" {
			continue
		}
		if detail.Reason == "DISPATCH_INTENT" {
			if detail.Terminal || detail.Level() != "info" || detail.Dispatch != "possibly_sent" || detail.Receipt != "not_received" || detail.HTTP != 0 || detail.DurationMS != nil || !strings.HasSuffix(detail.Message(), "已记录请求意图，等待响应") || strings.Contains(detail.Message(), "未收到 HTTP") {
				t.Fatal("live request intent was reported as a completed unknown result")
			}
			intents[detail.Stage] = detail
			continue
		}
		if detail.Reason != "REQUEST_COMPLETE" || detail.Dispatch != "response_received" || detail.Receipt != "complete" || detail.DurationMS == nil || *detail.DurationMS < 0 || detail.Ended == "" || detail.Level() != "info" {
			t.Fatal("successful HTTP transport lost its separate receipt evidence", detail.Stage)
		}
		action, ok := wantActions[detail.Stage]
		if detail.Stage == "login" {
			// The login adapter's provider/CSRF/sign-in requests are deliberately
			// represented by /:login, without copying their concrete URLs.
			action, ok = map[string]string{
				"GET /": "读取登录页面", "GET /:login": "读取登录验证", "POST /:login": "提交登录验证",
			}[detail.Method+" "+detail.Path]
		}
		if !ok || seen[detail.Stage] && detail.Stage != "login" || !strings.HasPrefix(detail.Message(), action+"：收到完整响应（HTTP ") || !strings.HasSuffix(detail.Message(), "ms）；待解析业务结果") || strings.Contains(detail.Message(), "成功") {
			t.Fatal("request completion did not identify its action or implied business success", detail.Message())
		}
		seen[detail.Stage] = true
		if detail.Access == "write" && detail.Stage != "login" {
			intent, ok := intents[detail.Stage]
			if !ok || intent.AttemptRef != detail.AttemptRef || intent.Seq >= detail.Seq || intent.Path != detail.Path || intent.Method != detail.Method {
				t.Fatal("write completion lost its original request-intent binding")
			}
		}
	}
	if len(seen) != len(wantActions) || len(intents) != 4 {
		t.Fatal("request ledger omitted an actual action or invented a read intent")
	}
	x.flow.EmitResetOutcome(out, nil, true)
	last := (*records)[len(*records)-1]
	if !last.Terminal || last.Commit != "committed" || last.Activation != "accepted" || last.Match != "skipped" || last.Writes == nil {
		t.Fatal("diagnostic changes lost the final independent evidence")
	}
	for _, count := range []resetlog.Count{last.Writes.Disable, last.Writes.Enroll, last.Writes.Activation, last.Writes.Logout} {
		if count != (resetlog.Count{Received: 1}) {
			t.Fatal("presentation changed write accounting", count)
		}
	}
	encoded, err := json.Marshal(records)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{resetFixtureSecret, resetFixtureToken, "new-session", "old-factor", "new-factor", "current-account", "enroll@example.com"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatal("public request diagnostics exposed a private request or response field")
		}
	}
}

func TestResetRequestObserverKeepsFailureEvidenceDistinctFromPendingIntent(t *testing.T) {
	unsent := unsentResetNetworkError(t)
	for _, tc := range []struct {
		name, reason, dispatch, receipt string
		response                        *transport.Response
		err                             error
		count                           resetlog.Count
	}{
		{"refused", "HTTP_REFUSED", "response_received", "complete", response(400, `{}`), nil, resetlog.Count{Received: 1}},
		{"server-error", "SERVER_ERROR", "response_received", "complete", response(503, `{}`), nil, resetlog.Count{Received: 1}},
		{"truncated-200", "INCOMPLETE_RESPONSE", "response_received", "incomplete", response(200, `{"success":true`), io.ErrUnexpectedEOF, resetlog.Count{Received: 1}},
		{"timeout", "REQUEST_TIMEOUT", "possibly_sent", "not_received", nil, context.DeadlineExceeded, resetlog.Count{Possible: 1}},
		{"canceled", "REQUEST_CANCELED", "possibly_sent", "not_received", nil, context.Canceled, resetlog.Count{Possible: 1}},
		{"lost", "NO_RESPONSE", "possibly_sent", "not_received", nil, errors.New("synthetic-private-error-canary"), resetlog.Count{Possible: 1}},
		{"unsent", "REQUEST_UNSENT", "proven_unsent", "not_received", nil, unsent, resetlog.Count{Unsent: 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := NewFlow(nil, FlowOptions{})
			f.resetLog = &resetDiagnostics{ref: resetlog.OperationRef("synthetic-observer")}
			f.resetCapture = &resetCapture{out: map[string]any{}}
			records := captureResetDiagnostic(t, f)
			finish := f.observeResetAttempt(http.MethodPost, f.Options.ChatGPTBase+"/backend-api/accounts/mfa/user/activate_enrollment?token=synthetic-private-url-canary")
			if finish == nil || len(*records) != 1 || (*records)[0].Reason != "DISPATCH_INTENT" || (*records)[0].Level() != "info" || (*records)[0].Terminal {
				t.Fatal("write dispatch did not produce a distinct live intent")
			}
			finish(tc.response, tc.err)
			if len(*records) != 2 {
				t.Fatal("one attempt produced extra completion records")
			}
			intent, complete := (*records)[0], (*records)[1]
			if complete.Reason != tc.reason || complete.Dispatch != tc.dispatch || complete.Receipt != tc.receipt || complete.Level() != "warning" || complete.AttemptRef != intent.AttemptRef || complete.DurationMS == nil || complete.Ended == "" || complete.Terminal || f.resetLog.writes.Activation != tc.count {
				t.Fatal("failure presentation changed receipt classification, attempt binding, or accounting", complete)
			}
			if !strings.HasPrefix(complete.Message(), "激活新 2FA：") || strings.Contains(complete.Message(), "待解析业务结果") || strings.Contains(complete.Message(), "激活成功") {
				t.Fatal("failure was mislabeled as a normal response or business success")
			}
			data, err := json.Marshal(records)
			if err != nil || strings.Contains(string(data), "canary") || strings.Contains(string(data), "?token") {
				t.Fatal("raw URL or error leaked into request diagnostics", err)
			}
		})
	}
}
