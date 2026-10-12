// Job.status also describes whether an old grant/checkpoint is reusable.
// Keep that account capability separate from the outcome of the latest action.
const TERMINAL_STATUSES = new Set(["imported", "completed", "failed", "canceled", "reauth_required", "resume_available", "interrupted"]);
const INPUT_STATUSES = new Set(["password", "email_otp", "mfa_otp", "totp_setup_otp", "phone", "phone_otp", "sms_otp", "waiting_input", "awaiting_input"]);
const PHONE_STATUSES = new Set(["phone", "phone_otp", "sms_otp"]);
const WORKFLOW_TERMINAL = new Set(["succeeded", "failed", "interrupted"]);
const OPERATION_TERMINAL = new Set(["succeeded", "failed", "canceled", "interrupted"]);
const OPERATION_ACTIVE = new Set(["queued", "running", "waiting"]);

const OPERATION_LABELS = {
  account_import: "导入账号", initial_authorization: "首次授权", reauthorize: "重新授权",
  relogin: "重登授权", automatic_relogin: "自动重登授权", resume: "继续授权",
  setup_2fa: "设置 2FA", reset_2fa: "重置 2FA", add_password: "添加密码",
  account_update: "更新账号资料", proxy_update: "更新代理 IP",
};

function text(value) { return typeof value === "string" ? value.trim() : ""; }

function message(value) {
  const raw = text(value);
  const http = raw.match(/\bHTTP(?:\s+status)?\s*[:=]?\s*(\d{3})\b/i)?.[1];
  function readable(detail, code, responseStatus = http) {
    const parts = [];
    if (responseStatus && !new RegExp(`\\bHTTP(?:\\s+status)?\\s*[:=]?\\s*${responseStatus}\\b`, "i").test(detail)) parts.push(`HTTP ${responseStatus}`);
    if (code && !detail.toLowerCase().includes(code.toLowerCase())) parts.push(code);
    parts.push(detail);
    return parts.filter(Boolean).join(" · ");
  }
  const jsonAt = raw.indexOf("{");
  if (jsonAt >= 0) {
    try {
      const payload = JSON.parse(raw.slice(jsonAt));
      const detail = payload?.error?.message || payload?.message;
      const code = text(payload?.error?.code) || text(payload?.code) || (typeof payload?.error === "string" ? text(payload.error) : "");
      const payloadStatus = Number(payload?.status_code || payload?.status || payload?.error?.status);
      const responseStatus = http || (Number.isInteger(payloadStatus) && payloadStatus >= 100 && payloadStatus < 600 ? String(payloadStatus) : "");
      if (typeof detail === "string" && detail.trim()) return readable(detail.trim(), code, responseStatus);
    } catch {}
  }
  const match = raw.match(/"message"\s*:\s*"((?:\\.|[^"\\])*)"/i);
  if (match) {
    try {
      const codeMatch = raw.match(/"code"\s*:\s*"((?:\\.|[^"\\])*)"/i);
      const code = codeMatch ? JSON.parse(`"${codeMatch[1]}"`).trim() : "";
      return readable(JSON.parse(`"${match[1]}"`).trim(), code);
    } catch {}
  }
  return raw;
}

export function jobOperation(job = {}) {
  let type = text(job.lastOperationType) || text(job.operation);
  type = ({
    "setup-2fa": "setup_2fa", "setup-totp": "setup_2fa", totp_setup: "setup_2fa",
    "reset-2fa": "reset_2fa", "add-password": "add_password", password_add: "add_password",
    regenerate: "reauthorize", refresh: "reauthorize", full: Number(job.attempt || job.attempts) > 1 ? "reauthorize" : "initial_authorization",
  })[type] || type;
  if (!type) type = job.status === "imported" ? "account_import" : "initial_authorization";
  return { type, label: OPERATION_LABELS[type] || "账号操作" };
}

