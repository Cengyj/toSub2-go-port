import React, { useId } from "react";
import { Check, Layers3, LogIn } from "lucide-react";

export function normalizeAuthorizationMethod(value) {
  return value === "bps" ? "bps" : "chatgpt";
}

export function authorizationMethodLabel(value) {
  return normalizeAuthorizationMethod(value) === "bps" ? "BPS" : "ChatGPT";
}

export function AuthorizationMethodPicker({ value, onChange, disabled = false, locked = false }) {
  const name = useId();
  const current = normalizeAuthorizationMethod(value);
  return <fieldset className="authorization-method-picker" disabled={disabled || locked}>
    <legend>登录授权方式</legend>
    <div className="authorization-method-options">
      {[
        { value: "chatgpt", title: "ChatGPT", description: "原有登录与授权流程", icon: LogIn },
        { value: "bps", title: "BPS", description: "Basispoints 登录授权", icon: Layers3 },
      ].map(({ value: method, title, description, icon: Icon }) => <label key={method} className={`authorization-method-card ${current === method ? "is-selected" : ""}`}>
        <input type="radio" name={name} value={method} checked={current === method} onChange={() => onChange(method)} />
        <span className="authorization-method-icon" aria-hidden="true"><Icon size={18} /></span>
        <span className="authorization-method-copy"><strong>{title}</strong><small>{description}</small></span>
        <span className="authorization-method-check" aria-hidden="true">{current === method && <Check size={13} />}</span>
      </label>)}
    </div>
    <p>{locked ? "继续已有流程将沿用当前授权方式；切换方式请使用重登授权。" : "本次选择会保存到账号，重试与自动重登沿用此方式。"}</p>
  </fieldset>;
}

export function AuthorizationMethodBadge({ value }) {
  return <small className={`authorization-method-badge ${normalizeAuthorizationMethod(value)}`} title="当前账号的登录授权方式">{authorizationMethodLabel(value)}</small>;
}
