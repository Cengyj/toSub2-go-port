import React, { useEffect, useRef, useState } from "react";
import { Ban, Check, CircleAlert, Clock3, LoaderCircle, RefreshCw, Settings2, X } from "lucide-react";
import { apiFetch } from "../lib/console.js";
import { createPollingTask } from "../lib/polling.js";
import { describeInsightSummary, describeInsightSubmission, describeInsightErrors, formatInsightDate, insightSnapshotForJob } from "../lib/accountInsights.js";
import { InsightErrors, InsightDataSections } from "./account-insight-details.jsx";
import { DialogFrame } from "./ui.jsx";
import "./account-insights.css";

export { AccountInsightSummary } from "./account-usage.jsx";
export { InsightWindow } from "./account-insight-details.jsx";

export function AccountInsightsSettings({ insights }) {
  const { draft, setDraft, settingsLoading, saving, dirty, settingsError, persistenceWarning, uncertain, verificationRequired } = insights;
  const [saved, setSaved] = useState(false);
  const blocked = !insights.settingsLoaded || settingsLoading || saving || verificationRequired;
  const change = (key, value) => { setSaved(false); setDraft(current => ({ ...current, [key]: value })); };
  async function save(event) { event.preventDefault(); setSaved(await insights.saveSettings(draft)); }
  return <form className="section-card settings-form insight-settings" onSubmit={save} aria-label="账号查询配置">
    <div className="card-header"><h2>账号用量与状态查询</h2><p>由本地 Go 服务读取上游信息。查询不会登录、刷新授权、生成模型内容或上传账号。</p></div>
    <div className="card-body insight-settings-fields">
      {settingsLoading && <p role="status"><LoaderCircle size={14} className="spin" />正在读取服务端配置…</p>}
      <label className="insight-check"><input type="checkbox" checked={draft.enabled} disabled={blocked} onChange={event => change("enabled", event.target.checked)} /><span><strong>启用账号查询</strong><small>关闭后不再发起手动或自动查询；已有缓存仍可查看。</small></span></label>
      <label className="settings-field"><span>更新方式</span><select value={draft.automatic ? "automatic" : "manual"} disabled={blocked} onChange={event => change("automatic", event.target.value === "automatic")}><option value="automatic">自动更新，也允许手动查询</option><option value="manual">仅手动查询</option></select></label>
      <label className="settings-field"><span>自动更新间隔（分钟）</span><input type="number" min="1" max="1440" step="1" value={draft.intervalMinutes} disabled={blocked || !draft.automatic} onChange={event => change("intervalMinutes", event.target.value)} /><small>1–1440 分钟。控制服务端上游查询频率，不是页面刷新频率。</small></label>
      <label className="insight-check"><input type="checkbox" checked={draft.includeResetCredits} disabled={blocked} onChange={event => change("includeResetCredits", event.target.checked)} /><span><strong>包含额度重置券明细</strong><small>只查询券的数量与到期时间，不消费重置券。</small></span></label>
      <p className="dialog-hint">当前排除 {draft.excludedAccountIds.length} 个账号的自动查询，可在“用量与状态”详情中调整。排除不禁止手动查询，手动查询仍受总开关与安全门禁约束。</p>
      <p className="dialog-hint">配置保存在服务端；刷新、关闭网页或换浏览器仍保持。关闭总开关或停止服务可停止采集；清除网页设置不会关闭本功能。</p>
      {settingsError && <p className="dialog-error" role="alert"><CircleAlert size={14} />{settingsError}</p>}
      {persistenceWarning && <p className="insight-warning" role="status">{persistenceWarning}</p>}
      {uncertain && <p className="insight-warning" role="status">上次配置保存的持久性尚未确认。重新读取仅核对服务当前记录；确认后请明确重新保存。</p>}
      {(settingsError || verificationRequired || uncertain) && <button type="button" className="secondary-button" disabled={saving || settingsLoading} onClick={() => { setSaved(false); void insights.reloadSettings(); }}><RefreshCw size={14} />重新读取配置（放弃草稿）</button>}
      {saved && !dirty && !uncertain && !verificationRequired && !settingsError && !persistenceWarning && <p className="provider-ready" role="status"><Check size={14} />账号查询配置已保存</p>}
    </div>
    <div className="dialog-footer"><button type="button" className="cancel-button" disabled={saving || !dirty} onClick={() => { insights.resetDraft(); setSaved(false); }}>放弃更改</button><button type="submit" className="primary-button" disabled={blocked || (!dirty && !uncertain)}>{saving ? <LoaderCircle size={15} className="spin" /> : <Check size={15} />}保存查询配置</button></div>
  </form>;
}