export function describeStatus(status) {
  const values = {
    imported: ["待授权", "neutral", "ListPlus"], queued: ["排队中", "neutral", "LoaderCircle"],
    starting: ["启动中", "active", "LoaderCircle", true], running: ["处理中", "active", "LoaderCircle", true],
    working: ["处理中", "active", "LoaderCircle", true], refreshing: ["刷新授权", "active", "RefreshCw", true],
    finalizing: ["保存授权", "active", "LoaderCircle", true],
    password: ["等待密码", "warning", "KeyRound"], email_otp: ["等待邮箱码", "warning", "Mail"],
    mfa_otp: ["等待 2FA", "warning", "ShieldCheck"], totp_setup_otp: ["等待激活 2FA", "warning", "ShieldCheck"],
    phone: ["等待手机号", "warning", "Smartphone"], phone_otp: ["等待手机码", "warning", "Smartphone"],
    sms_otp: ["等待手机码", "warning", "Smartphone"], waiting_input: ["等待输入", "warning", "CircleAlert"],
    awaiting_input: ["等待输入", "warning", "CircleAlert"],
    totp_starting: ["准备 2FA", "active", "LoaderCircle", true],
    password_add: ["添加密码中", "active", "LoaderCircle", true],
    password_add_starting: ["准备密码", "active", "LoaderCircle", true],
    completed: ["已完成", "success", "CheckCircle2"], failed: ["执行失败", "danger", "CircleAlert"],
    canceled: ["已取消", "neutral", "Ban"], reauth_required: ["待重登授权", "warning", "LogIn"],
    resume_available: ["可继续授权", "warning", "RotateCcw"], interrupted: ["已中断", "warning", "CircleAlert"],
  };
  const [label, tone, icon, spinning = false] = values[status] || ["状态待确认", "warning", "CircleAlert"];
  return { code: status || "unknown", label, tone, icon, spinning };
}

function workflowApplies(job, operation) {
  if (OPERATION_ACTIVE.has(job.operationStatus) && WORKFLOW_TERMINAL.has(job.totpWorkflowStatus)) return false;
  return ["setup_2fa", "reset_2fa"].includes(operation.type) && Boolean(job.totpWorkflowStatus && job.totpWorkflowStatus !== "not_requested");
}

function queueSummary(job, operation) {
  const position = Number(job.queuePosition);
  return Number.isFinite(position) && position > 0 ? `排队中，前方还有 ${Math.max(0, Math.floor(position) - 1)} 条任务` : `已加入队列，等待${operation.label}`;
}

export function describeWorkflow(job = {}) {
  const operation = jobOperation(job);
  if (!workflowApplies(job, operation)) return null;
  const status = job.totpWorkflowStatus;
  const current = job.totpWorkflowStep || "totp";
  const finished = WORKFLOW_TERMINAL.has(status);
  const recovery = Boolean(job.totpRecoveryPending || job.totpCredentialInvalidated);
  let totp = "pending";
  if (recovery || job.totpResetStatus === "unknown") totp = "unknown";
  else if (job.totpResetPhase === "credential_persisted" || status === "succeeded" || current !== "totp") totp = "succeeded";
  else if (status === "failed") totp = "failed";
  else if (status === "interrupted") totp = "unknown";
  else if (status === "running") totp = "running";
  const steps = [
    { key: "totp", label: operation.type === "reset_2fa" ? "重置 2FA" : "设置 2FA", state: totp },
    ...(job.logoutAllDevicesAfterTotp ? [{ key: "logout", label: "退出设备", state: job.logoutAllDevicesStatus || "pending" }] : []),
    ...(job.reloginAfterTotp ? [{ key: "relogin", label: "重登授权", state: job.totpReloginStatus || "pending" }] : []),
    ...(job.uploadAfterTotp ? [{ key: "upload", label: "上传 Sub2", state: job.totpUploadStatus || "pending" }] : []),
  ];
  const currentIndex = steps.findIndex((step) => step.key === current);
  for (const [index, step] of steps.entries()) {
    let state = step.state;
    if (state === "not_requested") state = "pending";
    if (step.key === current && ["pending", "running"].includes(state)) {
      if (status === "interrupted") state = "unknown";
      else if (status === "failed") state = "failed";
      else if (status === "running") state = "running";
    } else if (state === "pending" && finished) state = "skipped";
    // A stopped run must never leave future stages looking active.
    if (finished && index > currentIndex && state === "running") state = "skipped";
    step.state = state;
    step.stateLabel = ({ pending: "等待", running: "进行中", succeeded: "已完成", failed: "失败", skipped: "未执行", not_attempted: "未执行", unknown: "待核实" })[state] || "待核实";
  }
  const currentStep = steps.find((step) => step.key === current) || steps[0];
  const uncertain = steps.some((step) => step.state === "unknown");
  return {
    status, current, finished, steps, uncertain,
    label: ({ pending: "排队中", running: "执行中", succeeded: "已完成", failed: uncertain ? "结果待核实" : "已停止", interrupted: "已中断" })[status] || "待核实",
    tone: status === "succeeded" ? "success" : status === "failed" && !uncertain ? "danger" : finished ? "warning" : "active",
    error: message(job.totpWorkflowError || (current === "totp" ? job.totpResetError || job.totpSetupError : current === "logout" ? job.logoutAllDevicesError : "")),
    currentStep,
  };
}

