import React, { useEffect, useRef, useState } from "react";
import { Check, CircleAlert, Copy, KeyRound, LoaderCircle, Mail, RefreshCw, ShieldCheck, UserRound, X } from "lucide-react";
import { apiFetch } from "../lib/console.js";
import { PlanBadge } from "./PlanBadge.jsx";
import { DialogFrame } from "./ui.jsx";
import "./account-actions.css";

function VisibilityIcon({ hidden }) {
  return <svg width={14} height={14} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth={2} strokeLinecap="round" strokeLinejoin="round" aria-hidden="true"><path d="M2 12s3-7 10-7 10 7 10 7-3 7-10 7-10-7-10-7Z" /><circle cx="12" cy="12" r="3" />{hidden && <path d="m3 3 18 18" />}</svg>;
}

function AccountInfoField({ label, value, icon: Icon, secret = false, multiline = false, onCopyError }) {
  const [visible, setVisible] = useState(false);
  const [copied, setCopied] = useState(false);
  const copyTimer = useRef(null);
  const copyGeneration = useRef(0);
  useEffect(() => {
    copyGeneration.current += 1;
    setVisible(false);
    setCopied(false);
    return () => {
      copyGeneration.current += 1;
      clearTimeout(copyTimer.current);
    };
  }, [value]);
  const text = typeof value === "string" ? value : "";
  async function copy() {
    const generation = copyGeneration.current;
    try {
      await navigator.clipboard.writeText(text);
      if (generation !== copyGeneration.current) return;
      setCopied(true);
      clearTimeout(copyTimer.current);
      copyTimer.current = setTimeout(() => setCopied(false), 1800);
      onCopyError("");
    } catch {
      if (generation !== copyGeneration.current) return;
      onCopyError(`无法复制${label}，可显示内容后手动选择。`);
    }
  }
  return <div className={`account-info-field ${multiline ? "is-multiline" : ""}`}>
    <div className="account-info-field-heading"><span>{Icon && <Icon size={13} aria-hidden="true" />}{label}</span><div>
      {text && secret && <button type="button" className="icon-button" aria-label={`${visible ? "隐藏" : "显示"}${label}`} title={`${visible ? "隐藏" : "显示"}${label}`} onClick={() => setVisible(!visible)}><VisibilityIcon hidden={visible} /></button>}
      {text && <button type="button" className={`icon-button ${copied ? "copied" : ""}`} aria-label={`复制${label}`} title={copied ? "已复制" : `复制${label}`} onClick={copy}>{copied ? <Check size={14} /> : <Copy size={14} />}</button>}
    </div></div>
    <code className={!text ? "empty" : secret && !visible ? "is-masked" : ""}>{!text ? "未保存" : secret && !visible ? "••••••••••••" : text}</code>
    {copied && <span className="sr-only" role="status">{label}已复制</span>}
  </div>;
}

export function AccountInfoDialog({ job, token, onClose }) {
  const [info, setInfo] = useState(null);
  const [error, setError] = useState("");
  const [copyError, setCopyError] = useState("");
  const [loading, setLoading] = useState(true);
  const [revision, setRevision] = useState(0);
  const request = useRef(null);
  const closed = useRef(false);
  useEffect(() => {
    let active = true;
    const controller = new AbortController();
    request.current = controller;
    const timeout = setTimeout(() => controller.abort(), 10000);
    setInfo(null);
    setError("");
    setCopyError("");
    setLoading(true);
    apiFetch(token, `/api/jobs/${encodeURIComponent(job.id)}/account-info`, { signal: controller.signal, cache: "no-store" })
      .then((data) => { if (active && !closed.current) setInfo(data); })
      .catch((requestError) => { if (active && !closed.current) setError(requestError.name === "AbortError" ? "读取账号信息超时，请重试。" : requestError.message); })
      .finally(() => { clearTimeout(timeout); if (active && !closed.current) setLoading(false); });
    return () => { active = false; clearTimeout(timeout); controller.abort(); };
  }, [job.id, token, revision]);

  function close() {
    closed.current = true;
    request.current?.abort();
    setInfo(null);
    setError("");
    setCopyError("");
    onClose();
  }
  const mailHeaders = info && info.mailRequestHeaders && Object.keys(info.mailRequestHeaders).length ? JSON.stringify(info.mailRequestHeaders, null, 2) : "";
  const hasMailSettings = Boolean(info?.mailApiUrl || info?.mailRequestBody || mailHeaders);
  return <DialogFrame onClose={close}>
    <section className="batch-dialog account-info-dialog" role="dialog" aria-modal="true" aria-labelledby="account-info-title">
      <div className="dialog-header">
        <div className="account-dialog-heading"><span className="account-dialog-icon" aria-hidden="true"><UserRound size={21} /></span><div><h2 id="account-info-title">账号信息</h2><p>{job.email}</p></div></div>
        <button type="button" className="icon-button" onClick={close} aria-label="关闭账号信息"><X size={18} /></button>
      </div>
      <div className="account-info-meta"><PlanBadge value={job.planType} /><span><ShieldCheck size={13} aria-hidden="true" />当前保存的资料</span><button type="button" className="account-info-refresh" onClick={() => setRevision((n) => n + 1)} disabled={loading}><RefreshCw size={13} className={loading ? "spin" : ""} />刷新</button></div>
      {loading ? <div className="account-info-loading" role="status"><LoaderCircle size={21} className="spin" />正在读取账号信息…</div> : error ? <div className="account-info-error" role="alert"><CircleAlert size={18} /><p>{error}</p><button type="button" className="secondary-button" onClick={() => setRevision((n) => n + 1)}>重新读取</button></div> : info && <>
        <div className="account-info-grid">
          <AccountInfoField label="邮箱" icon={Mail} value={info.email} onCopyError={setCopyError} />
          <AccountInfoField label="密码" icon={KeyRound} value={info.password} secret onCopyError={setCopyError} />
          <AccountInfoField label="2FA 密钥" icon={ShieldCheck} value={info.totpSecret} secret onCopyError={setCopyError} />
          {hasMailSettings && <AccountInfoField label="邮箱收码 API" icon={Mail} value={info.mailApiUrl} secret onCopyError={setCopyError} />}
        </div>
        {(info.proxyUrl || hasMailSettings) && <details className="account-info-extra"><summary>代理与收码配置</summary><div className="account-info-extra-fields">
          {info.proxyUrl && <AccountInfoField label="账号代理" value={info.proxyUrl} secret onCopyError={setCopyError} />}
          {hasMailSettings && <AccountInfoField label="收码请求方法" value={info.mailRequestMethod || "GET"} onCopyError={setCopyError} />}
          {info.mailRequestBody && <AccountInfoField label="收码请求正文" value={info.mailRequestBody} secret multiline onCopyError={setCopyError} />}
          {mailHeaders && <AccountInfoField label="收码请求头" value={mailHeaders} secret multiline onCopyError={setCopyError} />}
        </div></details>}
        <p className="account-info-note"><ShieldCheck size={14} aria-hidden="true" /><span>2FA 重置成功后，此处与「导出原始信息」均使用新密钥。密码与密钥默认隐藏，关闭窗口后清空本次读取内容。</span></p>
      </>}
      {copyError && <p className="dialog-error" role="alert"><CircleAlert size={16} />{copyError}</p>}
      <div className="dialog-actions"><button type="button" className="secondary-button" onClick={close}>关闭</button></div>
    </section>
  </DialogFrame>;
}
