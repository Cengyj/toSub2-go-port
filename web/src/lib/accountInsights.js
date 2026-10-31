import { describeExpiration } from "./accountExpiration.js";

export function normalizeInsightSettings(value = {}) {
  if (!value || typeof value !== "object") value = {};
  const minutes = Number(value.intervalMinutes);
  return {
    schemaVersion: 1, revision: Number.isSafeInteger(value.revision) ? value.revision : 0,
    enabled: value.enabled === true, automatic: value.automatic !== false,
    intervalMinutes: Number.isInteger(minutes) && minutes >= 1 && minutes <= 1440 ? minutes : 15,
    includeResetCredits: value.includeResetCredits === true,
    excludedAccountIds: [...new Set((Array.isArray(value.excludedAccountIds) ? value.excludedAccountIds : []).filter(id => typeof id === "string" && id))],
  };
}

export function formatInsightDate(value) {
  const expiration = describeExpiration({ subscriptionExpiresAt: value });
  return expiration.dateTime ? expiration.label : "未知";
}

export function insightWindows(snapshot) {
  return Array.isArray(snapshot?.usage?.windows) ? snapshot.usage.windows : [];
}

export function insightWindowLabel(window = {}) {
  const seconds = Number(window.windowSeconds);
  if (seconds > 0 && Number.isFinite(seconds)) {
    if (seconds % 86400 === 0) return `${seconds / 86400} 天窗口`;
    if (seconds % 3600 === 0) return `${seconds / 3600} 小时窗口`;
    if (seconds % 60 === 0) return `${seconds / 60} 分钟窗口`;
    return `${seconds} 秒窗口`;
  }
  return typeof window.name === "string" && window.name ? window.name : "额度窗口";
}

export function insightPercent(value) {
  return typeof value === "number" && Number.isFinite(value) ? value : null;
}

export function insightWindowShortLabel(window = {}) {
  const seconds = Number(window.windowSeconds);
  if (seconds > 0 && Number.isFinite(seconds)) {
    if (seconds % 86400 === 0) return seconds / 86400 + "d";
    if (seconds % 3600 === 0) return seconds / 3600 + "h";
    if (seconds % 60 === 0) return seconds / 60 + "m";
    return seconds + "s";
  }
  return insightWindowLabel(window);
}

export function describeInsightReset(window = {}, now = Date.now()) {
  const reset = typeof window.resetsAt === "string" ? Date.parse(window.resetsAt) : NaN;
  if (!Number.isFinite(reset)) return { text: "重置时间未知", title: "上游尚未提供有效的重置时间", pending: false };
  const title = "预计重置：" + formatInsightDate(window.resetsAt);
  if (reset <= now) return { text: "预计已重置 · 待确认", title: title + "；等待查询确认，保留上次用量", pending: true };
  const minutes = Math.ceil((reset - now) / 60000);
  const hours = Math.floor(minutes / 60);
  const days = Math.floor(hours / 24);
  const remaining = days ? days + "天" + (hours % 24 ? hours % 24 + "小时" : "")
    : hours ? hours + "小时" + (minutes % 60 ? minutes % 60 + "分" : "") : minutes + "分钟";
  return { text: remaining + "后重置", title, pending: false };
}

export function insightSourceIsHistorical(snapshot, component) {
  if (snapshot?.stale === true || ["failed", "skipped", "cancelled", "canceled", "interrupted"].includes(snapshot?.lastAttempt?.status)) return true;
  const failed = (Array.isArray(snapshot?.errors) ? snapshot.errors : []).some(error => error.component === component || error.component === "query" || component === "resetCredits" && error.component === "usage");
  const observedAt = component === "resetCredits" ? snapshot?.usage?.resetCredits?.observedAt : snapshot?.[component]?.observedAt;
  const observed = Date.parse(observedAt), started = Date.parse(snapshot?.lastAttempt?.startedAt);
  // A different source can succeed while this observation is retained (for
  // example when reset-credit queries have been disabled). Preserve its age.
  if (Number.isFinite(observed) && Number.isFinite(started) && observed < started) return true;
  return failed && !(Number.isFinite(observed) && Number.isFinite(started) && observed >= started);
}

