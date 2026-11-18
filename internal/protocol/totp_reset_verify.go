package protocol

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"
	"tosub2-go-port/internal/transport"
)

const totpStatusChecks = 4
const totpReadDeadline = 30 * time.Second

func transientTOTPStatusFailure(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var failure *totpResetError
	if !errors.As(err, &failure) {
		return false
	}
	switch failure.httpStatus {
	case 0:
		return transport.IsRetryableReadError(failure.cause)
	case http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

// The first status read may lag a fresh login. Transient HTTP failures and
// structurally valid but missing/disabled metadata share one bounded GET budget
// in the same session. Conflicting identities and malformed responses stop.
func (f *Flow) readTOTPResetStateWithRetry(ctx context.Context, token, account string) (map[string]any, error) {
	ctx, cancel := context.WithTimeout(ctx, totpReadDeadline)
	defer cancel()
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		info, err := f.totpResetRequest(ctx, token, account, http.MethodGet, "/backend-api/accounts/mfa_info", nil)
		if err == nil {
			state := classifyTOTPState(info)
			if state.State != totpStateUnknown || !totpInitialEnrollmentAllowed(info) || attempt == totpStatusChecks-1 {
				return info, nil
			}
			f.emit("totp_reset_status_pending", "running", fmt.Sprintf("登录已成功，暂未读到明确可移除的 2FA；等待状态同步后重新读取（%d/%d）。%s", attempt+1, totpStatusChecks, state.Summary))
			f.readAgain("initial_state", totpStatusChecks-attempt-1)
			if err := pauseTOTPStatusRetry(ctx, attempt); err != nil {
				return nil, err
			}
			continue
		}
		if !transientTOTPStatusFailure(err) || attempt == totpStatusChecks-1 {
			return info, err
		}
		f.emit("totp_reset_status_pending", "running", fmt.Sprintf("2FA 状态查询暂时失败，保留原会话重试只读查询（%d/%d）；不会重复移除、登记或激活", attempt+1, totpStatusChecks))
		f.readAgain("initial_state", totpStatusChecks-attempt-1)
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
	ctx, cancel := context.WithTimeout(ctx, totpReadDeadline)
	defer cancel()
	var last totpLiveState
	stage := "verify_factor"
	if wanted == totpStateDisabled {
		stage = "verify_disable"
	}
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
			if wanted == totpStateEnabled && f.resetCapture != nil {
				last = verificationTOTPState(info)
				f.reportStateMatch(last, expectedID, oldID)
			}
			if last.State == wanted && (wanted == totpStateDisabled || last.matches(expectedID)) {
				return last, nil
			}
			if last.State == totpStateEnabled {
				if len(last.Factors) > 1 || !last.matches(oldID) && !last.matches(expectedID) {
					return last, resetError("当前 2FA 因子与本次重置不一致，已停止后续修改", true)
				}
			}
			if attempt == totpStatusChecks-1 {
				break
			}
			f.emit("totp_reset_status_pending", "running", fmt.Sprintf("2FA 状态尚未同步，保留当前登录会话继续核对（%d/%d）；不会重复移除或激活因子", attempt+1, totpStatusChecks))
		}
		f.readAgain(stage, totpStatusChecks-attempt-1)
		if err := pauseTOTPStatusRetry(ctx, attempt); err != nil {
			return last, err
		}
	}
	f.decision(stage, "READ_BUDGET_EXHAUSTED", "state_read")
	return last, resetError("状态复核次数已用尽；"+last.Summary, true)
}
