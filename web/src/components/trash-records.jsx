import React, { useEffect, useRef, useState } from "react";
import { ChevronLeft, ChevronRight, CircleAlert, FileText, RefreshCw, RotateCcw, Filter, Trash2 } from "lucide-react";
import { useConfirmation } from "./ui.jsx";
import { useRemovedRowFocus } from "../hooks/removalFocus.js";
import { canRestoreTrash, describeTrashDeadline, selectTrashPage, trashSelectionScope, trashTargetsUnchanged } from "../lib/accountTrash.js";
import { AccountTrashDetails, trashDate } from "./trash-details.jsx";

export function AccountTrashPage({ token, trash, available }) {
  const confirm = useConfirmation(), confirming = useRef(false), latest = useRef(null);
  const identity = useRef(null), mounted = useRef(true);
  if (!identity.current || identity.current.token !== token || identity.current.available !== available) identity.current = { token, available };
  const session = identity.current;
  const listHeadingRef = useRef(null), selectPageRef = useRef(null), detailTrigger = useRef(null), detailClosed = useRef(false);
  const [selected, setSelected] = useState(() => new Set()), [detailId, setDetailId] = useState(null);
  const [query, setQuery] = useState(""), [filter, setFilter] = useState("all"), [order, setOrder] = useState("newest"), [page, setPage] = useState(1);
  const [actionError, setActionError] = useState("");
  const blocked = !available || !trash.loaded || Boolean(trash.error) || Boolean(trash.actionBusy) || trash.verificationRequired || trash.status?.running === true;
  latest.current = { trash, blocked, token };
  const removalFocus = useRemovedRowFocus(trash.entries, listHeadingRef, blocked);
  const view = selectTrashPage(trash.entries, { query, filter, order, page });
  const scope = trashSelectionScope(trash.entries, selected, view.rows);
  const purgingCount = trash.entries.filter(entry => entry.state === "purging").length;
  useEffect(() => {
    if (!trash.loaded) return;
    const ids = new Set(trash.entries.map(entry => entry.id));
    setSelected(current => new Set([...current].filter(id => ids.has(id))));
  }, [trash.entries, trash.loaded]);
  useEffect(() => { setSelected(new Set()); setDetailId(null); setActionError(""); }, [token, available]);
  useEffect(() => { mounted.current = true; return () => { mounted.current = false; }; }, []);
  useEffect(() => {
    if (detailId || !detailClosed.current) return;
    detailClosed.current = false;
    const owner = listHeadingRef.current?.ownerDocument, focused = owner?.activeElement;
    if (!detailTrigger.current?.isConnected && (!focused || focused === owner?.body || !focused.isConnected)) listHeadingRef.current?.focus();
  }, [detailId]);
  useEffect(() => { if (page !== view.page) setPage(view.page); }, [page, view.page]);
  useEffect(() => { if (selectPageRef.current) selectPageRef.current.indeterminate = scope.someVisibleSelected && !scope.allVisibleSelected; }, [scope.someVisibleSelected, scope.allVisibleSelected]);
  function toggle(id) { setSelected(current => { const next = new Set(current); next.has(id) ? next.delete(id) : next.add(id); return next; }); }
  function togglePage() { setSelected(current => { const next = new Set(current); for (const entry of view.rows) scope.allVisibleSelected ? next.delete(entry.id) : next.add(entry.id); return next; }); }
  async function perform(action, records) {
    if (blocked || confirming.current || !records.length || action === "restore" && records.some(entry => !canRestoreTrash(entry))) return;
    const trigger = removalFocus.capture();
    let focusOperation;
    confirming.current = true;
    setActionError("");
    try {
      const label = records.length === 1 ? records[0].email : records.length + " 个账号";
      const hidden = records.filter(record => !view.rows.some(row => row.id === record.id)).length;
      const range = hidden ? "其中 " + hidden + " 个不在当前页（可能被筛选隐藏），本次操作包含这些账号。" : "";
      const message = action === "purge" ? "确定彻底删除 " + label + "？" + range + "账号资料、授权文件、凭据及全部历史日志将永久删除，无法恢复。Sub2API 中的账号不受影响。"
        : "确定恢复 " + label + " 及其全部历史记录？" + range + "不会自动登录、重新授权或上传；存在冲突时会保留回收记录并说明原因。";
      if (!await confirm(message, { title: action === "purge" ? "永久删除回收记录" : "恢复账号", confirmLabel: action === "purge" ? "彻底删除，无法恢复" : "恢复账号", danger: action === "purge" })) return;
      if (!mounted.current || identity.current !== session) return;
      if (latest.current.blocked || !trashTargetsUnchanged(records, latest.current.trash.entries, action)) {
        setActionError("确认期间回收记录或服务状态发生变化，本次未提交。请核对最新列表后重新操作。");
        return;
      }
      focusOperation = removalFocus.begin(trigger);
      const receipt = await latest.current.trash.mutate(action, records.map(entry => entry.id));
      if (!mounted.current || identity.current !== session) { focusOperation?.guard.cancel(); focusOperation = null; return; }
      removalFocus.complete(focusOperation, receipt?.count > 0 || ["unknown", "partial"].includes(receipt?.outcome));
      focusOperation = null;
    } finally {
      if (mounted.current && identity.current === session) removalFocus.complete(focusOperation, false);
      else focusOperation?.guard.cancel();
      confirming.current = false;
    }
  }
  function openDetails(entry, event) { detailTrigger.current = event.currentTarget; setDetailId(entry.id); }
  function closeDetails() {
    // Wait for the dialog's focus trap to detach before restoring lost focus.
    detailClosed.current = true;
    setDetailId(null);
  }
  return <section className="trash-page" aria-label="回收站">
    {!available ? <div className="section-card trash-empty" role="status">当前服务尚未提供回收站，请更新服务后使用。不会调用旧版永久删除接口。</div> : <>
      {(trash.persistenceUnconfirmed || trash.uncertain) && <p className="dialog-hint" role="status">自动清理已暂停，保留策略待确认，请在服务配置中核实。</p>}
      {trash.status?.lastError && <div className="global-notice warning" role="status"><CircleAlert size={16} /><span>上次自动清理未完成：{trash.status.lastError}</span><button type="button" className="trash-status-refresh" onClick={trash.reloadList} disabled={trash.loading || Boolean(trash.actionBusy)}>刷新状态</button></div>}
      {trash.error && <div className="global-notice warning" role="status"><CircleAlert size={16} /><span>{trash.loaded ? "回收站暂未更新，保留上次列表并暂停操作。" : "暂时无法读取回收站。"}{trash.error}</span></div>}
      {trash.verificationRequired && <p className="dialog-hint" role="status">操作已返回，正在重新读取回收站核实。读取完成前暂停新的操作。</p>}
      {actionError && <p className="dialog-error" role="alert">{actionError}</p>}
      <div className="section-card trash-list">
        <div className="card-header"><div><h2 ref={listHeadingRef} tabIndex={-1}>回收记录</h2><p role="status">{trash.loaded ? "共 " + trash.entries.length + " 个账号 · " + trash.entries.filter(canRestoreTrash).length + " 个可恢复" : "正在读取…"}{trash.status?.running ? " · 正在执行永久清理" : purgingCount > 0 ? " · " + purgingCount + " 个清理未完成" : ""}</p></div><button type="button" className="secondary-button" onClick={trash.reloadList} disabled={trash.loading || Boolean(trash.actionBusy)}><RefreshCw size={15} className={trash.loading ? "spin" : undefined} />刷新列表</button></div>
        <div className="trash-filters">
          <label className="trash-search"><Filter size={16} /><input type="search" aria-label="搜索回收账号或记录 ID" placeholder="搜索邮箱或记录 ID" value={query} onChange={event => { setQuery(event.target.value); setPage(1); }} /></label>
          <label><span>状态</span><select aria-label="筛选回收状态" value={filter} onChange={event => { setFilter(event.target.value); setPage(1); }}><option value="all">全部状态</option><option value="restorable">可恢复</option><option value="purging">清理未完成</option></select></label>
          <label><span>排序</span><select aria-label="回收记录排序" value={order} onChange={event => { setOrder(event.target.value); setPage(1); }}><option value="newest">最近移入优先</option><option value="oldest">最早移入优先</option><option value="expiry">即将清理优先</option></select></label>
        </div>
        <div className="trash-selection"><label><input ref={selectPageRef} type="checkbox" aria-label="选择本页回收记录" checked={scope.allVisibleSelected} disabled={blocked || !view.rows.length} onChange={togglePage} />选择本页</label><span role="status">已选 {scope.selectedEntries.length} 个{scope.hiddenCount > 0 && <strong> · 其中 {scope.hiddenCount} 个不在当前页</strong>}</span><button type="button" className="cancel-button" disabled={!scope.selectedEntries.length} onClick={() => setSelected(new Set())}>清空选择</button><div className="trash-tools"><button type="button" className="secondary-button" disabled={blocked || !scope.selectedEntries.length || scope.restorableCount !== scope.selectedEntries.length} onClick={() => perform("restore", scope.selectedEntries)}><RotateCcw size={15} />恢复所选</button><button type="button" className="secondary-button danger-outline" disabled={blocked || !scope.selectedEntries.length} onClick={() => perform("purge", scope.selectedEntries)}><Trash2 size={15} />彻底删除所选</button></div>{scope.selectedEntries.length > scope.restorableCount && <small>所选包含不可恢复记录；可清空后筛选“可恢复”重新选择。</small>}</div>
        <div className="trash-table-frame"><table className="trash-table"><thead><tr><th className="select-heading"><span className="trash-sr-only">选择</span></th><th>账号与历史</th><th>移入时间</th><th>保留与清理</th><th>操作</th></tr></thead><tbody>
          {view.rows.map(entry => { const deadline = describeTrashDeadline(entry, trash.settings, { uncertain: trash.uncertain }); return <tr key={entry.id} data-trash-id={entry.id}>
            <td className="select-cell"><input type="checkbox" checked={selected.has(entry.id)} disabled={blocked} aria-label={"选择回收账号 " + entry.email} onChange={() => toggle(entry.id)} /></td>
            <td className="trash-account-cell"><strong className="trash-email">{entry.email || "未记录邮箱"}</strong><small>{entry.recordCount} 条历史记录 · <span className={entry.state === "purging" ? "trash-danger" : undefined}>{canRestoreTrash(entry) ? "可恢复" : entry.state === "purging" ? "清理未完成 · 不可恢复" : "暂不可恢复"}</span></small>{entry.lastError && <p className="trash-row-error">{entry.lastError}</p>}</td>
            <td className="trash-time-cell" data-label="移入时间"><time dateTime={entry.deletedAt}>{trashDate(entry.deletedAt)}</time></td>
            <td className="trash-deadline-cell" data-label="保留与清理"><span className={"trash-deadline trash-" + deadline.tone}>{deadline.label}</span>{deadline.at ? <small><time dateTime={deadline.at}>{trashDate(deadline.at)}</time></small> : <small>{deadline.detail}</small>}</td>
            <td className="trash-actions-cell"><div className="trash-row-actions"><button type="button" className="secondary-button" disabled={blocked || !canRestoreTrash(entry)} onClick={() => perform("restore", [entry])}><RotateCcw size={14} />恢复</button><button type="button" className="secondary-button danger-outline" disabled={blocked} onClick={() => perform("purge", [entry])}><Trash2 size={14} />{entry.state === "purging" ? "继续清理" : "彻底删除"}</button><button type="button" className="secondary-button" onClick={event => openDetails(entry, event)} aria-haspopup="dialog"><FileText size={14} />详情与日志</button></div></td>
          </tr>; })}
          {!view.rows.length && <tr><td colSpan="5" className="trash-empty">{!trash.loaded ? trash.loading ? "正在读取回收记录…" : "尚未取得回收记录" : trash.entries.length ? <><p>没有匹配的回收记录</p><button className="secondary-button" type="button" onClick={() => { setQuery(""); setFilter("all"); setPage(1); }}>清除筛选</button></> : "回收站为空，删除的账号将在这里保留。"}</td></tr>}
        </tbody></table></div>
        <nav className="trash-pagination" aria-label="回收站分页"><span>{view.total ? "显示 " + view.start + "–" + view.end + " / " + view.total + " 个账号" : "0 个匹配账号"}</span><div><button type="button" className="secondary-button" aria-label="上一页回收记录" disabled={view.page <= 1} onClick={() => setPage(view.page - 1)}><ChevronLeft size={16} /></button><span>{view.page} / {view.totalPages}</span><button type="button" className="secondary-button" aria-label="下一页回收记录" disabled={view.page >= view.totalPages} onClick={() => setPage(view.page + 1)}><ChevronRight size={16} /></button></div></nav>
      </div>
    </>}
    {detailId && <AccountTrashDetails key={detailId} token={token} entry={trash.entries.find(entry => entry.id === detailId)} settings={trash.settings} uncertain={trash.uncertain} readError={trash.error} onClose={closeDetails} />}
  </section>;
}
