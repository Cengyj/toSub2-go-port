export function isRateLimited(httpStatus, code) {
  return httpStatus === 429 || code === "rate_limited" && !(Number.isInteger(httpStatus) && httpStatus >= 400 && httpStatus <= 599);
}

export function httpStatusTone(httpStatus, code) {
  return isRateLimited(httpStatus, code) ? "warning" : "danger";
}

// This observation is selected and bound to the current authorization by the
// server. Never reconstruct an HTTP code from a historical probe classification.
export function describeProbeHTTPStatus(snapshot) {
  const observation = snapshot?.httpObservation;
  if (!observation || observation.status !== "failed" || !["candy", "pelican"].includes(observation.source)
    || typeof observation.finishedAt !== "string" || !Number.isFinite(Date.parse(observation.finishedAt))) return null;
  const status = Number.isInteger(observation.httpStatus) && observation.httpStatus >= 400 && observation.httpStatus <= 599 ? observation.httpStatus : 0;
  const code = typeof observation.errorCode === "string" ? observation.errorCode : "";
  const fallback = { unauthorized: "授权异常", forbidden: "权限受限", rate_limited: "限流" }[code];
  if (!status && (!fallback || observation.httpStatus && observation.httpStatus !== 0)) return null;
  const sourceLabel = observation.source === "pelican" ? "鹈鹕绘图" : "糖果探针";
  const limited = isRateLimited(status, code);
  return {
    httpStatus: status, code, source: observation.source, sourceLabel, observedAt: observation.finishedAt,
    model: typeof observation.model === "string" ? observation.model : "",
    message: typeof observation.message === "string" && observation.message.trim() ? observation.message : limited ? "上游请求受到限流，请稍后再试。" : "上游拒绝了检测请求，请核对账号权限和模型。",
    label: status ? `HTTP ${status}${limited ? " 限流" : ""}` : fallback,
    title: `${sourceLabel} · ${status ? `HTTP ${status}` : fallback}${limited ? " · 请求限流" : ""}`,
    tone: httpStatusTone(status, code), limited,
  };
}

const requestSourceLabels = new Map([
  ["subscription", "套餐查询"], ["models", "模型列表"], ["candy", "糖果探针"], ["pelican", "鹈鹕绘图"],
  ["authentication", "登录授权"], ["refresh", "刷新授权"], ["totp", "2FA 操作"], ["password", "密码操作"], ["logout", "退出设备"],
  ["usage", "Codex 额度"], ["bps", "BPS 模型权限"], ["resetCredits", "额度重置券"],
]);
const requestText = value => typeof value === "string" ? value.trim() : "";
const requestTime = value => typeof value === "string" && Number.isFinite(Date.parse(value)) ? value : "";

function requestFailureLabel(httpStatus, code, fallback = "请求异常") {
  if (httpStatus >= 300) return `HTTP ${httpStatus}${isRateLimited(httpStatus, code) ? " 限流" : ""}`;
  return ({unauthorized: "授权异常", authorization_rejected: "授权异常", invalid_credentials: "授权异常",
    forbidden: "权限受限", permission_denied: "权限受限", rate_limited: "限流"})[code] || (httpStatus ? "响应异常" : fallback);
}

function normalizeRequestObservation(value) {
  if (!value || typeof value !== "object" || Array.isArray(value) || !requestSourceLabels.has(value.source)
    || !["failed", "success"].includes(value.status) || !requestTime(value.finishedAt)) return null;
  const httpStatus = value.httpStatus == null ? 0 : value.httpStatus;
  if (httpStatus !== 0 && !(Number.isInteger(httpStatus) && httpStatus >= 100 && httpStatus <= 599)) return null;
  if (value.status === "success" && httpStatus !== 0 && (httpStatus < 200 || httpStatus >= 300)) return null;
  const code = requestText(value.errorCode), label = requestFailureLabel(httpStatus, code);
  return {source: value.source, component: value.source, sourceLabel: requestSourceLabels.get(value.source), status: value.status,
    httpStatus, code, message: requestText(value.message) || "后端未提供可显示的错误说明。", observedAt: value.finishedAt,
    startedAt: requestTime(value.startedAt), historical: value.historical === true, label, tone: httpStatusTone(httpStatus, code), limited: isRateLimited(httpStatus, code)};
}

// Preserve sub-millisecond order from Go's RFC3339Nano timestamps. Date.parse
// alone rounds distinct requests into the same millisecond.
export function compareRequestTime(left, right) {
  const a = Date.parse(left), b = Date.parse(right);
  const am = Number.isFinite(a) ? a : -Infinity, bm = Number.isFinite(b) ? b : -Infinity;
  if (am !== bm) return am > bm ? 1 : -1;
  if (!Number.isFinite(am)) return 0;
  const tail = value => (String(value).match(/\.(\d+)(?:Z|[+-]\d{2}:\d{2})$/)?.[1] || "").padEnd(9, "0").slice(3, 9);
  return tail(left).localeCompare(tail(right));
}

