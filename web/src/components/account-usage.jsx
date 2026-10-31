import React from "react";
import { Activity } from "lucide-react";
import { describeInsightSummary, describeInsightErrors, describeInsightReset, formatInsightDate, insightPercent, insightWindowLabel, insightWindowShortLabel, insightWindows, insightSourceIsHistorical } from "../lib/accountInsights.js";
import "./account-usage.css";

// Pure presentation: the caller supplies the observation and its clock. Neither
// the compact table meter nor the detail meter performs requests or schedules work.
export function UsageMeter({ window, now = Date.now(), compact = false, trailingStatus = null }) {
  const observation = window && typeof window === "object" ? window : {};
  const percent = insightPercent(observation.usedPercent);
  const label = insightWindowLabel(observation);
  const reset = describeInsightReset(observation, now);
  const tone = percent === null ? "unknown" : percent >= 90 ? "danger" : percent >= 75 ? "warning" : "normal";
  const resetDate = typeof observation.resetsAt === "string" && Number.isFinite(Date.parse(observation.resetsAt));
  const resetText = compact ? reset.text : !resetDate ? "重置时间未知"
    : reset.pending ? `预计已重置（${formatInsightDate(observation.resetsAt)}），待更新确认`
    : `预计重置：${formatInsightDate(observation.resetsAt)}`;
  return <span className={`usage-window usage-meter is-${tone} ${compact ? "is-compact" : "is-detailed"}`}>
    <span className="usage-window-line">
      <span className="usage-window-label" title={label}>{compact ? insightWindowShortLabel(observation) : label}</span>
      <span className="usage-value">{!compact && percent !== null && <span className="usage-value-kind">已用 </span>}<span className="usage-percent">{percent === null ? compact ? "未知" : "用量未知" : percent + "%"}</span></span>
    </span>
    {percent === null ? <span className="usage-track-unknown" aria-hidden="true" />
      : <progress max="100" value={Math.max(0, Math.min(100, percent))} aria-label={`${label}已用 ${percent}%`} />}
    <span className="usage-reset-row">
      <span className={"usage-reset" + (reset.pending ? " is-pending" : "")} title={reset.title}>{resetText}</span>
      {trailingStatus?.label && <span className="usage-inline-status" title={trailingStatus.title || trailingStatus.label} aria-label={trailingStatus.title || trailingStatus.label}>{trailingStatus.label}</span>}
    </span>
  </span>;
}

export function AccountInsightSummary({ snapshot, onOpen, accountLabel = "", now = Date.now() }) {
  const summary = describeInsightSummary(snapshot, now);
  const errors = describeInsightErrors(snapshot, now);
  const windows = insightWindows(snapshot).filter(window => window && typeof window === "object");
  const visible = windows.slice(0, 2);
  const historical = Boolean(snapshot?.usage ? insightSourceIsHistorical(snapshot, "usage") : snapshot?.bps && insightSourceIsHistorical(snapshot, "bps"));
  const historyLabel = snapshot?.usage ? "历史额度" : "历史权限";
  const awaitingReset = windows.some(window => describeInsightReset(window, now).pending);
  const limitLabel = snapshot?.usage?.limitReached === true ? historical || awaitingReset ? "上次已用尽" : "额度已用尽" : "";
  const httpError = errors.find(error => error.httpStatus > 0);
  // List status is deliberately small. Detailed explanations stay in the dialog.
  const trailingStatus = httpError ? {label:`HTTP ${httpError.httpStatus}`, title:`${httpError.sourceLabel} · HTTP ${httpError.httpStatus}`}
    : limitLabel ? {label:limitLabel, title:`Codex 额度 · ${limitLabel}`}
    : errors.length ? {label:"查询异常", title:`${errors[0].sourceLabel} · HTTP 状态未知`} : null;
  const exhaustedWindowIndex = !httpError && limitLabel ? visible.findIndex(window => {
    const percent = insightPercent(window.usedPercent);
    return percent !== null && percent >= 100;
  }) : -1;
  const statusWindowIndex = exhaustedWindowIndex >= 0 ? exhaustedWindowIndex : visible.length - 1;
  const emptyLabel = snapshot?.bps?.allowed === false ? "BPS 权限受限" : snapshot?.bps?.allowed === true ? "BPS 权限可用"
    : snapshot?.bps ? "BPS 权限未知" : "额度未知";
  const values = visible.map(window => insightWindowLabel(window) + "已用 " + (insightPercent(window.usedPercent) === null ? "未知" : window.usedPercent + "%") + "，" + describeInsightReset(window, now).text).join("；");
  const observedAt = formatInsightDate(summary.observedAt);
  const observedSource = snapshot?.usage?.observedAt ? "额度" : snapshot?.bps?.observedAt ? "权限" : "订阅";
  const observedLabel = observedSource + "观察";
  const extraWindows = windows.length - visible.length;
  const observedCompact = observedAt.length === 16 ? observedAt.slice(5) : observedAt;
  const persistenceUnconfirmed = summary.text.includes("保存未确认");
  const attemptAnnouncement = ["queued", "pending", "running", "checking", "partial", "failed", "skipped", "cancelled", "canceled", "interrupted"].includes(snapshot?.lastAttempt?.status) ? summary.label : "";
  const title = [values || emptyLabel, trailingStatus?.title, limitLabel && httpError ? limitLabel : "", historical ? historyLabel : "",
    summary.observedAt ? `${observedLabel}：${observedAt}` : "", extraWindows > 0 ? `另有 ${extraWindows} 个窗口` : "",
    persistenceUnconfirmed ? "记录保存未确认" : "", "查看用量与状态"].filter(Boolean).join("；");

  return <button type="button" className={"insight-summary account-usage" + (historical ? " has-history" : "")} onClick={onOpen}
    aria-label={[accountLabel, title, attemptAnnouncement].filter(Boolean).join("；")} title={title}>
    {visible.length ? <span className="usage-windows">{visible.map((window, index) => <UsageMeter key={`${window.name || "window"}-${index}`} window={window} now={now} compact trailingStatus={index === statusWindowIndex ? trailingStatus : null} />)}</span>
      : <>
        <span className="usage-empty"><span className="usage-empty-icon"><Activity size={16} aria-hidden="true" /></span><span className="usage-empty-copy"><strong>{emptyLabel}</strong><small>{snapshot?.bps ? "额度数值未知" : "暂无额度窗口"}</small></span></span>
        <span className="usage-reset-row usage-empty-reset"><span className="usage-reset">重置时间未知</span>{trailingStatus && <span className="usage-inline-status" title={trailingStatus.title} aria-label={trailingStatus.title}>{trailingStatus.label}</span>}</span>
      </>}

    {(historical || extraWindows > 0 || summary.observedAt) && <span className="usage-footer">
      {(historical || extraWindows > 0) && <span className="usage-meta">
        {historical && <span className="usage-history-label">{historyLabel}</span>}
        {extraWindows > 0 && <span className="usage-extra">另有 {extraWindows} 个窗口</span>}
      </span>}
      {summary.observedAt && <span className="usage-observed" title={`${observedLabel}：${observedAt}`}><span>{observedLabel}</span>{observedAt === "未知" ? <span>未知</span> : <time dateTime={summary.observedAt}>{observedCompact}</time>}<span className="usage-open" aria-hidden="true">详情 ›</span></span>}
    </span>}
  </button>;
}
