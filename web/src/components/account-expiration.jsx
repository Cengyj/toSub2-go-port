import React, { useEffect, useRef, useState } from "react";
import { Check, CircleAlert, LoaderCircle, RefreshCw } from "lucide-react";
import { apiFetch } from "../lib/console.js";
import { browserTimeZone, describeExpiration, expirationInputValue } from "../lib/accountExpiration.js";

export function AccountExpiration({ job, onEdit, now }) {
  const expiration = describeExpiration(job, now);
  const timeZone = browserTimeZone();
  const title = expiration.dateTime ? `${expiration.label}${timeZone ? ` · ${timeZone}` : ""} · ${expiration.sourceLabel}到期时间` : "未填写手动时间，也未取得真实套餐到期时间";
  const content = <>
    {expiration.dateTime ? <time dateTime={expiration.dateTime}>{expiration.label}</time> : <span className="expiration-unknown">{expiration.label}</span>}
    <span className="expiration-detail"><span className="expiration-source">{expiration.sourceLabel}</span>{expiration.remaining && <span>{expiration.remaining}</span>}</span>
  </>;
  return onEdit
    ? <button type="button" className={`account-expiration is-${expiration.tone} is-editable`} title={`${title}；点击编辑`} aria-label={`编辑 ${job.email} 的到期时间`} onClick={onEdit}>{content}</button>
    : <div className={`account-expiration is-${expiration.tone}`} title={title}>{content}</div>;
}

export function AccountExpirationEditor({ job, token, onSaved, onBusyChange, onDirtyChange, onUncertainChange, onRefresh, refreshing = false, allowUnchangedSave = false }) {
  const initial = expirationInputValue(job.manualExpiresAt);
  const [draft, setDraft] = useState(initial);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  const [needsRefresh, setNeedsRefresh] = useState(false);
  const [now, setNow] = useState(Date.now);
  const pending = useRef(null);
  const mounted = useRef(false);
  const dirty = draft !== initial;
  const timeZone = browserTimeZone();
  useEffect(() => { setDraft(initial); }, [job.id, initial]);
  useEffect(() => { onDirtyChange?.(dirty); }, [dirty, onDirtyChange]);
  useEffect(() => {
    mounted.current = true;
    const timer = setInterval(() => setNow(Date.now()), 60000);
    return () => { mounted.current = false; clearInterval(timer); pending.current?.abort(); };
  }, []);

  async function save(value) {
    if (pending.current || needsRefresh || refreshing) return;
    if (value && !timeZone) { setError("无法识别当前浏览器时区，请刷新页面后重试。"); return; }
    const controller = new AbortController();
    pending.current = controller;
    setBusy(true);
    onBusyChange?.(true);
    setError("");
    setNotice("");
    const timeout = setTimeout(() => controller.abort(), 10000);
    try {
      const data = await apiFetch(token, `/api/jobs/${encodeURIComponent(job.id)}/expiration`, {method: "POST", signal: controller.signal, body: JSON.stringify({expiresAt: value, timeZone})});
      if (!mounted.current) return;
      if (!data.job || data.job.id !== job.id) throw new Error("保存结果未能确认，请刷新账号信息核实。");
      onSaved(data.job);
      onUncertainChange?.(false);
      setDraft(expirationInputValue(data.job.manualExpiresAt));
      setNotice(value ? "到期时间已保存。" : "已清除手动时间，将使用套餐到期时间；没有套餐时间时显示未知。");
    } catch (requestError) {
      if (!mounted.current) return;
      const unknown = !requestError.status || requestError.status >= 500 || requestError.outcome === "unknown" || requestError.data?.uncertain === true;
      setNeedsRefresh(unknown);
      if (unknown) onUncertainChange?.(true);
      setError(unknown ? "保存结果尚未确认，请先刷新账号信息核实，再决定是否重新保存。" : requestError.message);
    } finally {
      clearTimeout(timeout);
      pending.current = null;
      if (mounted.current) { setBusy(false); onBusyChange?.(false); }
    }
  }

  return <form className="account-expiration-editor" aria-label="账号到期时间" onSubmit={(event) => { event.preventDefault(); if (dirty || allowUnchangedSave) void save(draft); }}>
    <div className="expiration-editor-heading"><strong>到期时间</strong><AccountExpiration job={job} now={now} /></div>
    <label htmlFor="account-manual-expiration">手动到期时间</label>
    <input id="account-manual-expiration" type="datetime-local" step="60" value={draft} onChange={(event) => { setDraft(event.target.value); setNotice(""); }} disabled={busy || needsRefresh || refreshing} aria-describedby="account-expiration-help" />
    <p id="account-expiration-help">按浏览器时区{timeZone ? ` ${timeZone}` : "（暂未识别）"}显示和保存。手动值优先，清除后回退真实套餐时间；不会使用 Token 到期时间，也不会自动停用或删除账号。</p>
    {error && <p className="dialog-error" role="alert"><CircleAlert size={14} />{error}</p>}
    {needsRefresh && onRefresh && <button type="button" className="secondary-button expiration-refresh" disabled={busy || refreshing} onClick={onRefresh}><RefreshCw size={14} className={refreshing ? "spin" : ""} />重新读取到期时间</button>}
    {notice && <p className="expiration-save-notice" role="status"><Check size={14} />{notice}</p>}
    <div className="expiration-editor-actions">
      <button type="button" className="text-action" disabled={busy || needsRefresh || refreshing || !initial} onClick={() => void save("")}>清除手动时间</button>
      <button type="button" className="secondary-button" disabled={busy || needsRefresh || refreshing || !dirty} onClick={() => { setDraft(initial); setError(""); setNotice(""); }}>放弃更改</button>
      <button type="submit" className="primary-button" disabled={busy || needsRefresh || refreshing || (!dirty && !allowUnchangedSave)}>{busy ? <LoaderCircle size={14} className="spin" /> : <Check size={14} />}保存到期时间</button>
    </div>
  </form>;
}