export function AccountInsightsDialog({ job, token, initialSnapshot, insights, onClose }) {
  const [snapshotData, setSnapshot] = useState(initialSnapshot || null);
  const snapshot = insightSnapshotForJob(job, snapshotData);
  const [loading, setLoading] = useState(true);
  const [readError, setReadError] = useState("");
  const [actionError, setActionError] = useState("");
  const [notice, setNotice] = useState("");
  const [now, setNow] = useState(Date.now);
  const mounted = useRef(false);
  useEffect(() => { mounted.current = true; return () => { mounted.current = false; }; }, []);
  useEffect(() => {
    setLoading(true); setReadError("");
    const task = createPollingTask({
      intervalMs: 5000,
      request: signal => apiFetch(token, `/api/account-insights/${encodeURIComponent(job.id)}`, { signal, cache: "no-store" }),
      onData: data => { if (!insightSnapshotForJob(job, data)) { setReadError("查询详情身份不匹配，请重新打开后核实。"); setLoading(false); return; } setSnapshot(data); setReadError(""); setLoading(false); setNow(Date.now()); },
      onError: error => { setReadError(error.message); setLoading(false); setNow(Date.now()); },
    });
    return task.stop;
  }, [job.id, job.authorizationMethod, token, insights.revision]);
  const summary = describeInsightSummary(snapshot, now);
  const attempt = snapshot?.lastAttempt || {};
  const running = ["queued", "pending", "running", "checking"].includes(attempt.status);
  const canQuery = insights.settings.enabled && insights.settingsLoaded && !insights.settingsLoading && !insights.verificationRequired && snapshot?.canQuery !== false;
  const excluded = insights.settings.excludedAccountIds.includes(job.id);
  const errors = describeInsightErrors(snapshot, now);
  async function refresh() {
    setActionError(""); setNotice("");
    try { const data = await insights.submit([job.id]); if (mounted.current && data) setNotice(describeInsightSubmission(data).text); }
    catch (error) { if (mounted.current) setActionError(error.message); }
  }
  async function cancel() {
    setActionError("");
    try { const data = await insights.cancel(job.id); if (mounted.current && data) setNotice(data.cancelled ? "查询取消请求已确认，已保存的历史结果保留。" : "当前没有可取消的查询，请核对最新状态。"); }
    catch (error) { if (mounted.current) setActionError(error.message); }
  }
  async function automatic(enabled) {
    setNotice(""); setActionError("");
    if (await insights.setAutomatic(job.id, enabled) && mounted.current) setNotice(enabled ? "已允许此账号参加自动查询。" : "已排除此账号的自动查询；仍可手动查询。");
  }
  return <DialogFrame onClose={onClose}>
    <section className="batch-dialog insight-dialog" role="dialog" aria-modal="true" aria-labelledby="account-insight-title">
      <div className="dialog-header"><div><div className="insight-title-line"><h2 id="account-insight-title">用量与状态</h2><span className="insight-method">{job.authorizationMethod === "bps" ? "BPS" : "ChatGPT"}</span></div><p className="insight-email">{job.email}</p></div><button type="button" className="icon-button" aria-label="关闭用量与状态" onClick={onClose}><X size={18} /></button></div>
      <div className={"insight-result is-" + summary.tone} role="status"><strong>{summary.label}</strong><span>{summary.text}</span></div>
      <div className="insight-controls" aria-label="账号查询操作">
        <div className="insight-control-actions"><button type="button" className="primary-button" disabled={!canQuery || insights.actionBusy || running} onClick={refresh}>{insights.actionBusy || running ? <LoaderCircle size={15} className="spin" /> : <RefreshCw size={15} />}立即查询</button>{running && <button type="button" className="secondary-button" disabled={insights.actionBusy} onClick={cancel}><Ban size={15} />取消查询</button>}</div>
        <label className="insight-check insight-auto-account"><input type="checkbox" checked={!excluded} disabled={insights.saving || !insights.settingsLoaded || insights.settingsLoading || insights.verificationRequired} onChange={event => void automatic(event.target.checked)} /><span>此账号参加自动查询<small>总开关开启且更新方式为自动时执行</small></span></label>
      </div>
      <p className="insight-control-hint">{!insights.settings.enabled ? "总开关已关闭，仍可查看历史结果。" : snapshot?.unavailableReason || "查询不会重新登录、刷新授权或上传。"}</p>
      {loading && !snapshot && <p className="insight-loading" role="status"><LoaderCircle size={18} className="spin" />正在读取缓存详情…</p>}
      {(readError || actionError || insights.settingsError || insights.persistenceWarning || insights.uncertain || insights.verificationRequired || notice || (attempt.message && !errors.some(error => error.message === attempt.message))) && <div className="insight-feedback">
        {readError && <p className="dialog-error" role="alert"><CircleAlert size={14} /><span><span className="insight-feedback-label">详情读取：</span>{readError} 保留最近一次已读取的数据。</span></p>}
        {actionError && <p className="dialog-error" role="alert"><CircleAlert size={14} /><span><span className="insight-feedback-label">查询操作：</span>{actionError}</span></p>}
        {insights.settingsError && <p className="dialog-error" role="alert"><CircleAlert size={14} /><span><span className="insight-feedback-label">查询配置：</span>{insights.settingsError}</span></p>}
        {insights.persistenceWarning && <p className="insight-warning" role="status">{insights.persistenceWarning}</p>}
        {insights.uncertain ? <p className="insight-warning" role="status">查询配置保存的持久性尚未确认，请在服务配置页核对并重新保存。</p> : insights.verificationRequired && <p className="insight-warning" role="status">查询配置尚待重新核对，请在服务配置页重新读取。</p>}
        {notice && <p className="insight-notice" role="status">{notice}</p>}
        {attempt.message && !errors.some(error => error.message === attempt.message) && <p className="insight-warning" role="status">最新查询：{attempt.message}</p>}
      </div>}
      <InsightErrors errors={errors} />
      <InsightDataSections job={job} snapshot={snapshot} now={now} />
      <div className="insight-dialog-footer"><p className="dialog-hint"><span>关闭窗口仅停止页面读取，已受理的后台查询继续执行。</span><span>最新尝试时间：{formatInsightDate(attempt.finishedAt || attempt.startedAt)}。本页不读取密码或 2FA 密钥。</span></p><button type="button" className="secondary-button" onClick={onClose}>关闭</button></div>
    </section>
  </DialogFrame>;
}

