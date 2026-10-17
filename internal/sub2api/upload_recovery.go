package sub2api

import "context"

// uploadRecoveryError preserves the successful credential update while naming
// the later lifecycle step that still needs reconciliation. A canceled step
// that was never invoked is a known failure, not an unknown remote mutation.
type uploadRecoveryError struct {
	step    string
	started bool
	cause   error
}

func (e *uploadRecoveryError) Error() string {
	return "Sub2API 授权已更新，但" + e.step + "未完成；请先在 Sub2API 核对账号状态：" + SafeError(e.cause)
}

func (e *uploadRecoveryError) Unwrap() error { return e.cause }

// recoverUploadedAccount runs only after a confirmed credential write to an
// account whose detail was in error. It must not be used to enable disabled or
// healthy-but-paused accounts. Each mutation retains the transport's bounded
// before-send retry policy; a sent request is never replayed here.
func (c *Client) recoverUploadedAccount(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return &uploadRecoveryError{step: "清除错误状态", cause: err}
	}
	if err := c.ClearError(ctx, id); err != nil {
		return &uploadRecoveryError{step: "清除错误状态", started: true, cause: err}
	}
	if err := ctx.Err(); err != nil {
		return &uploadRecoveryError{step: "恢复调度", cause: err}
	}
	if err := c.SetSchedulable(ctx, id, true); err != nil {
		return &uploadRecoveryError{step: "恢复调度", started: true, cause: err}
	}
	return nil
}
