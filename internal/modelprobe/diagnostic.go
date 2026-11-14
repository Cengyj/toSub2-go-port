package modelprobe

import (
	"context"
	"errors"
)

// FailureDiagnostic is a public-safe explanation. Messages and codes come from
// this finite vocabulary, never from provider text, URLs or upstream bodies.
type FailureDiagnostic struct {
	Code       string
	Message    string
	HTTPStatus int
}

func DiagnoseFailure(err error) FailureDiagnostic {
	if errors.Is(err, context.Canceled) {
		return FailureDiagnostic{Code: "canceled", Message: "模型检测已取消"}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return FailureDiagnostic{Code: "timeout", Message: "模型检测超时，未取得完整结果"}
	}
	var failure *Error
	if errors.As(err, &failure) && failure != nil {
		if message := failureMessage(failure.Code); message != "" {
			status := failure.HTTPStatus
			if status < 100 || status > 599 {
				status = 0
			}
			return FailureDiagnostic{Code: failure.Code, Message: message, HTTPStatus: status}
		}
	}
	return FailureDiagnostic{Code: "probe_failed", Message: "模型检测未取得有效结果"}
}

func failureMessage(code string) string {
	switch code {
	case "authorization_unavailable":
		return "没有可用的已保存授权，未发起检测"
	case "authorization_expired":
		return "已保存授权已过期，请先更新授权"
	case "proxy_selection_required":
		return "账号代理身份尚未明确，未改用其他出口"
	case "invalid_credentials":
		return "已保存授权身份无效，未发起检测"
	case "invalid_options":
		return "模型检测配置无效"
	case "unsupported":
		return "当前授权方式不支持此模型检测"
	case "transport_unavailable", "unsafe_transport":
		return "安全检测传输不可用，未发起检测"
	case "redirect_blocked":
		return "检测请求被重定向，已阻止凭据转发"
	case "unauthorized":
		return "上游拒绝了当前授权，请先核实授权状态"
	case "forbidden", "permission_denied":
		return "上游拒绝了本次请求，请核对账号权限和所选模型"
	case "rate_limited":
		return "模型检测受到上游限流，请稍后重试"
	case "timeout":
		return "模型检测超时，未取得完整结果"
	case "network_error", "transport_error":
		return "模型检测网络请求失败，未自动重试"
	case "http_error":
		return "模型检测上游返回错误状态"
	case "incomplete":
		return "模型回复未完整结束，未取得有效结果"
	case "upstream_error":
		return "上游报告模型检测失败"
	case "model_mismatch":
		return "上游回复的模型与所选模型不一致"
	case "refused":
		return "模型拒绝完成检测题目"
	case "unexpected_content_type":
		return "上游未返回 SSE 流式响应，未取得有效结果"
	case "invalid_stream":
		return "模型响应流结构无效或前后不一致，未取得有效结果"
	case "invalid_answer_format":
		return "模型已完整回复，但答案不是要求的单个规范非负整数"
	case "invalid_response", "invalid_json", "empty_response":
		return "模型回复格式无效，未取得有效结果"
	case "response_too_large":
		return "模型回复超过检测大小限制"
	case "model_unavailable":
		return "所选检测模型当前不可用"
	case "canceled", "cancelled":
		return "模型检测已取消"
	}
	return ""
}

// withHTTPStatus retains an observed status when stream or answer validation
// fails after a response arrived. It never changes the error classification.
func withHTTPStatus(err error, status int) error {
	var failure *Error
	if errors.As(err, &failure) && failure != nil && failure.HTTPStatus == 0 && status >= 100 && status <= 599 {
		copy := *failure
		copy.HTTPStatus = status
		return &copy
	}
	return err
}
