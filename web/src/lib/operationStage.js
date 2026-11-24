// Public protocol milestones shared by the live account row and history.
// A stage reports one step; it is never evidence that the entire operation or
// its local credential commit succeeded.
export const OPERATION_STAGES = {
  profile_required: { label: "资料待补全", phase: "资料补全", tone: "active", message: "登录返回资料未完善步骤（about_you）；将按既有规则补全资料" },
  profile_submitting: { label: "提交资料中", phase: "资料补全", tone: "active", message: "正在提交资料补全；响应未确认时本轮不会重复提交" },
  profile_completed: { label: "资料已补全", phase: "资料补全", tone: "active", message: "资料补全已确认，正在继续本次登录" },
  profile_rejected: { label: "资料提交被拒绝", phase: "资料补全", tone: "warning", message: "资料补全被拒绝，本轮已停止提交" },
  profile_unknown: { label: "资料结果待核实", phase: "资料补全", tone: "warning", message: "资料补全结果未确认，本轮已停止，不重复提交" },
  profile_not_sent: { label: "资料未提交", phase: "资料补全", tone: "warning", message: "资料补全请求未发送" },
  totp_setup_seed_unverified: { label: "2FA 密钥待核实", phase: "核实 2FA 密钥", tone: "warning", message: "远端已确认激活，但人工验证码未证明候选密钥正确；尚未将候选密钥确认为可用凭据" },
  password_already_exists: { label: "已有密码 · 已跳过", phase: "添加密码", tone: "neutral", message: "账号已有密码，已跳过添加" },
};
