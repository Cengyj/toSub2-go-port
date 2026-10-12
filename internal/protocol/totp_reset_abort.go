package protocol

import (
	"context"
	"time"

	"tosub2-go-port/internal/transport"
)

// This is allowed only before dispatch, or after transport evidence proves
// every attempt stopped before obtaining an HTTP-capable connection. It never
// rolls back enrollment, activation, or a removal with an ambiguous response.
func (f *Flow) abortTOTPResetBeforeSend(ctx context.Context, path string, out *map[string]any, operationErr *error) {
	if f.Options.TOTPResetAbortBeforeSend == nil {
		return
	}
	initial := make(map[string]any, len(*out))
	for key, value := range *out {
		initial[key] = value
	}
	initial["reset_totp_phase"] = "initial"
	initial["reset_totp_status"] = "failed"
	initial["reset_totp_attempted_at"] = nil
	initial["old_factor_invalidated"] = false
	for _, key := range []string{"old_totp_factor_id", "reset_totp_last_phase"} {
		delete(initial, key)
	}
	// Write the safe journal first. A crash before restoring the vault leaves
	// quarantine intact; no crash window can expose a possibly invalid seed.
	if writeTOTPJournal(path, initial) != nil {
		return
	}
	// Finish this bounded, local-only persistence even if the user canceled
	// the network operation. Do not resume any remote account request here.
	localCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if f.Options.TOTPResetAbortBeforeSend(localCtx) != nil {
		_ = writeTOTPJournal(path, *out)
		return
	}
	*out = initial
	*operationErr = resetError(transport.ReadFailureMessage(*operationErr)+"；移除请求尚未发送，原 2FA 仍有效，可以再次执行", false)
	f.emit("totp_reset_not_started", "running", "尚未发送 2FA 修改请求，已恢复原密钥的使用资格，可以再次重置")
}
