// Display-only projection of the public reset diagnostic. Paths and stages are
// fixed names, never raw upstream URLs or business-success evidence.
const stageActions = {
  identity: "核对账号身份", initial_state: "读取 2FA 设置", conflict_read: "复查已有验证器",
  verify_disable: "核实移除结果", verify_factor: "核实新验证器", disable: "移除旧 2FA",
  initial_enroll: "首次登记新 2FA", enroll: "登记新 2FA", retry_enroll: "再次登记新 2FA",
  activation: "激活新 2FA", logout: "退出所有设备",
};
const readStages = {
  "/": ["login"], "/:login": ["login"], "/api/auth/session": ["identity"],
  "/backend-api/accounts/mfa_info": ["initial_state", "conflict_read", "verify_disable", "verify_factor"],
};
const ordinaryFields = new Set(["op_ref", "seq", "attempt_ref", "stage", "reason_code", "evidence_source", "next_action", "terminal", "method", "path", "dispatch", "http_status", "receipt", "started_at", "ended_at", "duration_ms", "access"]);
const ongoingStatuses = new Set([undefined, "", "running", "working", "starting", "queued"]);
const own = (object, key) => Object.prototype.hasOwnProperty.call(object, key);
const validTime = value => typeof value === "string" && /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d{1,9})?(?:Z|[+-]\d{2}:\d{2})$/.test(value) && Number.isFinite(Date.parse(value));

function requestAction(detail) {
  switch (detail.path) {
    case "/api/auth/session": return "核对账号身份";
    case "/backend-api/accounts/mfa_info": return ["conflict_read", "verify_disable", "verify_factor"].includes(detail.stage) ? stageActions[detail.stage] : "读取 2FA 设置";
    case "/backend-api/accounts/mfa/user/disable_in_house": return "移除旧 2FA";
    case "/backend-api/accounts/mfa/enroll": return ["initial_enroll", "retry_enroll"].includes(detail.stage) ? stageActions[detail.stage] : "登记新 2FA";
    case "/backend-api/accounts/mfa/user/activate_enrollment": return "激活新 2FA";
    case "/backend-api/accounts/logout_all": return "退出所有设备";
    case "/": return detail.method === "POST" ? "提交登录请求" : ["GET", "HEAD"].includes(detail.method) ? "读取登录页面" : "";
    case "/:login": return detail.method === "POST" ? "提交登录验证" : ["GET", "HEAD"].includes(detail.method) ? "读取登录验证" : "";
    case undefined: case "":
      return detail.stage === "login" ? detail.method === "POST" ? "提交登录验证" : "读取登录验证" : own(stageActions, detail.stage) ? stageActions[detail.stage] : "";
    default: return "";
  }
}

export function presentLogDiagnostic(record) {
  const detail = record?.reset_diagnostic;
  if (!detail || typeof detail !== "object" || Array.isArray(detail)) return {};
  if (detail.terminal === true) return { phaseLabel: "2FA步骤结果", requestObservation: detail.evidence_source === "transport" };
  if (detail.evidence_source !== "transport") return {};
  const complete = detail.reason_code === "REQUEST_COMPLETE" && detail.dispatch === "response_received" && detail.receipt === "complete"
    && Number.isInteger(detail.http_status) && detail.http_status >= 200 && detail.http_status < 400;
  const conflict = detail.business_code || detail.cause_reason || detail.last_write_issue;
  const action = requestAction(detail);
  const duration = Number.isSafeInteger(detail.duration_ms) && detail.duration_ms >= 0 ? ` · ${detail.duration_ms} ms` : "";
  const ordinaryStatus = ongoingStatuses.has(record.status) && ongoingStatuses.has(record.operationStatus) && record.level === "info";
  const message = complete && !conflict && action && ordinaryStatus ? `${action}：收到完整响应（HTTP ${detail.http_status}${duration}）；待解析业务结果` : "";
  // Hiding is stricter than translation. Missing, new or inconsistent evidence
  // remains visible. In particular a GET on a mutation endpoint is not a read.
  const routineNetwork = complete && !conflict && ordinaryStatus && record.stage === "totp_reset_diagnostic"
    && detail.terminal !== true && (detail.terminal === undefined || detail.terminal === false)
    && typeof detail.op_ref === "string" && /^O[0-9a-f]{16}$/.test(detail.op_ref)
    && typeof detail.attempt_ref === "string" && /^A[1-9][0-9]{0,6}$/.test(detail.attempt_ref)
    && Number.isSafeInteger(detail.seq) && detail.seq > 0 && detail.next_action === "classify_receipt"
    && detail.access === "read" && ["GET", "HEAD"].includes(detail.method)
    && typeof detail.path === "string" && own(readStages, detail.path) && readStages[detail.path].includes(detail.stage)
    && (detail.duration_ms === undefined || Number.isSafeInteger(detail.duration_ms) && detail.duration_ms >= 0)
    && ["started_at", "ended_at"].every(key => detail[key] === undefined || validTime(detail[key]))
    && Object.keys(detail).every(key => ordinaryFields.has(key));
  return { message, routineNetwork, requestObservation: true };
}

export const LOG_FILTER_LABELS = { progress: "关键进展", all: "全部", abnormal: "异常" };

export function selectLogEntries(entries, filter) {
  if (filter === "abnormal") return entries.filter(entry => entry.abnormal);
  if (filter === "progress") return entries.filter(entry => !entry.routineNetwork);
  return entries;
}

export function readableLogLine(entry) {
  return [entry.fullTime, `[${entry.label}]`, entry.phaseLabel && `[${entry.phaseLabel}]`, entry.message].filter(Boolean).join(" ");
}
