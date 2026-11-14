import React from "react";
import { normalizeUploadTarget } from "../lib/uploadTarget.js";

export function UploadTargetPicker({ value = "sub2api", onChange, disabled = false, sub2apiAvailable = true, cpaAvailable = false,
  name = "workflow-upload-target", saved = true }) {
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
          <span className="upload-target-copy"><strong>{choice.label}{!choice.available ? "（不可用）" : ""}</strong></span>
        </label>)}
      </div>
    </fieldset>
    {!saved && <p className="dialog-hint" role="status">推送目标仅本次有效，浏览器未能保存偏好。</p>}
  </div>;
}
