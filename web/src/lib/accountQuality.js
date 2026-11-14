import { compareRequestTime, isRateLimited } from "./accountHTTP.js";
export const QUALITY_REASONING_EFFORTS = Object.freeze(["none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra"]);
export const DEFAULT_QUALITY_SETTINGS = Object.freeze({ schemaVersion: 1, revision: 1, enabled: false, beforeUpload: false, blockDegraded: true, blockFailed: true, model: "", reasoningEffort: "high", minimumScore: 1, timeoutSeconds: 90, cooldownSeconds: 60, resultTTLSeconds: 900 });
const object = value => value && typeof value === "object" && !Array.isArray(value);
const count = value => Number.isSafeInteger(value) && value >= 0;
const date = value => typeof value === "string" && Number.isFinite(Date.parse(value));
export function qualitySettingsPayload(value) {
  const result = {};
  for (const key of Object.keys(DEFAULT_QUALITY_SETTINGS)) result[key] = typeof DEFAULT_QUALITY_SETTINGS[key] === "number" ? Number(value?.[key]) : typeof DEFAULT_QUALITY_SETTINGS[key] === "boolean" ? value?.[key] === true : String(value?.[key] ?? "").trim();
  return result;
}
export function validateQualitySettings(value) {
  const data = value?.settings || value;
  if (!object(data) || data.schemaVersion !== 1 || !count(data.revision) || data.revision < 1
    || ["enabled", "beforeUpload", "blockDegraded", "blockFailed"].some(key => typeof data[key] !== "boolean")
    || typeof data.model !== "string" || data.model.length > 128 || /[\r\n\t]/.test(data.model) || !QUALITY_REASONING_EFFORTS.includes(data.reasoningEffort)
    || data.minimumScore !== 1
    || ["timeoutSeconds", "cooldownSeconds", "resultTTLSeconds"].some(key => !count(data[key]))
    || data.timeoutSeconds < 1 || data.timeoutSeconds > 300 || data.cooldownSeconds > 86400 || data.resultTTLSeconds < 1 || data.resultTTLSeconds > 86400 || data.enabled && !data.model.trim()) throw new Error("推理探针配置无效：启用需填写模型，糖果题答对通过，超时 1–300 秒，冷却 0–86400 秒，有效期 1–86400 秒。");
  return { ...qualitySettingsPayload(data), persistenceWarning: typeof data.persistenceWarning === "string" ? data.persistenceWarning : "" };
}
export function validateQualityStatus(data) {
  if (!object(data) || ["enabled", "beforeUpload", "running", "settingsAvailable"].some(key => typeof data[key] !== "boolean") || !count(data.queued) || !count(data.active) || !count(data.revision) || !count(data.settingsRevision) || data.settingsRevision < 1) throw new Error("推理探针状态响应不完整，保留最近结果。");
  return data;
}
export function validateQualityList(data) {
  if (!object(data) || !object(data.snapshots) || Object.entries(data.snapshots).some(([id, item]) => !object(item) || item.id !== id || !id || !object(item.lastAttempt) || typeof item.state !== "string" || typeof item.canCheck !== "boolean" || !count(item.revision))) throw new Error("推理探针列表响应不完整，保留最近结果。");
  return { snapshots: data.snapshots, status: validateQualityStatus(data.status) };
}
export function validateQualityReceipt(data) {
  if (!object(data) || typeof data.checkId !== "string" || !Array.isArray(data.accepted) || !Array.isArray(data.merged) || !Array.isArray(data.skipped) || [...data.accepted, ...data.merged].some(id => typeof id !== "string" || !id) || data.skipped.some(item => !object(item) || typeof item.id !== "string" || typeof item.reason !== "string")) throw new Error("检测受理结果尚未确认，请核对状态，勿重复提交。");
  return data;
}
export function describeQualityReceipt(data) {
  const receipt = validateQualityReceipt(data);
  return { text: `已受理 ${receipt.accepted.length} 个，合并已有检测 ${receipt.merged.length} 个，跳过 ${receipt.skipped.length} 个。受理不代表检测通过。${receipt.skipped.length ? " " + receipt.skipped.slice(0, 2).map(item => item.reason).join("；") : ""}`, tone: receipt.skipped.length ? "warning" : "info", dismissAfterMs: receipt.skipped.length ? 0 : 6000 };
}
export function qualityDate(value, compact = false) {
  if (!date(value)) return "时间未知";
  const d = new Date(value), part = n => String(n).padStart(2, "0");
  return `${compact ? "" : d.getFullYear() + "-"}${part(d.getMonth() + 1)}-${part(d.getDate())} ${part(d.getHours())}:${part(d.getMinutes())}`;
}
export function qualityResult(snapshot) {
  const result = snapshot?.lastResult;
  return object(result) && ["normal", "degraded"].includes(result.verdict) && Number.isInteger(result.score) && Number.isInteger(result.total) && result.total > 0 && result.score >= 0 && result.score <= result.total && date(result.checkedAt) ? result : null;
}