export function describeSMS(job = {}) {
  const provider = text(job.smsProviderName) || "接码平台";
  const labels = {
    requesting: "正在获取手机号", number_acquired: "已取号，正在发送验证码",
    waiting_sms: "等待短信验证码", submitting: "已收码，正在自动提交",
    submitted: "已提交验证码，等待验证结果", manual_submitted: "自动收码已停止，正在验证手动输入",
    verified: "手机验证已通过", completed: "手机验证已通过，订单已完成",
    error: "接码失败", canceled: "接码订单已取消", expired: "接码订单已过期",
  };
  if (!job.smsStatus || ["idle", "unavailable"].includes(job.smsStatus)) return null;
  const detail = message(job.smsError);
  return {
    key: "sms", title: provider, text: detail || labels[job.smsStatus] || "接码状态待确认",
    tone: detail || ["error", "expired"].includes(job.smsStatus) ? "warning" : ["verified", "completed"].includes(job.smsStatus) ? "success" : "info",
    icon: detail ? "CircleAlert" : "PhoneIncoming", state: job.smsStatus,
  };
}

function selectedOperationError(job, operation, workflow) {
  if (job.operationStatus === "succeeded") return "";
  const distinct = Object.prototype.hasOwnProperty.call(job, "operationError") || Object.prototype.hasOwnProperty.call(job, "error");
  if (distinct) return message(job.operationError || job.error);
  if (workflow?.error) return workflow.error;
  if (["setup_2fa", "reset_2fa"].includes(operation.type)) return message(job.totpResetError || job.totpSetupError || job.logoutAllDevicesError);
  if (operation.type === "add_password") return message(job.passwordAddError);
  // The legacy field merges operation errors with credential warnings. A
  // successful action with only this field is a reminder, not proven failure.
  return ["failed", "canceled", "interrupted"].includes(job.status) ? message(job.lastError) : "";
}

function workflowSummary(job, workflow, operation) {
  if (!workflow) return "";
  if (workflow.status === "pending") return queueSummary(job, operation);
  if (workflow.status === "running") {
    if (INPUT_STATUSES.has(job.status)) return text(job.prompt) || "请完成当前验证，流程将自动继续";
    return ({ totp: `正在${operation.label}`, logout: "正在退出所有设备", relogin: "正在重新登录并生成新授权", upload: "正在同步新授权到 Sub2API" })[workflow.current] || "正在执行 2FA 流程";
  }
  if (workflow.status === "succeeded") {
    if (job.status === "reauth_required") return "2FA 已完成，请重新登录生成授权文件";
    if (!job.canDownload && !job.resultSaved) return "2FA 已完成，尚未生成授权文件";
    return job.uploadAfterTotp ? "2FA、新授权与 Sub2API 同步已完成" : job.reloginAfterTotp ? "2FA 与重新授权已完成，授权文件已保存" : `${operation.label}已完成，授权文件可下载`;
  }
  const canceled = job.operationStatus === "canceled";
  const interrupted = job.operationStatus === "interrupted" || workflow.status === "interrupted";
  const totpSaved = workflow.steps[0]?.state === "succeeded";
  if (totpSaved) {
    const saved = operation.type === "reset_2fa" ? "新 2FA 已保存" : "2FA 已保存";
    const outcome = canceled ? "已取消" : interrupted ? "已中断" : workflow.uncertain ? "结果待核实" : ["skipped", "not_attempted"].includes(workflow.currentStep.state) ? "未执行" : "失败";
    const step = ({ logout: "退出设备", relogin: "重登授权", upload: "Sub2API 同步" })[workflow.current];
    const authorizationSaved = workflow.current === "upload" && job.totpReloginStatus === "succeeded" && Boolean(job.canDownload || job.resultSaved);
    const result = step ? `${authorizationSaved ? "新授权已保存" : saved} · ${step}${outcome}` : `${saved} · 后续操作${canceled ? "已取消" : interrupted ? "已中断" : "未完成"}`;
    return `${result}${interrupted ? "；后续步骤不会自动执行" : ""}`;
  }
  if (workflow.status === "interrupted") return "流程已中断，请核实已执行步骤；后续步骤不会自动执行";
  return `${operation.label}未完成，后续步骤未执行`;
}

