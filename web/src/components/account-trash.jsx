import React, { useEffect, useRef, useState } from "react";
import { LoaderCircle, RefreshCw } from "lucide-react";
import { useConfirmation } from "./ui.jsx";
import "./account-trash.css";

export { AccountTrashPage } from "./trash-records.jsx";

export function AccountTrashSettings({ trash }) {
  const confirm = useConfirmation(), confirming = useRef(false), latest = useRef(trash), mounted = useRef(true);
  latest.current = trash;
  useEffect(() => { mounted.current = true; return () => { mounted.current = false; }; }, []);
  const [saved, setSaved] = useState(false);
  const [checking, setChecking] = useState(false), checkInFlight = useRef(false);
  const blocked = !trash.settings || trash.settingsLoading || trash.saving || trash.uncertain || trash.verificationRequired || Boolean(trash.actionBusy);
  const draftDays = Number(trash.retentionDraft);
  const validDraft = /^[0-9]+$/.test(trash.retentionDraft) && Number.isSafeInteger(draftDays) && draftDays >= 0 && draftDays <= 3650;
  async function verifyRecords() {
    if (checkInFlight.current || trash.actionBusy) return;
    checkInFlight.current = true; setChecking(true);
    try { await trash.reloadList(); }
    finally { checkInFlight.current = false; if (mounted.current) setChecking(false); }
  }
  async function save(event) {
    event.preventDefault();
    if (confirming.current || blocked) return;
    const days = Number(trash.retentionDraft), revision = trash.settings.revision, draft = trash.retentionDraft;
    confirming.current = true;
    try {
      if (validDraft && days > 0 && (trash.settings.retentionDays === 0 || days < trash.settings.retentionDays || trash.persistenceUnconfirmed)) {
        if (!await confirm("保留期将按账号移入回收站的时间计算，并应用于已有记录。启用或缩短为 " + days + " 天后，已到期的账号、日志及凭据可能立即被永久清理，无法恢复。确定保存吗？", { title: "启用或缩短自动清理保留期", confirmLabel: "确认保存清理策略", danger: true })) return;
      }
      if (!mounted.current || latest.current.verificationRequired || latest.current.actionBusy || latest.current.uncertain) return;
      const confirmed = await trash.saveSettings({ draft, revision });
      if (mounted.current) setSaved(confirmed);
    } finally { confirming.current = false; }
  }
  return <form className="section-card settings-form" onSubmit={save}>
    <div className="card-header"><h2>回收站保留策略</h2><p>配置保存在本机服务，所有浏览器共用。</p></div>
    <div className="card-body">
      <p className="dialog-hint">删除账号时先移入回收站，保留账号资料、历史日志与授权文件，恢复后仍由你决定后续操作。</p>
      <div className="trash-policy-current"><strong>当前生效策略</strong><span>{!trash.settings ? trash.settingsLoading ? "正在读取…" : "尚未读取" : trash.persistenceUnconfirmed || trash.uncertain ? "自动清理暂停 · 需核实并确认" : trash.settings.retentionDays === 0 ? "持续保留 · 自动清理关闭" : "移入后保留 " + trash.settings.retentionDays + " 天"}</span></div>
      <label className="settings-field"><span>自动清理保留天数</span><input className="input trash-retention-input" type="number" min="0" max="3650" step="1" value={trash.retentionDraft} disabled={blocked} aria-describedby="trash-retention-help trash-retention-preview" onChange={event => { trash.setRetentionDraft(event.target.value); setSaved(false); }} /></label>
      <p id="trash-retention-help" className="dialog-hint">0 表示关闭自动清理，持续保留。1–3650 天按移入时间计算，到期后永久删除；修改策略也适用于已有回收记录。关闭自动清理不会撤销已经开始的永久清理。</p>
      <p id="trash-retention-preview" className="trash-retention-preview" role="status">{!trash.settings ? "读取服务端策略后可编辑。" : trash.dirty ? !validDraft ? "请输入 0–3650 之间的整数。" : draftDays === 0 ? "待保存：关闭后，尚未开始清理的账号将持续保留。" : "待保存：按原移入时间保留 " + draftDays + " 天，已到期的记录可能立即清理。" : "修改后点击保存才会生效。"}</p>
      {trash.verificationRequired && <div><p className="dialog-hint" role="status">上次回收站操作尚待核实，完成前暂停修改清理策略。可在这里重新读取状态，不会重放操作。</p><button type="button" className="secondary-button" disabled={checking || Boolean(trash.actionBusy)} onClick={verifyRecords}><RefreshCw size={14} className={checking ? "spin" : undefined} />核实回收站状态</button>{trash.error && <p className="dialog-error" role="status">核实未完成：{trash.error}</p>}</div>}
      {trash.actionBusy && <p className="dialog-hint" role="status">回收站操作进行中，完成后可修改保留策略。</p>}
      {trash.uncertain && <p className="dialog-error" role="status">上次保存结果尚未确认，请先重新读取配置，不会自动重试保存。</p>}
      {trash.persistenceUnconfirmed && <p className="dialog-error" role="status">自动清理暂停，需明确重新保存。{trash.settings?.persistenceWarning}</p>}
      {trash.settingsError && <p className="dialog-error" role="alert">{trash.settingsError}</p>}
      {saved && !trash.dirty && !trash.uncertain && !trash.persistenceUnconfirmed && <p className="provider-ready" role="status">保留策略已保存。</p>}
    </div>
    <div className="dialog-footer">
      <button type="button" className="secondary-button" onClick={() => { setSaved(false); trash.reloadSettings(); }} disabled={trash.settingsLoading || trash.saving || Boolean(trash.actionBusy)}><RefreshCw size={15} />重新读取配置</button>
      <button type="button" className="cancel-button" onClick={() => { trash.resetDraft(); setSaved(false); }} disabled={!trash.dirty || blocked}>放弃更改</button>
      <button type="submit" className="primary-button" disabled={blocked || !validDraft || !trash.dirty && !trash.persistenceUnconfirmed}>{trash.saving && <LoaderCircle size={15} className="spin" />}{trash.persistenceUnconfirmed ? "重新确认保存" : "保存保留策略"}</button>
    </div>
  </form>;
}
