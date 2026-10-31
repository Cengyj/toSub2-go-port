package protocol

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"testing"
	"time"

	"tosub2-go-port/internal/transport"
)

func TestInsightDiagnosticsPaymentRequiredPreservesClassification(t *testing.T) {
	response := insightResponse(http.StatusPaymentRequired, `{"error":{"code":"insufficient_quota","type":"billing_error","message":"synthetic-secret owner@example.test https://private.example/?token=synthetic-secret"}}`)
	response.Header.Set("Set-Cookie", "session=synthetic-secret")
	response.Header.Set("X-Request-Id", "synthetic-secret")
	response.Header.Set("Retry-After", "120")
	calls := 0
	client := &InsightsClient{Transport: insightFixtureDoer(func(_ context.Context, request transport.Request) (*transport.Response, error) {
		calls++
		if request.Method != http.MethodGet || !request.SingleAttempt || request.Headers.Get("Authorization") != "Bearer "+insightTestCredentials().AccessToken {
			t.Fatal("diagnostics changed read or identity semantics")
		}
		return response, nil
	})}
	_, failure := client.get(t.Context(), "usage", insightUsageURL, insightTestCredentials(), InsightsOptions{}, time.Second)
	if failure == nil || failure.Code != "upstream_error" || failure.HTTPStatus != 402 || failure.Message != "查询接口返回 HTTP 402，未取得该来源数据" || failure.UpstreamCode != "insufficient_quota" || failure.UpstreamType != "billing_error" || failure.RetryAfter != nil || calls != 1 {
		t.Fatalf("payment-required diagnostics changed rejection semantics: %+v calls=%d", failure, calls)
	}
	assertInsightDiagnosticPrivate(t, failure)
}

func TestInsightDiagnosticsOnlyKnownStructuredFieldsSurvive(t *testing.T) {
	for _, tc := range []struct {
		name, body, code, kind string
	}{
		{"known", `{"error":{"code":"invalid_token","type":"authentication_error","message":"synthetic-secret"}}`, "invalid_token", "authentication_error"},
		{"scope", `{"error":{"code":"insufficient_scope","type":"permission_error"}}`, "insufficient_scope", "permission_error"},
		{"unknown-code", `{"error":{"code":"owner@example.test","type":"authentication_error"}}`, "", "authentication_error"},
		{"unknown-type", `{"error":{"code":"token_expired","type":"synthetic-secret"}}`, "token_expired", ""},
		{"mixed-case", `{"error":{"code":"Invalid_Token","type":"AUTHENTICATION_ERROR"}}`, "", ""},
		{"scalar", `{"error":"synthetic-secret"}`, "", ""},
		{"message-only", `{"error":{"message":"insufficient_scope"}}`, "", ""},
		{"wrong-types", `{"error":{"code":402,"type":["billing_error"]}}`, "", ""},
		{"null", `{"error":{"code":null,"type":null}}`, "", ""},
		{"html", `<html>synthetic-secret owner@example.test</html>`, "", ""},
		{"truncated", `{"error":{"code":"invalid_token"`, "", ""},
		{"duplicate-code", `{"error":{"code":"invalid_token","code":"synthetic-secret","type":"authentication_error"}}`, "", ""},
		{"duplicate-error", `{"error":{"code":"invalid_token"},"error":{"type":"authentication_error"}}`, "", ""},
		{"trailing", `{"error":{"code":"invalid_token"}} {}`, "", ""},
		{"oversized-body", `{"error":{"code":"invalid_token"},"padding":"` + strings.Repeat("x", insightMaxBodyBytes) + `"}`, "", ""},
		{"oversized-code", `{"error":{"code":"` + strings.Repeat("x", 4096) + `","type":"synthetic-secret"}}`, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			failure := insightHTTPFailure("usage", insightResponse(401, tc.body))
			if failure.HTTPStatus != 401 || failure.Code != "authorization_rejected" || failure.UpstreamCode != tc.code || failure.UpstreamType != tc.kind {
				t.Fatalf("unexpected bounded diagnostics: %+v", failure)
			}
			assertInsightDiagnosticPrivate(t, failure)
		})
	}
}

