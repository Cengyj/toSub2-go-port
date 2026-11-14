import React from "react";
import { Clock3, LoaderCircle } from "lucide-react";
import { describeInsightSummary, describeInsightQueryActivity, describeInsightErrors, describeInsightReset, formatInsightDate, insightNeverQueried, insightPercent, insightWindowLabel, insightWindowShortLabel, insightWindows, insightSourceIsHistorical } from "../lib/accountInsights.js";
import { AccountMetricState } from "./account-metric-state.jsx";
import "./account-usage.css";
import { describeAccountRequestStatuses } from "../lib/accountHTTP.js";

// Pure presentation: the caller supplies the observation and its clock. Neither
// the compact table meter nor the detail meter performs requests or schedules work.
function UsageQuerySlot({ activity }) {
  const Icon = activity?.busy ? LoaderCircle : Clock3;
  return <span className={"usage-query-slot" + (activity?.active ? ` is-${activity.phase}` : " is-idle")} title={activity?.active ? activity.description : undefined} aria-hidden={!activity?.active}>
    {activity?.active && <><Icon size={11} className={activity.busy ? "spin" : undefined} aria-hidden="true" /><span className="usage-query-label">{activity.busy ? activity.hasUsage ? "刷新中" : "查询中" : "排队中"}</span></>}
  </span>;
}

// Preserve countdown rounding and uncertainty, with no repeated window name.
function compactResetText(window, now) {
  const reset = describeInsightReset(window, now);
  if (reset.pending) return "重置待确认";
  if (reset.text === "重置时间未知") return "";
  return reset.text;
}

function compactWindowLabel(window) {
  const label = insightWindowShortLabel(window);
  const seconds = Number(window?.windowSeconds);
  return Number.isFinite(seconds) && seconds > 0 ? label.replace(/h$/, " 小时").replace(/d$/, " 天").replace(/m$/, " 分") : label;
}

function nextReset(windows, now) {
  const known = windows.filter(window => typeof window.resetsAt === "string" && Number.isFinite(Date.parse(window.resetsAt)));
  const upcoming = known.filter(window => Date.parse(window.resetsAt) > now)
    .reduce((earliest, window) => !earliest || Date.parse(window.resetsAt) < Date.parse(earliest.resetsAt) ? window : earliest, null);
  if (!upcoming) return { label: known.length ? "重置待确认" : "", title: known.length ? "预计重置时间已过，等待查询确认；保留上次用量" : "", pending: known.length > 0 };
  const full = formatInsightDate(upcoming.resetsAt);
  return { at: upcoming.resetsAt, label: full.length === 16 ? full.slice(5) : full, title: `${insightWindowLabel(upcoming)}；预计重置：${full}`, pending: false };
}

export function UsageMeter({ window, now = Date.now(), compact = false, trailingStatus = null, extraWindows = 0 }) {
  const observation = window && typeof window === "object" ? window : {};
  const percent = insightPercent(observation.usedPercent);
  if (percent === null) return null;
  const label = insightWindowLabel(observation);
  const reset = describeInsightReset(observation, now);
  const tone = percent >= 90 ? "danger" : percent >= 75 ? "warning" : "normal";
  const resetDate = typeof observation.resetsAt === "string" && Number.isFinite(Date.parse(observation.resetsAt));
  const resetText = !resetDate ? "" : compact ? reset.text
    : reset.pending ? `预计已重置（${formatInsightDate(observation.resetsAt)}），待更新确认`
    : `预计重置：${formatInsightDate(observation.resetsAt)}`;
  const track = <progress max="100" value={Math.max(0, Math.min(100, percent))} aria-label={`${label}已用 ${percent}%`} />;
  const disclosure = [`${label}已用 ${percent}%`, resetDate ? reset.text : "", resetDate ? reset.title : ""].filter(Boolean).join("；");
  return <span className={`usage-window usage-meter is-${tone} ${compact ? "is-compact" : "is-detailed"}`} title={compact ? disclosure : undefined} aria-label={compact ? disclosure : undefined}>
    <span className="usage-window-line">
      {compact ? <span className="usage-window-heading"><span className="usage-window-label" title={label}>{compactWindowLabel(observation)}</span>{extraWindows > 0 && <span className="usage-extra-count" title={`另有 ${extraWindows} 个窗口；查看详情`}>+{extraWindows}</span>}</span> : <span className="usage-window-label" title={label}>{label}</span>}
      <span className="usage-value">{!compact && <span className="usage-value-kind">已用 </span>}<span className="usage-percent" title={percent + "%"}><span className="usage-percent-number">{percent}</span><span className="usage-percent-unit">%</span></span></span>
    </span>
    {compact ? <span className="usage-meter-context">{track}{resetDate && <span className={"usage-reset-countdown" + (reset.pending ? " is-pending" : "")} title={`${label}；${reset.text}；${reset.title}`}>{compactResetText(observation, now)}</span>}</span> : track}
    <span className={"usage-reset-row" + (compact ? " usage-hidden" : "")} aria-hidden={compact || undefined}>
      {resetDate && <span className={"usage-reset" + (reset.pending ? " is-pending" : "")} title={reset.title}>{resetText}</span>}
      {trailingStatus?.label && <span className="usage-inline-status" data-tone={trailingStatus.tone || "danger"} title={trailingStatus.title || trailingStatus.label} aria-label={trailingStatus.title || trailingStatus.label}>{trailingStatus.label}</span>}
    </span>
  </span>;
}

