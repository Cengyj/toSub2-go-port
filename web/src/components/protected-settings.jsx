import React from "react";
import { CircleAlert, LoaderCircle, RefreshCw, ShieldCheck } from "lucide-react";

export function ProtectedSettingsNotice({ state, onApply, applyPending = false, applied = true }) {
  if (!state?.enabled) return null;
  const busy = state.loading || state.saving || applyPending;
  return <section className="protected-settings-notice" aria-label="配置安全保存状态">
    <p className="dialog-hint"><ShieldCheck size={14} aria-hidden="true" />此配置由本机服务加密保存，所有浏览器共用。仅在安全保存并读回确认后清除本浏览器的旧秘密。</p>
    {state.loading && <p role="status"><LoaderCircle size={14} className="spin" />正在读取安全配置…</p>}
    {state.migrationRequired && <p className="dialog-hint" role="status">当前浏览器仍有旧配置。请核对下方草稿后点击保存，将其迁移到服务端；若服务端已有另一份配置，可明确采用服务端配置。读取不会自动覆盖任一份配置。</p>}
    {state.error && <p className="dialog-error" role="alert"><CircleAlert size={14} />{state.error}</p>}
    {state.notice && <p className="provider-ready" role="status">{state.notice}</p>}
    {state.loaded && !state.snapshot?.config && onApply && !applied && <p className="dialog-hint" role="status">服务端尚未保存此配置，默认值也尚未应用到当前进程。请核对下方草稿并保存，安全保存与进程应用分别确认。</p>}
    {state.loaded && state.snapshot?.config && !applied && <p className="dialog-hint" role="status">已读取服务端保护配置；当前进程应用尚未确认。应用结果与保存结果分别展示。</p>}
    <div className="dialog-actions">
      {(!state.loaded || state.error) && <button type="button" className="secondary-button" disabled={busy} onClick={state.reload}><RefreshCw size={14} />读取安全配置</button>}
      {state.uncertain && <button type="button" className="secondary-button" disabled={busy} onClick={state.confirm}>核实安全保存结果</button>}
      {(state.migrationRequired || state.conflict) && state.snapshot?.config && <button type="button" className="secondary-button" disabled={busy} onClick={state.adoptServer}>采用服务端配置并清理浏览器副本</button>}
      {state.loaded && state.snapshot?.config && !applied && onApply && <button type="button" className="secondary-button" disabled={busy || !state.ready} onClick={onApply}>应用已保存配置</button>}
    </div>
  </section>;
}