// The usage surface has one current request status across all account sources.
// Historical source records stay on the server; a newer confirmed success hides
// older failures here. Never rank errors by HTTP number or apparent severity.
export function describeAccountRequestStatuses({queryErrors = [], querySnapshot, qualitySnapshot, requestObservations} = {}) {
  const batches = new Map();
  const positions = new Map();
  for (const value of Array.isArray(requestObservations) ? requestObservations : []) {
    const item = normalizeRequestObservation(value);
    if (!item) continue;
    positions.set(item, positions.size);
    const at = item.startedAt || item.observedAt, batch = batches.get(item.source);
    const order = batch ? compareRequestTime(at, batch.at) : 1;
    if (order > 0) batches.set(item.source, {at, items: [item]});
    else if (order === 0) batch.items.push(item);
  }
  const legacyProbe = qualitySnapshot?.httpObservation;
  const normalizedProbe = ["candy", "pelican"].includes(legacyProbe?.source) ? normalizeRequestObservation(legacyProbe) : null;
  const probeBatch = batches.get(normalizedProbe?.source);
  if (probeBatch && probeBatch.items.every(item => compareRequestTime(normalizedProbe.observedAt, item.observedAt) > 0)) batches.delete(normalizedProbe.source);
  const candidates = [];
  const remainingQueries = (Array.isArray(queryErrors) ? queryErrors : []).filter(error => !batches.has(error.component));
  for (const error of remainingQueries) {
    const observedAt = requestTime(error.observedAt) || (error.origin !== "retained" ? requestTime(querySnapshot?.lastAttempt?.finishedAt) : "");
    candidates.push({kind: "query", item: {...error, source: error.component, status: "failed", observedAt,
      startedAt: observedAt ? requestTime(querySnapshot?.lastAttempt?.startedAt) : "",
      label: error.code === "cache_unavailable" ? "缓存不可读" : requestFailureLabel(error.httpStatus, error.code, "查询异常")}});
  }
  if (remainingQueries.every(error => error.origin === "retained") && querySnapshot?.lastAttempt?.status === "success" && requestTime(querySnapshot.lastAttempt.finishedAt)) {
    candidates.push({kind: "query", item: {source: "query", status: "success", startedAt: requestTime(querySnapshot.lastAttempt.startedAt), observedAt: querySnapshot.lastAttempt.finishedAt}});
  }
  if (normalizedProbe && !batches.has(normalizedProbe.source)) candidates.push({kind: "probe", item: {...normalizedProbe, model: requestText(legacyProbe.model)}});
  for (const batch of batches.values()) {
    const failures = batch.items.filter(item => item.status === "failed");
    for (const item of failures.length ? failures : batch.items) candidates.push({kind: "request", item, order: positions.get(item)});
  }
  let latest = null;
  for (const candidate of candidates) {
    if (!latest) { latest = candidate; continue; }
    const left = candidate.item, right = latest.item;
    const order = compareRequestTime(left.startedAt || left.observedAt, right.startedAt || right.observedAt)
      || compareRequestTime(left.observedAt, right.observedAt);
    // Equal-time entries retain backend observation order, never HTTP ordering.
    if (order > 0 || order === 0 && (candidate.order ?? -1) >= (latest.order ?? -1)) latest = candidate;
  }
  const empty = {queryErrors: [], probeStatus: null, requestErrors: [], badges: [], latestStatus: latest?.item || null};
  if (!latest || latest.item.status !== "failed") return empty;
  const item = latest.item, label = (item.historical ? "上次 " : "") + item.label;
  const sourceLabel = item.sourceLabel || "账号请求";
  const title = [sourceLabel + (item.historical ? "（上次）" : ""), item.observedAt || "记录未提供请求时间", label].join(" · ");
  const badge = {key: "latest-request", httpStatus: item.httpStatus || 0, label, sources: [item.source], sourceLabels: [sourceLabel],
    hasProbe: ["candy", "pelican"].includes(item.source), hasCode: Boolean(item.code), historical: Boolean(item.historical),
    title, tone: item.tone || httpStatusTone(item.httpStatus, item.code)};
  return {...empty, badges: [badge], queryErrors: latest.kind === "query" ? [item] : [],
    probeStatus: latest.kind === "probe" ? {...item, title} : null, requestErrors: latest.kind === "request" ? [item] : []};
}