// Require explicit absence of probe evidence. Eligibility and persistence of
// an empty default snapshot do not establish that a probe has been attempted.
export function qualityNeverChecked(snapshot) {
  const present = value => value !== undefined && value !== null && value !== "";
  if (!object(snapshot) || !object(snapshot.lastAttempt) || snapshot.lastAttempt.status !== "never") return false;
  const attempt = snapshot.lastAttempt;
  if (![undefined, null, "", "idle", "eligible", "unavailable", "disabled"].includes(snapshot.state)
    || ![undefined, null, "", "idle", "eligible", "unavailable", "disabled"].includes(snapshot.refreshState)) return false;
  if ([attempt.checkId, attempt.startedAt, attempt.finishedAt, attempt.errorCode, attempt.message, snapshot.lastResult, snapshot.httpObservation, snapshot.nextAllowedAt, snapshot.staleReason].some(present)
    || present(attempt.httpStatus) && attempt.httpStatus !== 0) return false;
  if (snapshot.probeHistory != null && (!Array.isArray(snapshot.probeHistory) || snapshot.probeHistory.length > 0)
    || snapshot.errors != null && (!Array.isArray(snapshot.errors) || snapshot.errors.length > 0)) return false;
  return ![snapshot.loading, snapshot.isLoading, snapshot.uncertain, snapshot.verificationRequired, snapshot.recoveryPending, snapshot.persistenceUnconfirmed].some(value => value === true)
    && ![snapshot.error, snapshot.lastError, snapshot.persistenceWarning].some(value => present(value) && value !== false);
}

