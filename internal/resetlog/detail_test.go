package resetlog

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRequestMessagesIdentifyActionsWithoutPromotingHTTPToSuccess(t *testing.T) {
	for _, tc := range []struct {
		stage, method, path, action string
	}{
		{"login", "GET", "/", "读取登录页面"},
		{"login", "POST", "/", "提交登录请求"},
		{"login", "HEAD", "/:login", "读取登录验证"},
		{"login", "POST", "/:login", "提交登录验证"},
		{"identity", "GET", "/api/auth/session", "核对账号身份"},
		{"initial_state", "GET", "/backend-api/accounts/mfa_info", "读取 2FA 设置"},
		{"conflict_read", "GET", "/backend-api/accounts/mfa_info", "复查已有验证器"},
		{"verify_disable", "GET", "/backend-api/accounts/mfa_info", "核实移除结果"},
		{"verify_factor", "GET", "/backend-api/accounts/mfa_info", "核实新验证器"},
		{"disable", "POST", "/backend-api/accounts/mfa/user/disable_in_house", "移除旧 2FA"},
		{"initial_enroll", "POST", "/backend-api/accounts/mfa/enroll", "首次登记新 2FA"},
		{"enroll", "POST", "/backend-api/accounts/mfa/enroll", "登记新 2FA"},
		{"retry_enroll", "POST", "/backend-api/accounts/mfa/enroll", "再次登记新 2FA"},
		{"activation", "POST", "/backend-api/accounts/mfa/user/activate_enrollment", "激活新 2FA"},
		{"logout", "POST", "/backend-api/accounts/logout_all", "退出所有设备"},
		// A known route takes priority over inconsistent historical stage text.
		{"initial_state", "POST", "/backend-api/accounts/mfa/user/activate_enrollment", "激活新 2FA"},
		{"verify_disable", "GET", "", "核实移除结果"},
	} {
		t.Run(tc.stage+"/"+tc.method+"/"+tc.path, func(t *testing.T) {
			ms := int64(653)
			d := (&Detail{OpRef: OperationRef("synthetic-operation"), Seq: 2, AttemptRef: "A1", Stage: tc.stage,
				Reason: "REQUEST_COMPLETE", Source: "transport", Method: tc.method, Path: tc.path,
				Dispatch: "response_received", Receipt: "complete", HTTP: 200, DurationMS: &ms}).Safe()
			if d == nil {
				t.Fatal("valid public transport record rejected")
			}
			before, err := json.Marshal(d)
			if err != nil {
				t.Fatal(err)
			}
			want := tc.action + "：收到完整响应（HTTP 200 · 653 ms）；待解析业务结果"
			if got := d.Message(); got != want || strings.Contains(got, "成功") || strings.Contains(got, "/backend-api/") {
				t.Fatalf("request action or business-result boundary lost: %s", got)
			}
			after, err := json.Marshal(d)
			if err != nil || string(before) != string(after) || d.Level() != "info" || d.Next != "classify_receipt" {
				t.Fatal("presentation changed structured evidence or completion authority", err)
			}
		})
	}
}

func TestDispatchIntentIsPendingWhileInterruptedWriteRemainsUncertain(t *testing.T) {
	d := (&Detail{OpRef: OperationRef("synthetic-operation"), Seq: 1, AttemptRef: "A1", Stage: "disable", Reason: "DISPATCH_INTENT",
		Source: "transport", Method: "POST", Path: "/backend-api/accounts/mfa/user/disable_in_house", Access: "write",
		Dispatch: "possibly_sent", Receipt: "not_received"}).Safe()
	if d == nil || d.Message() != "移除旧 2FA：已记录请求意图，等待响应" || d.Level() != "info" || d.Next != "await_receipt" || d.Terminal {
		t.Fatal("live dispatch intent was presented as a final failure or sent request")
	}
	if d.Dispatch != "possibly_sent" || d.Receipt != "not_received" || d.HTTP != 0 {
		t.Fatal("pending wording changed protected dispatch evidence")
	}
	interrupted := (&Detail{OpRef: d.OpRef, Seq: 2, Stage: "disable", Reason: "INTERRUPTED_WRITE", Terminal: true, Source: "protected_journal"}).Safe()
	if interrupted == nil || interrupted.Level() != "warning" || !strings.HasPrefix(interrupted.Message(), "本轮结果：") || !strings.Contains(interrupted.Message(), "可能已发") || !strings.Contains(interrupted.Message(), "不再发送") {
		t.Fatal("interrupted write intent lost its uncertainty or no-replay boundary")
	}
}

