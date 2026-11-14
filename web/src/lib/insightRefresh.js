import { describeInsightQueryActivity } from "./accountInsights.js";

const countFields = ["total", "eligible", "queued", "active", "coolingDown", "unavailable", "retryAfterSeconds"];

export function insightRefreshIds(ids = []) {
  return [...new Set(ids.filter(id => typeof id === "string" && id))].sort();
}

// Row/detail actions use the already-read target snapshots. Unknown server
// projections remain unavailable; time alone never makes them eligible.
export function summarizeInsightRefreshTargets(snapshots, targetIds, now = Date.now()) {
  const rows = targetIds.map(id => snapshots[id]);
  const known = rows.every((row, index) => row?.id === targetIds[index] && typeof row.canQuery === "boolean");
  const aggregate = { total: rows.length, eligible: 0, queued: 0, active: 0, coolingDown: 0, unavailable: 0, retryAfterSeconds: 0 };
  for (const row of rows) {
    const activity = describeInsightQueryActivity(row, false);
    // Unknown projections remain unavailable. Only an absent field uses
    // the older server contract; live eligibility still requires canQuery.
    const projected = row != null && Object.prototype.hasOwnProperty.call(row, "refreshState");
    const nextAllowed = Date.parse(row?.nextAllowedAt);
    if (activity.phase === "running") aggregate.active += 1;
    else if (activity.phase === "queued") aggregate.queued += 1;
    else if (projected ? row.refreshState === "coolingDown" : nextAllowed > now) {
      aggregate.coolingDown += 1;
      if (nextAllowed > now) {
        const seconds = Math.ceil((nextAllowed - now) / 1000);
        aggregate.retryAfterSeconds = aggregate.retryAfterSeconds ? Math.min(aggregate.retryAfterSeconds, seconds) : seconds;
      }
    } else if ((!projected || row.refreshState === "eligible") && row?.canQuery === true) aggregate.eligible += 1;
    else aggregate.unavailable += 1;
  }
  return { known, aggregate };
}

export function validateInsightRefreshStatus(data, selectionRequested) {
  const aggregate = value => value && typeof value === "object" && !Array.isArray(value)
    && countFields.every(key => Number.isSafeInteger(value[key]) && value[key] >= 0)
    && value.total === value.eligible + value.queued + value.active + value.coolingDown + value.unavailable
    && (value.nextAllowedAt === undefined || typeof value.nextAllowedAt === "string" && Number.isFinite(Date.parse(value.nextAllowedAt)));
  if (!data || typeof data.enabled !== "boolean" || typeof data.automatic !== "boolean"
    || !aggregate(data.refresh) || selectionRequested && !aggregate(data.selection)) {
    throw new Error("账号查询状态响应不完整，暂不能提交刷新；请重新核对状态。");
  }
  return data;
}

export function validateInsightRefreshReceipt(data) {
  const ids = value => Array.isArray(value) && value.every(id => typeof id === "string" && id);
  if (!data || typeof data !== "object" || Array.isArray(data) || typeof data.queryId !== "string" || !data.queryId
    || !ids(data.accepted) || !ids(data.merged) || !Array.isArray(data.skipped)
    || !data.skipped.every(item => item && typeof item.id === "string" && item.id && typeof item.reason === "string")) {
    const error = new Error("查询受理回执不完整，无法确认本次是否受理；未自动重试，请查看最新查询状态。");
    error.outcome = "unknown";
    throw error;
  }
  const allIds = [...data.accepted, ...data.merged, ...data.skipped.map(item => item.id)];
  if (new Set(allIds).size !== allIds.length) {
    const error = new Error("查询受理回执包含冲突账号，结果尚未确认；请重新核对状态。");
    error.outcome = "unknown";
    throw error;
  }
  return data;
}

export function describeInsightRefreshControl({ selected = false, selectionCount = 0, token, configured, configurationPending, online = true, statusKnown, statusLoading, statusError, aggregate, pending, actionBusy }) {
  const label = selected ? "刷新所选用量" : "用量查询状态";
  const blocked = (phase, description, busy = false, title = label) => ({ disabled: true, busy, label: title, description, phase });
  if (selected && selectionCount === 0) return blocked("unavailable", "请先选择要刷新用量的账号。");
  if (selected && selectionCount > 1000) return blocked("unavailable", "一次最多选择 1000 个账号，请减少选择后重试。");
  if (!token) return blocked("unavailable", "控制台尚未连接，请等待连接恢复。");
  if (!configured) return blocked("unavailable", "请先确认并启用账号查询配置。");
  if (configurationPending) return blocked("unavailable", "账号查询配置正在保存或尚待核实，请先完成配置核对。");
  if (!online) return blocked("unknown", "浏览器已离线，联网后将自动重新核对查询状态。");
  if (pending === "submitting" || actionBusy) return blocked("submitting", "正在提交查询操作，请勿重复点击。", true, "正在提交…");
  if (statusError) return blocked("unknown", "查询状态暂不可读，已保留上次数据；请核对刷新状态后再操作。");
  if (pending === "checking" || statusLoading && !statusKnown) return blocked("checking", "正在核对服务端状态；受理不代表查询完成。", true, "正在核对状态…");
  if (!statusKnown || !aggregate) return blocked("unknown", "查询状态尚未确认，请先重新核对状态。");
  const counts = `可查询 ${aggregate.eligible} 个，排队 ${aggregate.queued} 个，执行中 ${aggregate.active} 个，冷却 ${aggregate.coolingDown} 个，不可查询 ${aggregate.unavailable} 个。`;
  if (aggregate.active) return blocked("running", counts + "请等待当前范围的查询结束。", true, `刷新中（${aggregate.active}）`);
  if (aggregate.queued) return blocked("queued", counts + "请等待当前范围的查询执行。", true, `排队中（${aggregate.queued}）`);
  if (aggregate.eligible) return { disabled: false, busy: false, label, description: counts + "只查询用量与状态，不触发登录、授权刷新或上传。", phase: "ready" };
  if (aggregate.coolingDown) return blocked("cooldown", counts + (aggregate.retryAfterSeconds ? `最早 ${aggregate.retryAfterSeconds} 秒后可重试；以服务端状态为准。` : "等待服务端确认冷却结束。"), false, "冷却中");
  return blocked("unavailable", aggregate.total ? counts + "请查看账号详情中的不可查询原因。" : "当前范围没有可查询的账号。");
}