// Historical observations never participate in admission or upload decisions.
// Missing legacy history is not filled with synthetic attempts.
export function qualityProbeHistory(snapshot, kind = "") {
  let history = snapshot?.probeHistory;
  if (!Array.isArray(history)) {
    const attempt = snapshot?.lastAttempt, result = qualityResult(snapshot);
    const completedFailure = attempt?.status === "failed" && date(attempt.startedAt) && date(attempt.finishedAt)
      && compareRequestTime(attempt.finishedAt, attempt.startedAt) >= 0
      && Number.isInteger(attempt.httpStatus) && attempt.httpStatus >= 100 && attempt.httpStatus <= 599;
    history = completedFailure
      ? [{ id: attempt.checkId || `legacy:${attempt.finishedAt}`, kind: "candy", status: "failed", finishedAt: attempt.finishedAt, errorCode: attempt.errorCode, httpStatus: attempt.httpStatus }]
      : result ? [{ ...result, id: attempt?.status === "success" && attempt.checkId || `legacy:${result.checkedAt}`, kind: "candy", status: "success", finishedAt: result.checkedAt }] : [];
  }
  const unique = new Map();
  for (const value of history) {
    if (!object(value) || typeof value.id !== "string" || !value.id.trim() || !["candy", "pelican"].includes(value.kind)
      || !["success", "failed", "cancelled"].includes(value.status) || !date(value.finishedAt)) continue;
    if (kind && value.kind !== kind) continue;
    // Older multi-question measurements remain available in details, but are
    // not evidence that the single candy question passed.
    if (kind === "candy" && value.status === "success" && value.total !== 1) continue;
    const measured = value.kind === "candy" && value.status === "success"
      && ["normal", "degraded"].includes(value.verdict) && count(value.score) && count(value.total) && value.total > 0 && value.score <= value.total
      && (value.total !== 1 || (value.verdict === "normal") === (value.score === value.total));
    if (value.kind === "candy" && value.status === "success" && !measured) continue;
    const httpStatus = Number.isInteger(value.httpStatus) && value.httpStatus >= 100 && value.httpStatus <= 599 ? value.httpStatus : 0;
    if (value.status === "success" && httpStatus && (httpStatus < 200 || httpStatus >= 300)) continue;
    const limited = isRateLimited(httpStatus, value.errorCode);
    const unconfirmed = ["invalid_response", "unexpected_content_type", "invalid_stream", "invalid_answer_format", "incomplete", "model_mismatch", "response_too_large", "superseded", "persistence_failed"].includes(value.errorCode);
    const tone = value.status === "cancelled" ? "neutral" : value.status === "failed" ? limited || unconfirmed ? "warning" : "danger" : measured && value.verdict === "degraded" ? "warning" : "success";
    const label = value.status === "cancelled" ? "已取消" : value.status === "failed" ? limited ? "请求限流" : unconfirmed ? "结果未确认" : "检测失败"
      : value.kind === "pelican" ? "绘图完成" : value.verdict === "normal" ? "通过" : "答案不符";
    const kindLabel = value.kind === "pelican" ? "鹈鹕绘图" : "糖果探针";
    const model = typeof value.model === "string" ? value.model.trim().slice(0, 128) : "";
    const title = [kindLabel, label, measured ? `${value.score}/${value.total} 题正确` : "", qualityDate(value.finishedAt), model && `模型：${model}`, httpStatus && `HTTP ${httpStatus}`].filter(Boolean).join(" · ");
    const item = { id: value.id, kind: value.kind, status: value.status, finishedAt: value.finishedAt, tone, label, title, measured };
    const key = `${item.kind}:${item.id}`, prior = unique.get(key);
    if (!prior || compareRequestTime(item.finishedAt, prior.finishedAt) >= 0) { unique.delete(key); unique.set(key, item); }
  }
  return [...unique.values()].sort((a, b) => compareRequestTime(a.finishedAt, b.finishedAt)).slice(-5);
}
export function describeQuality(snapshot, authorizationMethod) {
  const result = qualityResult(snapshot), attempt = snapshot?.lastAttempt || {}, state = snapshot?.state;
  const running = state === "active", queued = state === "queued", active = running || queued;
  const limited = isRateLimited(attempt.httpStatus, attempt.errorCode);
  const failed = attempt.status === "failed", cancelled = ["cancelled", "canceled"].includes(attempt.status), skipped = attempt.status === "skipped";
  const unsupported = !active && authorizationMethod !== "bps" && ["unsupported", "unsupported_authorization"].includes(attempt.errorCode);
  const stale = Boolean(result && snapshot?.stale);
  const staleReason = stale ? snapshot?.staleReason || "expired" : "";
  const staleLabel = ({ options_changed: "参数不同", rule_changed: "规则已更新", authorization_changed: "授权已变化", account_unavailable: "账号不可用", expired: "已过期" })[staleReason] || "已过期";
  // Queued/running snapshots are deliberately provisional. Only a terminal
  // persistence failure invalidates the claim that a saved result is current.
  const persistenceWarning = Boolean(result && snapshot?.persisted === false && !active);
  const unknown = ["invalid_response", "unexpected_content_type", "invalid_stream", "invalid_answer_format", "incomplete", "model_mismatch", "response_too_large", "superseded"].includes(attempt.errorCode)
    || Boolean(attempt.status && !["never", "queued", "running", "success", "failed", "cancelled", "canceled", "skipped"].includes(attempt.status));
  // The latest attempt is the headline. A retained passing observation is
  // historical evidence, never proof that a failed or interrupted recheck passed.
  const label = unsupported ? "暂不支持" : running ? "检测中" : queued ? "排队中" : unknown ? "结果未知" : failed ? limited ? "请求限流" : "检测失败" : cancelled ? "已取消" : skipped ? "已跳过" : persistenceWarning ? "保存未确认" : stale ? staleLabel : result ? result.verdict === "normal" ? result.total === 1 ? "糖果通过" : "通过阈值" : result.total === 1 ? "答案不符" : "低于阈值" : attempt.status === "never" || !snapshot ? "未检测" : "结果未知";
  const tone = unsupported || active ? "muted" : unknown ? "warning" : failed ? limited ? "warning" : "danger" : persistenceWarning ? "warning" : cancelled || skipped || stale ? "muted" : result ? result.verdict === "normal" ? "success" : "warning" : "muted";
  const historical = Boolean(result && (unsupported || active || unknown || failed || cancelled || skipped || stale || persistenceWarning));
  const latestLabel = unsupported ? "不支持" : running ? "检测中" : queued ? "排队中" : unknown ? "本次未知" : failed ? limited ? "本次限流" : "本次失败" : cancelled ? "本次取消" : skipped ? "本次跳过" : persistenceWarning ? "保存未确认" : staleLabel;
  const phase = historical ? `历史${result.score}/${result.total}·${latestLabel}` : unsupported ? "当前授权不支持" : running ? "正在执行探针" : queued ? "等待执行探针" : unknown ? "未取得有效测量" : failed ? "查看失败原因" : cancelled ? "本次已取消" : skipped ? "查看跳过原因" : result ? `${result.score}/${result.total} 题正确` : "推理探针·非标准 IQ";
  const message = attempt.message || snapshot?.unavailableReason || "";
  const timestamp = result?.checkedAt || attempt.finishedAt || attempt.startedAt;
  const score = result ? historical ? `历史 ${result.score}/${result.total}` : `${result.score}/${result.total} 题正确` : "";
  const summary = [score, stale ? staleLabel : "", persistenceWarning ? "保存未确认" : ""].filter(Boolean).join(" · ");
  return { result, label, tone, running, queued, active, stale, staleReason, staleLabel, phase, message, timestamp, historical, summary, persistenceWarning };
}
export function qualityCanSubmit({ settings, settingsLoaded, settingsLoading, verificationRequired, uncertain, saving, actionBusy, status, error }, snapshots, ids) {
  if (!settingsLoaded || settingsLoading || verificationRequired || uncertain || saving || actionBusy || error || !settings?.enabled || !settings.model.trim() || !status?.settingsAvailable || status.enabled !== true || status.running !== true) return false;
  return ids === null || ids.length > 0 && ids.some(id => snapshots[id]?.canCheck === true && snapshots[id]?.state === "eligible");
}
