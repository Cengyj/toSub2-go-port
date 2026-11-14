import React from "react";
import { Ban, CheckCircle2, CircleAlert, KeyRound, ListPlus, LoaderCircle, LogIn, Mail, RefreshCw, RotateCcw, ShieldCheck, Smartphone } from "lucide-react";
import { authorizationMethodLabel } from "../lib/authorizationMethod.js";
import { describeJob } from "../lib/jobPresentation.js";
import "./account-indicators.css";

const icons = { Ban, CheckCircle2, CircleAlert, KeyRound, ListPlus, LoaderCircle, LogIn, Mail, RefreshCw, RotateCcw, ShieldCheck, Smartphone };
const compactStatusLabels = {
  "本次已完成": "已完成", "本次操作失败": "操作失败",
  "本次已中断": "已中断", "本次已取消": "已取消",
  "授权已保存 · 自动回传暂停": "授权已存 · 回传暂停",
};

export function operationExecutionHint(presentation) {
  // Only remove an exact repeated terminal prefix. Workflow steps, input
  // requests, errors and historical authorization guidance remain intact.
  if (!presentation.terminal) return presentation.summary;
  for (const ending of ["已完成，", "未完成，", "已取消，", "已中断，"]) {
    const prefix = presentation.operation.label + ending;
    if (presentation.summary.startsWith(prefix) && presentation.summary.length > prefix.length) return presentation.summary.slice(prefix.length);
  }
  return presentation.summary;
}

// Reuse the final presentation verdict: account capability, latest operation
// and downstream synchronization are deliberately not equivalent states.
export function AccountOperationSummary({ job, presentation = describeJob(job), expanded = false, onToggleLogs }) {
  const { operation, status, summary } = presentation;
  const Icon = icons[status.icon] || CircleAlert;
  const method = authorizationMethodLabel(job.authorizationMethod);
  const explanation = `${operation.label} · ${status.label}。${summary}。${expanded ? "收起" : "查看"}执行日志`;
  return <button type="button" className={`operation-summary operation-status tone-${status.tone}`} data-status={status.code} onClick={onToggleLogs} aria-expanded={expanded} aria-label={`${explanation} ${job.email}`} title={explanation}>
    <span className="operation-summary-heading">
      <strong className="operation-name" title={operation.label}>{operation.label}</strong>
      <span className="operation-method" title={`此账号保存的授权方式：${method}`} aria-label={`授权方式：${method}`}>{method}</span>
    </span>
    <span className="operation-summary-meta">
    <span className="account-pill operation-status-pill" data-tone={status.tone}>
      <Icon size={11} className={status.spinning ? "spin" : ""} aria-hidden="true" />
      <span>{compactStatusLabels[status.label] || status.label}</span>
    </span>
    </span>
  </button>;
}
