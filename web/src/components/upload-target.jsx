import React from "react";
import { Send, X } from "lucide-react";
import { DialogFrame } from "./ui.jsx";
import { normalizeUploadTarget, uploadTargetLabel } from "../lib/uploadTarget.js";

export function UploadTargetPicker({ value = "sub2api", onChange, disabled = false, sub2apiAvailable = true, cpaAvailable = false,
  name = "workflow-upload-target", saved = true, descriptions = {} }) {
  const selected = normalizeUploadTarget(value);
  const choices = [
    { value: "sub2api", label: "Sub2API", available: sub2apiAvailable },
    { value: "cpa", label: "CPA", available: cpaAvailable },
  ];
  return <div className="upload-target-picker">
    <fieldset className="upload-target-fieldset" disabled={disabled}>
      <legend>推送目标 <span>二选一</span></legend>
      <div className="upload-target-options">
        {choices.map(choice => <label key={choice.value} className={`upload-target-choice${selected === choice.value ? " is-selected" : ""}${!choice.available || disabled ? " is-disabled" : ""}`}>
          <input type="radio" name={name} value={choice.value} checked={selected === choice.value} disabled={disabled || !choice.available} onChange={event => { if (event.target.checked) onChange(choice.value); }} />
          <span className="upload-target-copy"><strong>{choice.label}{!choice.available ? "（不可用）" : ""}</strong>{descriptions[choice.value] && <span>{descriptions[choice.value]}</span>}</span>
        </label>)}
      </div>
    </fieldset>
    {!saved && <p className="dialog-hint" role="status">推送目标仅本次有效，浏览器未能保存偏好。</p>}
  </div>;
}

export function ManualUploadDialog({ action, targets, problem, onTargetChange, onSubmit, onClose, onConfigure, onReloadCPA, cpaLoading }) {
  const targetName = uploadTargetLabel(action.target);
  return <DialogFrame onClose={onClose}>
    <section className="settings-dialog totp-action-dialog" role="dialog" aria-modal="true" aria-labelledby="manual-upload-title" aria-describedby="manual-upload-summary">
      <div className="dialog-header"><div><p className="eyebrow">上传授权</p><h2 id="manual-upload-title">选择上传目标</h2></div><button type="button" className="icon-button" aria-label="关闭上传目标选择" onClick={onClose}><X size={18} /></button></div>
      <form onSubmit={event => { event.preventDefault(); if (!problem) onSubmit(); }}>
        <p id="manual-upload-summary" className="dialog-hint">本次选择 {action.ids.length} 个账号，仅上传到下方选中的服务。确认前不会发送上传请求。</p>
        <UploadTargetPicker value={action.target} onChange={onTargetChange} name="manual-upload-target" sub2apiAvailable={targets.sub2api.available} cpaAvailable={targets.cpa.available} descriptions={{ sub2api: targets.sub2api.status, cpa: targets.cpa.status }} />
        <p className="dialog-hint">{action.target === "cpa" ? "CPA：同名授权文件将替换，使用当前配置的优先级和权重。" : "Sub2API：已有账号更新授权并保留原有号池配置。"}</p>
        {problem && <p className="dialog-error" role="alert">{problem}</p>}
        {!targets[action.target].ready && <div>
          {action.target === "cpa" && <button type="button" className="secondary-button" disabled={cpaLoading} onClick={onReloadCPA}>{cpaLoading ? "正在读取 CPA 配置…" : "读取 CPA 配置"}</button>}
          <button type="button" className="totp-configure-link" onClick={() => onConfigure(action.target)}>配置 {targetName}</button>
        </div>}
        <div className="dialog-actions"><button type="button" className="secondary-button" onClick={onClose}>取消</button><button type="submit" className="primary-button" disabled={Boolean(problem)}><Send size={16} />上传到 {targetName}</button></div>
      </form>
    </section>
  </DialogFrame>;
}