export function describeInsightSummary(snapshot, now = Date.now()) {
  if (!snapshot) return { label: "尚未查询", tone: "neutral", text: "查看用量与状态", observedAt: "", stale: false };
  const attempt = snapshot.lastAttempt || {};
  const windows = insightWindows(snapshot);
  const observedAt = snapshot.usage?.observedAt || snapshot.bps?.observedAt || snapshot.subscription?.observedAt || "";
  const source = snapshot.usage?.observedAt ? "usage" : snapshot.bps?.observedAt ? "bps" : "subscription";
  const stale = insightSourceIsHistorical(snapshot, source);
  const knownWindow = windows.find(item => insightPercent(item.usedPercent) !== null);
  const resetPending = windows.some(window => window && describeInsightReset(window, now).pending);
  let label = snapshot.usage?.limitReached === true ? stale || resetPending ? "上次查询已用尽" : "当前额度已用尽"
    : knownWindow ? `Codex ${insightWindowLabel(knownWindow)}已用 ${knownWindow.usedPercent}%`
    : snapshot.bps?.observedAt ? "BPS 权限信息已更新"
    : snapshot.usage?.observedAt ? "额度数值未知"
    : observedAt ? "账号查询信息已更新" : "尚未取得查询结果";
  const unpersistedAttempt = snapshot.persisted === false && Boolean(observedAt || attempt.finishedAt || ["success", "partial", "failed", "skipped", "cancelled", "canceled", "interrupted"].includes(attempt.status));
  let tone = snapshot.usage?.limitReached === true || unpersistedAttempt ? "warning" : "neutral";
  let text = observedAt ? `${stale ? "历史结果 · " : ""}${formatInsightDate(observedAt)}` : "查询不会重新登录或上传";
  if (["queued", "pending"].includes(attempt.status)) { label = "查询已排队"; tone = "active"; }
  else if (["running", "checking"].includes(attempt.status)) { label = "正在查询"; tone = "active"; }
  else if (attempt.status === "partial") { label = "查询部分更新"; tone = "warning"; }
  else if (attempt.status === "failed") { label = "本次查询失败"; tone = "warning"; }
  else if (attempt.status === "skipped") { label = "本次查询已跳过"; text = attempt.message || snapshot.unavailableReason || text; }
  else if (["cancelled", "canceled", "interrupted"].includes(attempt.status)) { label = "查询已取消或中断"; }
  if (unpersistedAttempt) text = `本次查询记录保存未确认 · ${text}`;
  return { label, tone, text, observedAt, stale };
}

export function describeInsightSubmission(data = {}) {
  const accepted = Array.isArray(data.accepted) ? data.accepted.length : 0;
  const merged = Array.isArray(data.merged) ? data.merged.length : 0;
  const skipped = Array.isArray(data.skipped) ? data.skipped.length : 0;
  const parts = [];
  if (accepted) parts.push(`已受理 ${accepted} 个`);
  if (merged) parts.push(`已合并在途查询 ${merged} 个`);
  if (skipped) parts.push(`已跳过 ${skipped} 个`);
  const reasons = [...new Set((Array.isArray(data.skipped) ? data.skipped : []).map(item => item.reason).filter(value => typeof value === "string" && value))];
  return { tone: skipped ? "warning" : accepted || merged ? "active" : "neutral", text: `${parts.join("，") || "未受理新的查询"}。${accepted || merged ? "受理不代表查询完成，请查看各账号结果。" : ""}${reasons.length ? ` 跳过原因：${reasons.join("；")}` : ""}` };
}

export function getInsightExpirationJob(job, snapshot) {
  const subscription = snapshot?.subscription;
  if (!job || snapshot?.id !== job.id || snapshot.authorizationMethod !== job.authorizationMethod
    || subscription?.identityVerified !== true || subscription.reported !== true) return job;
  if (subscription.expiresAt === null) return { ...job, subscriptionExpiresAt: "", ...(job.expiresAtSource === "subscription" ? { expiresAt: "", expiresAtSource: "unknown" } : {}) };
  if (!describeExpiration({ subscriptionExpiresAt: subscription.expiresAt }).dateTime) return job;
  return { ...job, subscriptionExpiresAt: subscription.expiresAt };
}

export function insightSnapshotForJob(job, snapshot) {
  return snapshot?.id === job?.id && snapshot.authorizationMethod === job.authorizationMethod ? snapshot : undefined;
}

const insightErrorSources = new Map([["usage", "Codex 额度"], ["bps", "BPS 模型权限"], ["subscription", "账号与订阅"], ["resetCredits", "额度重置券"], ["query", "查询任务"]]);
const insightErrorText = value => typeof value === "string" ? value.trim() : "";
const insightErrorTime = value => typeof value === "string" && Number.isFinite(Date.parse(value)) ? value : "";

function insightErrorHint(code, httpStatus) {
  switch (code) {
    case "permission_denied": return "该来源拒绝查询，请核对权限及授权方式；不代表整个账号失效。";
    case "authorization_rejected":
    case "invalid_credentials": return "请核对账号已保存的授权与当前授权方式；查询不会自动重登或刷新授权。";
    case "unsupported_authorization": return "该来源不支持当前授权方式；其他来源的有效结果仍保留。";
    case "identity_mismatch": return "返回数据与账号身份不匹配，未采纳该来源结果；请核对账号归属。";
    case "query_timeout":
    case "timeout":
    case "network_error":
    case "transport_unavailable": return "请检查本地网络及代理，恢复后按冷却限制手动查询。";
    case "rate_limited": return "上游已限流，请避免重复提交，并遵循服务端冷却时间。";
    case "upstream_error": return httpStatus === 402 ? "上游以 HTTP 402 拒绝读取，请结合错误详情核对套餐与权限；仅凭此状态码不能确认欠费或过期。" : "上游暂未完成读取，请结合错误详情核实，并遵循当前查询门禁。";
    case "invalid_timezone": return "请检查服务配置中的请求时区。";
    case "challenge_or_html": return "上游返回验证页或非接口内容；请检查网络访问情况，不会自动进行浏览器验证。";
    case "invalid_response":
    case "protocol_error":
    case "empty_response": return "上游未返回可确认的结果，已有有效观察仍保留；请稍后手动查询核实。";
    case "cancelled":
    case "canceled": return "查询已停止，已有结果保留；需要更新时请明确手动发起查询。";
    default: return "暂无该错误码的专用处理建议，请依据错误信息核实；查看错误不会触发登录或重试。";
  }
}

