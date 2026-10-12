package console

import "tosub2-go-port/internal/jobs"

// repairEligibility mirrors the legacy console's process-local credential
// checks. Callers resolve credential availability before crossing the public
// boundary; no secret values are needed by the decision or its explanation.
func repairEligibility(job *jobs.Job, password, mail, totp bool) (bool, string) {
	if totpRecoveryBlocked(job) {
		return false, "2FA 重置恢复尚未完成"
	}
	if job.Status == jobs.StatusImported {
		return false, "账号仅已导入，尚未手动开始授权"
	}
	if job.AutoRepairBlocked {
		return false, "账号已确认封禁、删除或永久停用"
	}
	if !job.LastAuthAutomated {
		return false, firstNonEmpty(job.LastAuthAutomationReason, "上次授权不是全自动完成")
	}
	if job.LastAuthRequirements["password"] && !password {
		return false, "已保存的密码无法读取"
	}
	if job.LastAuthRequirements["emailOtp"] && !mail {
		return false, "缺少可自动收取邮箱验证码的 API"
	}
	if job.LastAuthRequirements["mfa"] && !totp {
		return false, "已保存的 2FA 密钥无法读取"
	}
	if !password && !mail {
		return false, "缺少可自动登录的密码或邮件收码 API"
	}
	if job.HasTOTPKey && !totp {
		return false, "2FA 密钥在当前系统上无法恢复"
	}
	return true, "上次完整登录全自动完成，所需资料仍可用"
}