export function AccountInsightSummary({ snapshot, qualitySnapshot, requestObservations, onOpen, accountLabel = "", now = Date.now() }) {
  const reportedWindows = insightWindows(snapshot).filter(window => window && typeof window === "object" && !Array.isArray(window));
  const summary = describeInsightSummary(snapshot?.usage ? { ...snapshot, usage: { ...snapshot.usage, windows: reportedWindows } } : snapshot, now);
  const {badges: requestBadges} = describeAccountRequestStatuses({querySnapshot: snapshot, queryErrors: describeInsightErrors(snapshot, now), qualitySnapshot, requestObservations});
  const windows = reportedWindows.filter(window => insightPercent(window.usedPercent) !== null);
  const visible = windows.slice(0, 2);
  const hasUsage = visible.length > 0;
  const activity = describeInsightQueryActivity(snapshot, hasUsage);
  const historical = Boolean(snapshot?.usage ? insightSourceIsHistorical(snapshot, "usage") : snapshot?.bps && insightSourceIsHistorical(snapshot, "bps"));
  const awaitingReset = reportedWindows.some(window => describeInsightReset(window, now).pending);
  const limitLabel = snapshot?.usage?.limitReached === true ? historical || awaitingReset ? "上次已用尽" : "额度已用尽" : "";
  // Keep the existing quota exhaustion label ahead of a generic, code-less
  // query-attempt fallback; the current request occupies only one status slot.
  const badges = limitLabel ? requestBadges.filter(badge => badge.httpStatus || badge.hasCode || badge.sources.some(source => source !== "query")) : requestBadges;
  // List status is deliberately small. Detailed explanations stay in the dialog.
  const trailingStatus = badges[0] || (limitLabel ? {label:limitLabel, title:`Codex 额度 · ${limitLabel}`, tone:historical || awaitingReset ? "warning" : "danger"} : null);
  const exhaustedWindowIndex = !badges.length && limitLabel ? visible.findIndex(window => {
    const percent = insightPercent(window.usedPercent);
    return percent !== null && percent >= 100;
  }) : -1;
  const statusTitle = trailingStatus ? [exhaustedWindowIndex >= 0 ? insightWindowLabel(visible[exhaustedWindowIndex]) : "", trailingStatus.title].filter(Boolean).join(" · ") : "";
  const permissionLabel = snapshot?.bps?.allowed === false ? "BPS 权限受限" : snapshot?.bps?.allowed === true ? "BPS 权限可用" : "";
  const values = visible.map(window => {
    const reset = describeInsightReset(window, now);
    return [insightWindowLabel(window) + "已用 " + window.usedPercent + "%", reset.text === "重置时间未知" ? "" : reset.text, reset.text === "重置时间未知" ? "" : reset.title].filter(Boolean).join("；");
  }).join("；");
  const reset = nextReset(windows, now);
  const extraWindows = windows.length - visible.length;
  const persistenceUnconfirmed = summary.text.includes("保存未确认");
  const attemptAnnouncement = activity.active || ["partial", "failed", "skipped", "cancelled", "canceled", "interrupted"].includes(snapshot?.lastAttempt?.status) ? summary.label : "";
  const attemptState = !activity.active && ({cancelled: "已取消", canceled: "已取消", skipped: "已跳过", interrupted: "已中断"})[snapshot?.lastAttempt?.status];
  const hasStatus = activity.active || Boolean(trailingStatus) || persistenceUnconfirmed || Boolean(attemptState);
  const denseStatus = badges.length + [activity.active, !badges.length && trailingStatus, persistenceUnconfirmed, attemptState, badges.length && limitLabel].filter(Boolean).length > 1;
  const emptyLabel = activity.active ? "等待用量" : insightNeverQueried(snapshot) ? "尚未查询" : !snapshot ? "等待数据"
    : snapshot.lastAttempt?.status === "failed" ? "未取得用量" : persistenceUnconfirmed ? "记录待核实" : "暂未提供数值";
  const title = [values || permissionLabel || emptyLabel, trailingStatus?.title, limitLabel && badges.length ? limitLabel : "",
    reset.title, extraWindows > 0 ? `另有 ${extraWindows} 个窗口` : "",
    persistenceUnconfirmed ? "记录保存未确认" : "", "查看用量与状态"].filter(Boolean).join("；");

  return <button type="button" className={"insight-summary account-usage" + (historical ? " has-history" : "") + (hasUsage ? " has-usage" : " is-usage-empty") + (activity.active ? ` is-query-${activity.phase}` : "")} onClick={onOpen}
    aria-label={[accountLabel, title, attemptAnnouncement, activity.description].filter(Boolean).join("；")} aria-busy={activity.busy} title={title}>
    {visible.length ? <span className="usage-windows">{visible.map((window, index) => <UsageMeter key={`${window.name || "window"}-${index}`} window={window} now={now} compact extraWindows={index === visible.length - 1 ? extraWindows : 0} />)}</span>
      : permissionLabel ? <span className="usage-empty has-permission"><strong>{permissionLabel}</strong></span>
      : <AccountMetricState label={emptyLabel} className="usage-placeholder" trackClassName="usage-track-empty" />}

    <span className="usage-footer">
      {extraWindows > 0 && <span className="usage-meta usage-hidden" aria-hidden="true">
        <span className="usage-extra">另有 {extraWindows} 个窗口</span>
      </span>}
      <span className={"usage-footer-line" + (hasStatus ? " has-status" : "") + (denseStatus ? " is-dense" : "")}>
        {reset.label && <span className={"usage-next-reset" + (reset.pending ? " is-pending" : "")} title={reset.title}>
          {reset.at ? <time className="usage-next-reset-time" dateTime={reset.at}>{reset.label}</time> : reset.label}
        </span>}
        <span className={"usage-statuses" + (badges.some(badge => badge.hasProbe) ? " has-probe" : "") + (badges.length ? " has-request-statuses" : "") + (hasStatus ? "" : " is-idle")}>
          <UsageQuerySlot activity={activity} />
          {attemptState && <span className="usage-attempt-state" title={summary.label}>{attemptState}</span>}
          <span className="usage-status-slot">{trailingStatus && <span className={"usage-inline-status" + (trailingStatus.hasProbe ? " usage-probe-status" : "")} data-tone={trailingStatus.tone} data-window-index={exhaustedWindowIndex >= 0 ? exhaustedWindowIndex : undefined} title={statusTitle} aria-label={statusTitle}>{trailingStatus.label}</span>}</span>
          {Boolean(badges.length) && limitLabel && <span className="usage-limit-state" data-tone={historical || awaitingReset ? "warning" : "danger"} title={limitLabel} aria-label={limitLabel}>用尽</span>}
          <span className={"usage-persistence" + (persistenceUnconfirmed ? " needs-verification" : " is-idle")} title={persistenceUnconfirmed ? "记录保存未确认" : undefined} aria-hidden={!persistenceUnconfirmed}>{persistenceUnconfirmed ? "待核实" : ""}</span>
        </span>
      </span>
    </span>
  </button>;
}