func TestRequestFailuresKeepTheirDistinctEvidenceAndWarnings(t *testing.T) {
	for _, tc := range []struct {
		reason, dispatch, receipt, text string
		http                            int
	}{
		{"SERVER_ERROR", "response_received", "complete", "写入结果不能由状态码确定", 503},
		{"HTTP_REFUSED", "response_received", "complete", "请求被拒绝", 400},
		{"INCOMPLETE_RESPONSE", "response_received", "incomplete", "回执不完整，未作为成功依据", 200},
		{"CONTRADICTORY_RECEIPT", "response_received", "complete", "不能作为成功凭据", 200},
		{"REQUEST_UNSENT", "proven_unsent", "not_received", "传输确认请求未发出", 0},
		{"REQUEST_TIMEOUT", "possibly_sent", "not_received", "请求超时，不能推断写入结果", 0},
		{"REQUEST_CANCELED", "possibly_sent", "not_received", "请求已取消，保留此前证据", 0},
		{"NO_RESPONSE", "possibly_sent", "not_received", "可能已经发出", 0},
	} {
		t.Run(tc.reason, func(t *testing.T) {
			ms := int64(0)
			d := (&Detail{OpRef: OperationRef("synthetic-operation"), Seq: 3, AttemptRef: "A2", Stage: "activation", Reason: tc.reason,
				Source: "transport", Method: "POST", Path: "/backend-api/accounts/mfa/user/activate_enrollment", Access: "write",
				Dispatch: tc.dispatch, Receipt: tc.receipt, HTTP: tc.http, DurationMS: &ms}).Safe()
			if d == nil {
				t.Fatal("valid failure record rejected")
			}
			message := d.Message()
			if !strings.HasPrefix(message, "激活新 2FA：") || !strings.Contains(message, tc.text) || !strings.Contains(message, "0 ms") || strings.Contains(message, "待解析业务结果") || d.Level() != "warning" {
				t.Fatal("failure lost action, evidence or severity", message)
			}
			if tc.http == 0 && !strings.Contains(message, "未收到 HTTP 响应") || d.Dispatch != tc.dispatch || d.Receipt != tc.receipt || d.HTTP != tc.http {
				t.Fatal("presentation changed or concealed transport evidence")
			}
		})
	}
}

func TestRequestMessageUsesOnlyWhitelistedContextAndKeepsTerminalSummary(t *testing.T) {
	ms := int64(-1)
	d := (&Detail{OpRef: OperationRef("synthetic-operation"), Seq: 2, AttemptRef: "A1", Stage: "verify_factor", Reason: "REQUEST_COMPLETE",
		Source: "transport", Method: "PRIVATE-METHOD", Path: "/backend-api/accounts/mfa_info?token=private-token-canary",
		Dispatch: "response_received", Receipt: "complete", HTTP: 200, DurationMS: &ms}).Safe()
	if d == nil || d.Path != "" || d.Method != "" || d.Message() != "核实新验证器：收到完整响应（HTTP 200）；待解析业务结果" || strings.Contains(d.Message(), "canary") {
		t.Fatal("request title exposed an untrusted path or invented elapsed time")
	}
	terminal := (&Detail{OpRef: d.OpRef, Seq: 3, Stage: "local_commit", Reason: "RESET_SAVED_BY_RECEIPT", Source: "workflow", Terminal: true,
		LastTrusted: "local_commit", Pending: "verified", Activation: "accepted", Match: "skipped", Commit: "committed", Logout: "not_requested"}).Safe()
	if terminal == nil || terminal.Message() != "本轮结果：网站已确认激活，新密钥已保存；未另查验证器列表；可在“账号信息 → 2FA 密钥”查看" {
		t.Fatal("request formatting changed the existing terminal summary")
	}
}
