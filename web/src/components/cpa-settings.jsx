import React from "react";
import { Check, CircleAlert, LoaderCircle, RefreshCw } from "lucide-react";

export function CPASettings({ cpa }) {
  const busy = cpa.saving || cpa.loading;
  return <div className="settings-editor">
    <form className="batch-dialog cpa-settings" onSubmit={cpa.save} aria-labelledby="cpa-settings-title">
      <div className="dialog-header">
        <div><h2 id="cpa-settings-title">CPA 配置</h2><span>推送至 CLIProxyAPI，支持 ChatGPT / Codex 授权，不支持 BPS。</span></div>
      </div>
      <section className="provider-config-section" aria-labelledby="cpa-connection-title">
        <h3 className="cpa-section-title" id="cpa-connection-title">服务连接</h3>
        <div className="provider-config-grid">
          <label className="settings-field wide-settings-field">
            <span>管理地址</span>
            <input type="text" value={cpa.draft.baseUrl} disabled={busy || !cpa.loaded} onChange={event => cpa.setDraft(current => ({ ...current, baseUrl: event.target.value }))} placeholder="http://127.0.0.1:8317" autoComplete="off" spellCheck="false" />
            <small className="settings-field-help">填写 CPA 服务地址；清空地址并保存可移除配置。</small>
          </label>
          <label className="settings-field wide-settings-field">
            <span>管理密钥</span>
            <input type="password" value={cpa.draft.adminApiKey} disabled={busy || !cpa.loaded} onChange={event => cpa.setDraft(current => ({ ...current, adminApiKey: event.target.value }))} placeholder={cpa.settings?.hasApiKey ? "已保存；同地址留空可沿用" : "填写 CPA 管理密钥"} autoComplete="new-password" />
            <small className="settings-field-help">使用管理接口密钥。同地址留空沿用，更换地址须重新填写；保存后输入框清空。</small>
          </label>
        </div>
      </section>
      <section className="provider-config-section" aria-labelledby="cpa-scheduling-title">
        <h3 className="cpa-section-title" id="cpa-scheduling-title">推送参数</h3>
        <div className="provider-config-grid cpa-scheduling-grid">
          <label className="settings-field">
            <span>优先级</span>
            <input type="number" name="cpa-priority" min="-2147483648" max="2147483647" step="1" value={cpa.draft.priority ?? "0"} disabled={busy || !cpa.loaded} onChange={event => cpa.setDraft(current => ({ ...current, priority: event.target.value }))} placeholder="默认 0" />
            <small className="settings-field-help">默认 0，数值越大越优先。</small>
          </label>
          <label className="settings-field">
            <span>权重</span>
            <input type="number" name="cpa-weight" min="0" max="1000000" step="1" value={cpa.draft.weight ?? "1"} disabled={busy || !cpa.loaded} onChange={event => cpa.setDraft(current => ({ ...current, weight: event.target.value }))} placeholder="默认 1" />
            <small className="settings-field-help">默认 1，控制同优先级账号的分配比例；0 不参与加权轮询。</small>
          </label>
        </div>
        <p className="settings-field-help">权重需 CPA 启用 weighted-round-robin。参数随下次手动或自动推送生效，保存不会修改远端账号或路由策略。</p>
      </section>
      {cpa.settings?.hasApiKey && cpa.settings.credentialPersistence === "memory_only" && <p className="dialog-error" role="status"><CircleAlert size={16} />当前系统无法持久保存管理密钥，仅本次服务运行有效；服务重启后请重新填写。</p>}
      {cpa.error && <p className="dialog-error" role="alert"><CircleAlert size={16} />{cpa.error}</p>}
      {cpa.notice && <p className="dialog-hint" role="status">{cpa.notice}</p>}
      <div className="dialog-actions">
        <button type="button" className="secondary-button" disabled={busy} onClick={() => void cpa.reload()}><RefreshCw size={16} />{cpa.loading ? "正在读取…" : "读取当前配置"}</button>
        <button type="submit" className="primary-button" disabled={busy || !cpa.loaded || cpa.uncertain}>{cpa.saving ? <LoaderCircle className="spin" size={16} /> : <Check size={16} />}保存配置</button>
      </div>
    </form>
  </div>;
}