export function describeJob(job = {}) {
  const operation = jobOperation(job);
  const workflow = describeWorkflow(job);
  const operationStatus = text(job.operationStatus);
  const terminal = OPERATION_TERMINAL.has(operationStatus) ? true : OPERATION_ACTIVE.has(operationStatus) ? false : TERMINAL_STATUSES.has(job.status);
  const recovery = Boolean(job.totpRecoveryPending || job.totpCredentialInvalidated);
  const totpAction = ["setup_2fa", "reset_2fa"].includes(operation.type);
  const passwordAction = operation.type === "add_password";
  const operationError = selectedOperationError(job, operation, workflow);
  const credentialWarning = message(job.credentialWarning);
  const hasDistinctError = Object.prototype.hasOwnProperty.call(job, "operationError") || Object.prototype.hasOwnProperty.call(job, "credentialWarning");
  const legacyReminder = !hasDistinctError && !operationError && !credentialWarning ? message(job.lastError) : "";
  const phoneActive = !terminal && (PHONE_STATUSES.has(job.status) || /(?:phone|sms)/.test(text(job.stage)));
  let status = describeStatus(job.status);
  let summary = text(job.prompt);
  if (recovery) {
    status = { code: "recovery_required", label: "2FA 待恢复", tone: "warning", icon: "ShieldCheck", spinning: false };
    summary = "2FA 变更未完整完成，请核实并恢复有效密钥";
  } else if (["failed", "canceled", "interrupted"].includes(operationStatus)) {
    const canceled = operationStatus === "canceled";
    const interrupted = operationStatus === "interrupted";
    const uncertain = Boolean(workflow?.uncertain);
    status = { code: `operation_${operationStatus}`, label: canceled ? "本次已取消" : interrupted ? "本次已中断" : uncertain ? "结果待核实" : "本次操作失败", tone: canceled ? "neutral" : interrupted || uncertain ? "warning" : "danger", icon: canceled ? "Ban" : "CircleAlert", spinning: false };
    summary = workflow && workflow.status !== "succeeded" ? workflowSummary(job, workflow, operation) : `${operation.label}${canceled ? "已取消" : interrupted ? "已中断" : "未完成"}${job.canDownload ? "，已有授权文件仍可下载" : job.canResume ? "，原授权流程可继续" : ""}`;
  } else if (workflow?.finished && workflow.status !== "succeeded") {
    status = { code: workflow.status === "interrupted" ? "workflow_interrupted" : "workflow_failed", label: workflow.uncertain ? "结果待核实" : workflow.status === "interrupted" ? "流程已中断" : "流程未完成", tone: workflow.tone, icon: "CircleAlert", spinning: false };
    summary = workflowSummary(job, workflow, operation);
  } else if (operationError && terminal && job.status !== "canceled") {
    const canceled = /(?:用户取消|操作已取消|本次.*取消)/.test(operationError);
    status = { code: canceled ? "operation_canceled" : "operation_failed", label: canceled ? "本次已取消" : "本次操作失败", tone: canceled ? "neutral" : "danger", icon: canceled ? "Ban" : "CircleAlert", spinning: false };
    summary = `${operation.label}${canceled ? "已取消" : "未完成"}${job.canDownload ? "，已有授权文件仍可下载" : job.canResume ? "，原授权流程可继续" : ""}`;
  } else if (workflow) {
    summary = workflowSummary(job, workflow, operation);
    if (workflow.status === "running" && !INPUT_STATUSES.has(job.status)) {
      status = { code: "workflow_running", label: workflow.currentStep.label + "中", tone: "active", icon: "LoaderCircle", spinning: true };
    }
  } else if (job.status === "completed") {
    summary = totpAction || passwordAction ? `${operation.label}已完成${job.canDownload ? "，授权文件可下载" : ""}` : job.canDownload || job.resultSaved ? "授权文件已保存，可下载或同步 Sub2API" : "本次操作已完成，尚无可下载的授权文件";
    if (legacyReminder) status = { code: "completed_warning", label: "完成 · 有提醒", tone: "warning", icon: "CircleAlert", spinning: false };
  } else if (job.status === "failed") {
    summary = `${operation.label}未完成${job.canDownload ? "，已有授权文件仍可下载" : "，请查看错误后重试"}`;
  } else if (job.status === "canceled") {
    summary = `本次任务已取消${job.canDownload ? "，已有授权文件仍可下载" : ""}`;
  } else if (job.status === "imported") {
    summary = "账号资料已导入，选择功能开始执行";
  } else if (!summary) {
    summary = ({ queued: "已加入队列，等待执行", reauth_required: "需要重新登录生成授权文件", resume_available: "已有授权检查点，可继续完成流程", interrupted: "服务重启中断了任务，请手动继续或重试" })[job.status] || `正在${operation.label}`;
  }

  if (!recovery && operationStatus === "succeeded" && (!workflow || workflow.status === "succeeded")) {
    status = { code: "operation_succeeded", label: workflow ? "流程已完成" : "本次已完成", tone: "success", icon: "CheckCircle2", spinning: false };
    if (!workflow) summary = `${operation.label}已完成${job.canDownload ? "，授权文件可下载" : job.status === "reauth_required" ? "，请重新登录获取授权" : job.canResume ? "，原授权流程可继续" : "，尚未生成授权文件"}`;
  } else if (!recovery && operationStatus === "queued") {
    status = describeStatus("queued");
    summary = queueSummary(job, operation);
  } else if (!recovery && operationStatus === "running") {
    if (TERMINAL_STATUSES.has(job.status) || workflow?.finished) {
      status = describeStatus("running");
      summary = `正在${operation.label}`;
    }
  } else if (!recovery && operationStatus === "waiting") {
    if (!INPUT_STATUSES.has(job.status)) {
      status = describeStatus("waiting_input");
      summary = "请完成当前验证，流程将自动继续";
    }
  }

  // A later step failure must not describe the safely saved 2FA as failed.
  // Cancellation and interruption keep their own outcome instead of becoming
  // ordinary partial success or suggesting that the workflow will continue.
  if (!recovery && workflow?.finished && workflow.status !== "succeeded" && workflow.steps[0]?.state === "succeeded"
    && !["canceled", "interrupted"].includes(operationStatus) && workflow.status !== "interrupted") {
    status = { code: "workflow_partial", label: workflow.uncertain ? "结果待核实" : "部分完成", tone: "warning", icon: "CircleAlert", spinning: false };
  }

  const reloginUpload = operation.type === "relogin" && job.uploadAfterRelogin ? text(job.reloginUploadStatus) : "";
  if (!recovery && reloginUpload === "running" && !terminal) {
    status = { code: "relogin_upload_running", label: "同步 Sub2API", tone: "active", icon: "LoaderCircle", spinning: true };
    summary = "新授权已保存，正在更新 Sub2API 账号";
  } else if (!recovery && operationStatus === "succeeded" && reloginUpload) {
    if (reloginUpload === "succeeded") {
      status = { code: "relogin_upload_succeeded", label: "授权并同步成功", tone: "success", icon: "CheckCircle2", spinning: false };
      summary = "新授权已保存并同步至 Sub2API";
    } else if (["failed", "unknown", "skipped"].includes(reloginUpload)) {
      status = { code: `relogin_upload_${reloginUpload}`, label: reloginUpload === "unknown" ? "授权成功 · 同步待核实" : reloginUpload === "skipped" ? "授权成功 · 同步未执行" : "授权成功 · 同步失败", tone: "warning", icon: "CircleAlert", spinning: false };
      summary = `重登授权已完成，Sub2API ${reloginUpload === "unknown" ? "同步结果待核实" : reloginUpload === "skipped" ? "同步未执行" : "同步未完成"}；新授权文件可下载`;
    }
  }

  const notices = [];
  const historyNotices = [];
  const seen = new Set();
  function add(key, title, detail, tone = "warning", scope = "current", icon = "CircleAlert", details = "") {
    const value = message(detail);
    if (!value) return;
    const normalized = value.replace(/\s+/g, " ").toLowerCase();
    if (seen.has(normalized)) return;
    seen.add(normalized);
    const notice = { key, title, text: value, tone, scope, icon, details };
    (scope === "history" ? historyNotices : notices).push(notice);
  }
  const stoppedWorkflow = workflow?.finished && workflow.status !== "succeeded" && operationStatus !== "succeeded";
  const workflowErrors = stoppedWorkflow ? [
    workflow.current === "logout" ? job.logoutAllDevicesError : workflow.current === "totp" ? job.totpResetError || job.totpSetupError : "",
    operationError, workflow.error,
  ].map(message).filter(Boolean).filter((value, index, values) => values.findIndex((other) => other.replace(/\s+/g, " ").toLowerCase() === value.replace(/\s+/g, " ").toLowerCase()) === index) : [];
  const workflowDetails = workflowErrors.join("\n\n");
  if (recovery) add("totp-recovery", "需要恢复", "旧密钥已停用或状态未确认，新密钥安全保存前，自动登录和再次重置已暂停。", "warning", "account", "ShieldCheck");
  if (workflowDetails) {
    const http = workflowDetails.match(/\bHTTP(?:\s+status)?\s*[:=]?\s*(\d{3})\b/i)?.[1];
    add("workflow", workflow.currentStep.label, http ? `HTTP ${http} · 查看原因` : "查看原因", status.tone === "neutral" ? "info" : status.tone === "warning" ? "warning" : "danger", "current", "CircleAlert", workflowDetails);
  } else if (operationError && operationError !== "canceled") add("operation", "本次操作", operationError, status.tone === "neutral" ? "info" : status.tone === "warning" ? "warning" : "danger");
  add("credential", "凭据保存", credentialWarning, "warning", "account", "KeyRound");
  if (legacyReminder) add("legacy-reminder", terminal ? "操作提醒" : "当前提醒", legacyReminder);
  if (["failed", "unknown", "skipped"].includes(reloginUpload)) add("relogin-upload", reloginUpload === "unknown" ? "同步待核实" : "Sub2API 同步", message(job.reloginUploadError) || (reloginUpload === "skipped" ? "自动上传未执行，请手动上传新授权。" : "请查看日志并手动确认 Sub2API 中的账号授权。"), "warning", "current", "Send");
  if (totpAction && operationStatus !== "succeeded" && !workflowDetails) {
    add("totp-setup", "2FA", job.totpSetupError);
    add("totp-reset", "重置 2FA", job.totpResetError);
    add("logout", "退出设备", job.logoutAllDevicesError);
  }
  if (passwordAction && operationStatus !== "succeeded") add("password", "添加密码", job.passwordAddError);
  if (!terminal && job.status === "email_otp") add("mail", "自动收码", job.mailApiError, "warning", "current", "Mail");
  if (phoneActive) {
    add("phone", "手机验证", job.phoneError);
    if (job.phoneRetryRequired) add("phone-retry", "需要确认", "上次验证码请求结果未确认，请手动重新发送或更换手机号。", "warning", "current", "Smartphone");
  }
  const sms = describeSMS(job);
  if (sms && phoneActive) add(sms.key, sms.title, sms.text, sms.tone, "current", sms.icon);
  if (job.securityCheckRequired && (!terminal || job.status === "failed" || operationStatus === "failed")) add("security", "人工核验", "账号需要人工核验，自动重登已暂停。", "warning", "account", "ShieldCheck");
  if (job.autoRepairBlocked) add("monitor-blocked", "号池监控", message(job.autoRepairBlockedReason) || "账号已被监控永久跳过，请人工核实。", "warning", "monitor", "ShieldCheck");
  if (job.autoRepairLastError) add("monitor-error", "自动修复", job.autoRepairLastError, "warning", "monitor", "RefreshCw");

  // Old diagnostics remain inspectable without repainting the current action
  // as failed or leaving an old SMS spinner visible after authentication.
  if (!totpAction || operationStatus === "succeeded") {
    add("old-totp-setup", "此前 2FA 设置", job.totpSetupError, "warning", "history");
    add("old-totp-reset", "此前 2FA 重置", job.totpResetError, "warning", "history");
    add("old-logout", "此前退出设备", job.logoutAllDevicesError, "warning", "history");
    add("old-workflow", "此前 2FA 流程", job.totpWorkflowError, "warning", "history");
  }
  if (!passwordAction || operationStatus === "succeeded") add("old-password", "此前添加密码", job.passwordAddError, "warning", "history");
  if (!phoneActive) {
    add("old-phone", "此前手机验证", job.phoneError, "warning", "history");
    if (sms) add("old-sms", `此前${sms.title}`, sms.text, sms.tone, "history", sms.icon);
  }
  if (terminal || job.status !== "email_otp") add("old-mail", "此前自动收码", job.mailApiError, "warning", "history", "Mail");

  return {
    status, operation, summary, notices, historyNotices, workflow, terminal,
    phone: { visible: phoneActive && Boolean(job.currentPhone), number: job.currentPhone || "", retryRequired: phoneActive && Boolean(job.phoneRetryRequired) },
    sms: phoneActive ? sms : null,
    authorization: { available: Boolean(job.canDownload || job.resultSaved), resumable: Boolean(job.canResume), reauthRequired: job.status === "reauth_required" },
  };
}

