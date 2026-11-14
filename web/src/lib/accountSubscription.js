import { normalizeAccountPlan } from "./console.js";
import { formatInsightDate } from "./accountInsights.js";
import { compareRequestTime, describeAccountRequestStatuses } from "./accountHTTP.js";

// Query failures affect presentation only. The saved plan remains available to
// workflows that require the last verified authorization or subscription.
export function subscriptionDisplayPlan(job, state = {}) {
  const checkedAt = describeSubscriptionObservation(job).observedAt;
  const sameRequest = !state.requestJob || subscriptionJobMatches(job, state.requestJob)
    && ["createdAt", "attempt", "lastOperationAt"].every(key => (job?.[key] ?? "") === (state.requestJob[key] ?? ""));
  const newerPlan = checkedAt && state.requestJob && compareRequestTime(checkedAt, state.checkedAt) > 0;
  if (sameRequest && !newerPlan && state.failed !== false && (state.failed || state.error)) return "unknown";
  const observations = (Array.isArray(job?.accountRequestObservations) ? job.accountRequestObservations : [])
    .filter(item => item?.source === "subscription" && item.historical !== true);
  const latest = describeAccountRequestStatuses({ requestObservations: observations }).latestStatus;
  // A verified plan from the same query takes precedence over optional endpoint
  // warnings. A subsequent failed query makes the displayed plan unknown again.
  if (latest?.status === "failed" && (!checkedAt || compareRequestTime(checkedAt, latest.startedAt || latest.observedAt) < 0)) return "unknown";
  return normalizeAccountPlan(job?.planType);
}

export function describeSubscriptionObservation(job) {
  const value = job?.subscriptionPlanCheckedAt;
  const observedAt = typeof value === "string" && Number.isFinite(Date.parse(value)) ? value : "";
  const source = typeof job?.subscriptionPlanSource === "string" ? job.subscriptionPlanSource.trim() : "";
  const sourceLabel = ({ chatgpt_accounts: "账号订阅信息", chatgpt_subscriptions: "订阅详情" })[source] || source;
  return { observedAt, source, sourceLabel, stale: job?.subscriptionPlanStale === true,
    text: observedAt ? `上次查询：${formatInsightDate(observedAt)}${job.subscriptionPlanStale === true ? " · 历史结果" : ""}` : "授权快照 · 尚未查询当前套餐" };
}

export function subscriptionJobMatches(job, expected) {
  const sameEmail = typeof job?.email === "string" && job.email.trim().toLowerCase() === String(expected.email || "").trim().toLowerCase();
  return Boolean(job && job.id === expected.id && sameEmail && job.authorizationMethod === expected.authorizationMethod);
}

export function subscriptionResponseIsCurrent(job, submitted, latest) {
  if (!latest || !subscriptionJobMatches(job, latest)) return false;
  const latestAt = Date.parse(latest.updatedAt), responseAt = Date.parse(job.updatedAt);
  if (Number.isFinite(latestAt)) return Number.isFinite(responseAt) && responseAt >= latestAt;
  // Older servers may omit version times. Never overwrite a changed row when
  // there is no reliable ordering evidence. A local GET can reconcile it.
  return latest === submitted || JSON.stringify(latest) === JSON.stringify(submitted);
}

export function subscriptionWarnings(values) {
  return values.flatMap(value => {
    if (typeof value === "string") return value.trim() ? [value.trim()] : [];
    if (typeof value?.message === "string" && value.message.trim()) return [value.message.trim()];
    if (typeof value?.code === "string" && value.code.trim()) return ["部分订阅信息未更新：" + value.code.trim()];
    return [];
  });
}

export function validateSubscriptionRefresh(data, expected) {
  const job = data?.job, subscription = data?.subscription;
  const plan = normalizeAccountPlan(subscription?.planType);
  if (!subscriptionJobMatches(job, expected)
    || subscription?.identityVerified !== true || !subscription.accountId || plan === "unknown"
    || normalizeAccountPlan(job.planType) !== plan || !describeSubscriptionObservation(job).observedAt
    || !subscription.observedAt || !Number.isFinite(Date.parse(subscription.observedAt))
    || Date.parse(job.subscriptionPlanCheckedAt) !== Date.parse(subscription.observedAt)
    || typeof subscription.source !== "string" || !subscription.source.trim()
    || job.subscriptionPlanSource !== subscription.source || !Array.isArray(data.warnings)) {
    throw new Error("套餐刷新响应未能核实账号、套餐或查询时间，已保留上次结果。");
  }
  return data;
}