// Consume only the public, server-sanitized error DTO. Never stringify the
// response or expose unrecognized fields such as credentials or raw bodies.
export function describeInsightErrors(snapshot, now = Date.now()) {
  const errors = [];
  const add = (value, component, origin, nextAllowedAt) => {
    if (!value || typeof value !== "object" || Array.isArray(value)) return;
    const source = insightErrorText(component) || insightErrorText(value.component) || "query";
    const item = {
      component: source, sourceLabel: insightErrorSources.get(source) || "其他来源",
      code: insightErrorText(value.code), message: insightErrorText(value.message),
      httpStatus: Number.isInteger(value.httpStatus) && value.httpStatus >= 100 && value.httpStatus <= 599 ? value.httpStatus : 0,
      retryAfter: insightErrorTime(value.retryAfter), nextAllowedAt: insightErrorTime(nextAllowedAt), origin,
      upstreamCode: insightErrorText(value.upstreamCode), upstreamType: insightErrorText(value.upstreamType), transportReason: insightErrorText(value.transportReason),
    };
    const existing = errors.find(error => error.component === item.component && error.code === item.code && error.message === item.message && error.httpStatus === item.httpStatus && error.upstreamCode === item.upstreamCode && error.upstreamType === item.upstreamType && error.transportReason === item.transportReason);
    if (existing) {
      if (origin === "retained") existing.origin = origin;
      if (item.nextAllowedAt) existing.nextAllowedAt = item.nextAllowedAt;
      return;
    }
    errors.push(item);
  };
  for (const error of Array.isArray(snapshot?.errors) ? snapshot.errors : []) add(error, "", "reported", snapshot?.components?.[error?.component]?.nextAllowedAt);
  const components = snapshot?.components;
  if (components && typeof components === "object" && !Array.isArray(components)) {
    for (const [component, state] of Object.entries(components)) add(state?.lastError, component, "retained", state?.nextAllowedAt);
  }
  const attempt = snapshot?.lastAttempt || {};
  const failedAttempt = ["failed", "partial"].includes(attempt.status) || ["skipped", "cancelled", "canceled", "interrupted"].includes(attempt.status) && (attempt.errorCode || attempt.message);
  const attemptCode = insightErrorText(attempt.errorCode), attemptMessage = insightErrorText(attempt.message);
  const specificAttemptError = attemptCode || ["failed", "skipped", "cancelled", "canceled", "interrupted"].includes(attempt.status) && attemptMessage;
  const distinctAttemptError = specificAttemptError && !errors.some(error => (!attemptCode || error.code === attemptCode) && error.message === attemptMessage);
  if (failedAttempt && (!errors.length || distinctAttemptError)) {
    add({code:attempt.errorCode,message:attempt.message}, "query", "attempt", snapshot?.nextAllowedAt);
    const attemptIndex = errors.findIndex(error => error.origin === "attempt");
    if (attemptIndex > 0) errors.unshift(...errors.splice(attemptIndex, 1));
  }
  return errors.map(error => {
    const message = error.message || "原因未知：后端未提供可显示的错误说明。";
    const historical = ["queued", "pending", "running", "checking"].includes(attempt.status);
    const dates = [error.retryAfter, error.nextAllowedAt].filter(Boolean).sort((a, b) => Date.parse(b) - Date.parse(a));
    const cooldown = dates[0];
    const cooldownText = cooldown ? Date.parse(cooldown) > now ? "冷却至：" + formatInsightDate(cooldown) + "；到期后仍需符合查询条件。" : "记录中的冷却时间已过；是否恢复仍需查询确认。" : "后端未提供重试时间；请遵循当前查询门禁。";
    const summary = [historical ? "历史错误" : "", error.httpStatus ? "HTTP " + error.httpStatus : "", error.sourceLabel].filter(Boolean).join(" · ") + "：" + message + (error.transportReason && !message.includes(error.transportReason) ? "（" + error.transportReason + "）" : "");
    const characters = Array.from(summary);
    return {...error, message, historical, cooldownText, hint:insightErrorHint(error.code, error.httpStatus),
      originLabel: error.origin === "attempt" ? "最近查询记录" : historical ? "来源历史错误记录" : error.origin === "retained" ? "来源最近错误" : "查询返回的错误",
      originNote: historical || error.origin === "retained" ? "保存的最近错误，可能因冷却从前次查询保留；不代表本次再次请求或再次失败。" : "",
      summary, shortText: characters.length > 120 ? characters.slice(0, 120).join("") + "…" : summary};
  });
}
