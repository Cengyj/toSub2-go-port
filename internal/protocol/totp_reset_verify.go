package protocol

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"
)

const totpStatusChecks = 4

func transientTOTPStatusFailure(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var failure *totpResetError
	if !errors.As(err, &failure) {
		return false
	}
	switch failure.httpStatus {
	case http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

// A first status read does not have an expected factor state yet. Give its
// transient HTTP failures the same bounded budget as later reconciliation,
// using the same in-memory session. Persist confirmed operation evidence at
// the caller; waitForTOTPResetState already owns its separate status budget.
func (f *Flow) readTOTPResetStateWithRetry(ctx context.Context, token, account string) (map[string]any, error) {
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		info, err := f.totpResetRequest(ctx, token, account, http.MethodGet, "/backend-api/accounts/mfa_info", nil)
		if err == nil || !transientTOTPStatusFailure(err) || attempt == totpStatusChecks-1 {
			return info, err
		}
		f.emit("totp_reset_status_pending", "running", fmt.Sprintf("2FA 状态查询暂时失败，保留原会话重试只读查询（%d/%d）；不会重复移除、登记或激活", attempt+1, totpStatusChecks))
		if err := pauseTOTPStatusRetry(ctx, attempt); err != nil {
			return nil, err
		}
	}
}

func pauseTOTPStatusRetry(ctx context.Context, attempt int) error {
	pause := time.NewTimer(500 * time.Millisecond << attempt)
	defer pause.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-pause.C:
		return nil
	}
}

// Confirm eventual read visibility in the existing authenticated session.
// Only GETs are repeated: a factor removal, enrollment, activation or logout
// is never dispatched again because its status read was delayed.
func (f *Flow) waitForTOTPResetState(ctx context.Context, token, account, wanted, expectedID, oldID string) (totpLiveState, error) {
	var last totpLiveState
	for attempt := 0; attempt < totpStatusChecks; attempt++ {
		if err := ctx.Err(); err != nil {
			return last, err
		}
		info, err := f.totpResetRequest(ctx, token, account, http.MethodGet, "/backend-api/accounts/mfa_info", nil)
		if err != nil {
			// Only transient status GET failures share this bounded read budget.
			// Authentication refusals and mutations never
			// gain a retry or a fresh login from this branch.
			if !transientTOTPStatusFailure(err) || attempt == totpStatusChecks-1 {
				return last, err
			}
			f.emit("totp_reset_status_pending", "running", fmt.Sprintf("2FA 状态查询暂时失败，保留原会话重试只读查询（%d/%d）；不会重复移除、登记或激活", attempt+1, totpStatusChecks))
		} else {
			last = classifyTOTPState(info)
			if last.State == wanted && (wanted == totpStateDisabled || str(last.Factors[0]["id"]) == expectedID) {
				return last, nil
			}
			if last.State == totpStateEnabled {
				id := str(last.Factors[0]["id"])
				if id != oldID && id != expectedID {
					return last, resetError("当前 2FA 因子与本次重置不一致，已停止后续修改", true)
				}
			}
			if attempt == totpStatusChecks-1 {
				break
			}
			f.emit("totp_reset_status_pending", "running", fmt.Sprintf("2FA 状态尚未同步，保留当前登录会话继续核对（%d/%d）；不会重复移除或激活因子", attempt+1, totpStatusChecks))
		}
		if err := pauseTOTPStatusRetry(ctx, attempt); err != nil {
			return last, err
		}
	}
	return last, resetError("状态复核次数已用尽；"+last.Summary, true)
}
