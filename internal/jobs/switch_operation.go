package jobs

import (
	"context"
	"errors"
	"time"
)

// SwitchOperation cancels the current checkpointed phone wait and waits for its
// runner to release all resources before scheduling the requested suboperation.
func (m *Manager) SwitchOperation(ctx context.Context, id, operation string, values map[string]any) (*Job, error) {
	events, unsubscribe := m.Subscribe(32)
	defer unsubscribe()
	m.mu.Lock()
	j, ok := m.jobs[id]
	if !ok {
		m.mu.Unlock()
		return nil, osErrNotFound(id)
	}
	cancel, active := m.active[id]
	if active && j.Status != "phone" && j.Status != "phone_otp" && j.Status != "sms_otp" {
		m.mu.Unlock()
		return nil, errors.New("job is busy outside a checkpointed phone step")
	}
	m.mu.Unlock()
	if active {
		cancel()
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			m.mu.Lock()
			_, running := m.active[id]
			m.mu.Unlock()
			if !running {
				break
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-events:
			case <-ticker.C:
			}
		}
	}
	return m.QueueOperation(id, operation, values)
}
func isSuboperation(op string) bool {
	switch op {
	case "setup-2fa", "setup_2fa", "totp_setup", "setup-totp", "add-password", "add_password", "password_add":
		return true
	}
	return false
}

func restoreCanceledSuboperation(job *Job) bool {
	if !isSuboperation(job.Operation) {
		return false
	}
	job.Status = "resume_available"
	job.Prompt = "操作已取消，原登录检查点仍可继续"
	if job.ResultSaved {
		job.Status = StatusSucceeded
		job.Prompt = "操作已取消，原授权文件仍可使用"
	}
	job.Error = ""
	switch job.Operation {
	case "add-password", "add_password", "password_add":
		if !job.HasPassword {
			job.PasswordAddError = "用户取消了本次添加密码"
		}
	default:
		if !job.TOTPKnownEnabled {
			job.TOTPSetupError = "用户取消了本次 2FA 设置"
		}
	}
	return true
}
