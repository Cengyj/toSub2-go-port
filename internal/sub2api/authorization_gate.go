package sub2api

import "context"

// AuthorizationGateError is an explicit preflight refusal, never an uncertain
// remote mutation. Its public message is selected from a closed vocabulary.
type AuthorizationGateError struct {
	Reason string
}

func (e *AuthorizationGateError) Error() string {
	switch e.Reason {
	case "degraded":
		return "模型检测未达到配置阈值，已按策略停止上传；已保存授权仍保留"
	case "failed":
		return "模型检测失败或结果无法确认，已按策略停止上传；已保存授权仍保留"
	case "busy":
		return "模型检测或账号操作尚未结束，已停止后续上传"
	case "settings":
		return "模型检测策略暂不可读取，已停止后续上传，请先核实服务配置"
	case "identity":
		return "上传授权与当前账号身份不一致，已停止后续上传"
	case "authorization_expired":
		return "当前授权已过期，历史模型检测结果不能用于放行，已停止后续上传"
	case "authorization_changed":
		return "授权文件已变化、尚未提交或正在恢复，已停止后续上传；请刷新后核实"
	case "persistence":
		return "模型检测结果未能安全保存，已停止后续上传"
	default:
		return "上传前模型检测尚未通过，已停止后续同步授权"
	}
}

func (c *Client) checkAuthorizationSync(ctx context.Context, account map[string]any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if c.cfg.BeforeAuthorizationSync != nil {
		if err := c.cfg.BeforeAuthorizationSync(ctx, account); err != nil {
			return err
		}
	}
	return ctx.Err()
}