export function uploadBlockedJob(job = {}) {
  return !describeJob(job).terminal || Boolean(job.totpRecoveryPending || job.totpCredentialInvalidated)
    || ["pending", "running"].includes(job.totpWorkflowStatus);
}

export function describeMonitor(state = {}) {
  const running = Boolean(state.running);
  const enabled = Boolean(state.enabled);
  const configured = Boolean(state.configured);
  const error = message(state.lastError);
  return {
    status: running ? { code: "running", label: "正在巡检", tone: "active", icon: "LoaderCircle", spinning: true }
      : !configured ? { code: "unconfigured", label: "未配置", tone: "neutral", icon: "Settings2", spinning: false }
      : !enabled ? { code: "disabled", label: "未启用", tone: "neutral", icon: "Pause", spinning: false }
      : error ? { code: "error", label: "上次巡检异常", tone: "warning", icon: "CircleAlert", spinning: false }
      : { code: "enabled", label: "已启用", tone: "success", icon: "ShieldCheck", spinning: false },
    error, errorTitle: running ? "上次巡检提醒" : "巡检提醒",
    result: state.lastResult || null,
    resultTitle: running ? "上次巡检结果" : "最近巡检结果",
  };
}

export function describeUpload(result = {}, { running = false, error = "" } = {}) {
  if (running) return { label: "正在同步 Sub2API", tone: "active", icon: "LoaderCircle", spinning: true, text: "正在检查已有账号并同步授权" };
  if (error) return { label: "同步未完成", tone: "warning", icon: "CircleAlert", spinning: false, text: message(error) };
  const counts = result.result || result;
  const created = Number(counts.created || 0);
  const updated = Number(counts.updated || 0);
  const failed = Number(counts.failed || 0);
  const skipped = Number(result.skipped || 0) + (counts !== result ? Number(counts.skipped || 0) : 0);
  const parts = [`新增 ${created} 条`, `更新 ${updated} 条`];
  if (skipped) parts.push(`跳过 ${skipped} 条`);
  if (failed) parts.push(`失败 ${failed} 条`);
  return { label: failed ? "部分同步未完成" : skipped ? "同步完成 · 有跳过" : "同步已完成", tone: failed || skipped ? "warning" : "success", icon: failed || skipped ? "CircleAlert" : "CheckCircle2", spinning: false, text: parts.join("，") };
}
