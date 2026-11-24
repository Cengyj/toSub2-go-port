import React from "react";
import { CircleAlert, LoaderCircle, Pause, RefreshCw } from "lucide-react";

export function MaintenanceSettings({ maintenance }) {
  if (!maintenance.enabled) return null;
  const state = maintenance.status;
  const confirmed = state && !state.uncertain && !maintenance.verificationRequired && !maintenance.error;
  const drained = confirmed && state.enabled && state.drained;
  return <section className="section-card maintenance-settings" aria-label="维护与排空">
    <div className="card-header"><h2>维护与排空</h2><p>进入维护不取消任务；排空后仍需正常停服，再备份或升级。</p></div>
    <div className="card-body">
      <p role="status">{!state ? "正在读取维护状态…" : !confirmed ? "维护状态待核实；新操作可能仍被暂停" : drained ? "维护已启用，当前工作已排空" : state.enabled ? "维护已启用，等待已受理工作排空" : "正常运行，允许受理新操作"}</p>
      <p className="dialog-hint">维护期间暂停新任务和自动来源。已受理任务仍可输入或取消；同主机 CLI 的活动也计入排空判断。</p>
      {state && <p className="dialog-hint">排队 {state.jobs.queued} · 运行 {state.jobs.running} · 查询 {state.jobs.queries + state.insights} · 探针 {state.probes} · 短信 {state.sms} · 在途请求 {state.activeRequests}{state.recycleRunning ? " · 回收站正在清理" : ""}</p>}
      {maintenance.error && <p className="dialog-error" role="alert"><CircleAlert size={14} />{maintenance.error}</p>}
      <div className="maintenance-actions">
        <button type="button" className="secondary-button" disabled={maintenance.saving || maintenance.loading} onClick={maintenance.reload}><RefreshCw size={14} />读取核实维护状态</button>
        <button type="button" className="primary-button" disabled={!maintenance.canChange} onClick={() => maintenance.change(!state?.enabled)}>{maintenance.saving ? <LoaderCircle className="spin" size={14} /> : <Pause size={14} />}{state?.enabled ? "退出维护并恢复准入" : "进入维护并暂停准入"}</button>
        {state?.uncertain && <button type="button" className="secondary-button" disabled={!maintenance.canChange} onClick={() => maintenance.change(true)}>重新确认保持维护</button>}
      </div>
    </div>
  </section>;
}