func TestInsightDiagnosticsKeepHTTPRejectionsAuthoritative(t *testing.T) {
	for _, tc := range []struct {
		name, body, code string
		status           int
		err              error
		retryAfter       bool
	}{
		{"authorization", `{"error":{"code":"invalid_token"}}`, "authorization_rejected", 401, nil, false},
		{"permission", `{"error":{"code":"access_denied"}}`, "permission_denied", 403, nil, false},
		{"unsupported", `{"error":{"code":"insufficient_scope"}}`, "unsupported_authorization", 403, nil, false},
		{"truncated-rejection", `{"error":`, "permission_denied", 403, io.ErrUnexpectedEOF, false},
		{"limited", `{"error":{"type":"rate_limit_error"}}`, "rate_limited", 429, nil, true},
		{"unavailable", `{"error":{"type":"server_error"}}`, "upstream_error", 503, nil, true},
		{"redirect", `{}`, "redirect_blocked", 302, nil, false},
		{"oversized", strings.Repeat("x", insightMaxBodyBytes+1), "response_too_large", 403, nil, false},
		{"transport-size-limit", "", "response_too_large", 401, transport.ErrResponseTooLarge, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			client := &InsightsClient{Transport: insightFixtureDoer(func(context.Context, transport.Request) (*transport.Response, error) {
				calls++
				response := insightResponse(tc.status, tc.body)
				response.Header.Set("Retry-After", "120")
				return response, tc.err
			})}
			_, failure := client.get(t.Context(), "usage", insightUsageURL, insightTestCredentials(), InsightsOptions{}, time.Second)
			if failure == nil || failure.Code != tc.code || failure.HTTPStatus != tc.status || (failure.RetryAfter != nil) != tc.retryAfter || calls != 1 {
				t.Fatalf("diagnostics changed rejection, cooldown or retry: %+v calls=%d", failure, calls)
			}
			if tc.name == "truncated-rejection" && failure.TransportReason != "网络连接中断" {
				t.Fatal("truncated rejection lost safe transport reason")
			}
		})
	}
}

func TestInsightDiagnosticsNetworkReasonsAreClosedAndPrivate(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"dns", &net.DNSError{Err: "synthetic-secret", Name: "owner@example.test", Server: "synthetic-secret"}, "域名解析失败"},
		{"certificate", x509.UnknownAuthorityError{}, "证书验证失败"},
		{"hostname", x509.HostnameError{Host: "owner@example.test"}, "证书验证失败"},
		{"tls-record", tls.RecordHeaderError{Msg: "synthetic-secret"}, "TLS 协商或协议不兼容"},
		{"tls-alert", tls.AlertError(40), "TLS 协商或协议不兼容"},
		{"refused", &net.OpError{Op: "synthetic-secret", Net: "synthetic-secret", Err: syscall.ECONNREFUSED}, "连接被拒绝"},
		{"unreachable", syscall.ENETUNREACH, "网络不可达"},
		{"interrupted", io.ErrUnexpectedEOF, "网络连接中断"},
		{"timeout", context.DeadlineExceeded, "请求超时"},
		{"cancelled", context.Canceled, "请求已取消"},
		{"unknown", errors.New("synthetic-secret owner@example.test https://private.example/?token=synthetic-secret cookie=synthetic-secret"), "请求未完成"},
		{"no-response", nil, "未收到 HTTP 响应"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			failure := insightNetworkFailure("usage", tc.err)
			if failure.Code != "network_error" || failure.TransportReason != tc.want || !strings.Contains(failure.Message, tc.want) {
				t.Fatalf("network diagnostic was collapsed: %+v", failure)
			}
			assertInsightDiagnosticPrivate(t, failure)
			if tc.err != nil {
				wrapped := &url.Error{Op: "GET", URL: "https://owner:synthetic-secret@private.example/?token=synthetic-secret", Err: tc.err}
				failure = insightNetworkFailure("usage", wrapped)
				if failure.TransportReason != tc.want {
					t.Fatal("wrapping lost typed diagnostic")
				}
				assertInsightDiagnosticPrivate(t, failure)
			}
		})
	}
}

func TestInsightDiagnosticsProductionTransportUsesSafeClassification(t *testing.T) {
	for _, tc := range []struct {
		name  string
		err   error
		want  string
		calls int
	}{
		{"dns", &net.DNSError{Err: "synthetic-secret", Name: "owner@example.test", IsNotFound: true}, "域名不存在或解析配置错误", 1},
		{"certificate", x509.UnknownAuthorityError{}, "证书验证失败", 1},
		{"tls", tls.RecordHeaderError{Msg: "synthetic-secret"}, "TLS 协商或协议不兼容", 1},
		{"connection", syscall.ECONNREFUSED, "连接被拒绝", 2},
		{"timeout", context.DeadlineExceeded, "请求超时", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			native := transport.NewClient(nil)
			// No socket or upstream connection is ever opened by this transport.
			native.HTTP.Transport = &http.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) { calls++; return nil, tc.err }}
			t.Cleanup(native.Close)
			client := &InsightsClient{Transport: native}
			_, failure := client.get(t.Context(), "usage", insightUsageURL, insightTestCredentials(), InsightsOptions{}, time.Second)
			if failure == nil || failure.Code != "network_error" || failure.TransportReason != tc.want || calls != tc.calls {
				t.Fatalf("production transport classification/retry changed: %+v calls=%d", failure, calls)
			}
			assertInsightDiagnosticPrivate(t, failure)
		})
	}
}

func assertInsightDiagnosticPrivate(t *testing.T, failure any) {
	t.Helper()
	data, err := json.Marshal(failure)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) > 1024 {
		t.Fatal("diagnostic exceeded bounded public metadata size")
	}
	for _, private := range []string{"synthetic-secret", "owner@example.test", "private.example", "Cookie", "cookie", "requestId", "Set-Cookie"} {
		if strings.Contains(string(data), private) {
			t.Fatal("diagnostic exposed private upstream material")
		}
	}
}