export function AccountInsightsMonitor({ insights, onConfigure }) {
  const { status: latestStatus, statusError } = insights;
  const status = latestStatus || {};
  const counts = status.counts || {};
  const activityLabel = !latestStatus ? statusError ? "查询状态暂不可读" : "正在读取查询状态…"
    : Number(status.active) > 0 ? "正在查询" : Number(status.queued) > 0 ? "查询已排队"
    : status.enabled ? status.automatic ? "自动查询已开启" : "仅手动查询" : "账号查询已关闭";
  return <section className="section-card insight-monitor"><div className="card-header"><div><h2>账号查询概览</h2><p>只读信息采集，与 Sub2 自动修复巡检分别统计。</p></div><button type="button" className="secondary-button" onClick={onConfigure}><Settings2 size={14} />查询配置</button></div><div className="card-body">
    <div className="insight-monitor-state"><strong>{activityLabel}</strong><span><Clock3 size={14} />下次检查：{!latestStatus ? "未知" : status.enabled && status.automatic ? formatInsightDate(status.nextCheckAt) : "不安排自动查询"}</span><span>排队 {latestStatus ? Number(status.queued || 0) : "—"} · 执行中 {latestStatus ? Number(status.active || 0) : "—"}</span></div>
    {(statusError || status.lastError || status.persistenceWarning) && <p className="insight-warning" role="status">{statusError || status.lastError || status.persistenceWarning}；保留已保存的历史结果。</p>}
    <div className="insight-counts" aria-label="最近查询批次统计">{[["success", "已完成"], ["partial", "部分更新"], ["failed", "查询失败"], ["skipped", "已跳过"], ["cancelled", "已取消"]].map(([key, label]) => <div key={key}><strong>{latestStatus ? Number(counts[key] || 0) : "—"}</strong><span>{label}</span></div>)}</div>
  </div></section>;
}
