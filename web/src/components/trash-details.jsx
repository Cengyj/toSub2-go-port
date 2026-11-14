import React, { useEffect, useRef, useState } from "react";
import { FileText, X } from "lucide-react";
import { DialogFrame } from "./ui.jsx";
import { JobLogs } from "./job-logs.jsx";
import { canRestoreTrash, describeTrashDeadline } from "../lib/accountTrash.js";
import { describeStatus, jobOperation } from "../lib/jobPresentation.js";

export function trashDate(value) {
  const date = new Date(value);
  return value && Number.isFinite(date.getTime()) ? date.toLocaleString("zh-CN", { year: "numeric", month: "2-digit", day: "2-digit", hour: "2-digit", minute: "2-digit" }) : "未知";
}
export function AccountTrashDetails({ token, entry, settings, uncertain, readError, onClose }) {
  const closeRef = useRef(null);
  const [jobId, setJobId] = useState(entry?.jobs[0]?.id || "");
  const job = entry?.jobs.find(item => item.id === jobId) || entry?.jobs[0];
  const deadline = entry && describeTrashDeadline(entry, settings, { uncertain });
  useEffect(() => {
    const owner = closeRef.current?.ownerDocument, focused = owner?.activeElement;
    if (!entry && (!focused || focused === owner?.body || focused === owner?.documentElement || !focused.isConnected)) closeRef.current?.focus();
  }, [entry]);
  return <DialogFrame onClose={onClose}><section className="batch-dialog trash-detail-dialog" role="dialog" aria-modal="true" aria-labelledby="trash-detail-title">
    <div className="dialog-header"><div><h2 id="trash-detail-title"><FileText size={18} />回收记录详情</h2><p className="trash-detail-email">{entry?.email || "记录已不在回收站"}</p></div><button ref={closeRef} type="button" className="icon-button" aria-label="关闭回收记录详情" onClick={onClose}><X size={18} /></button></div>
    {!entry ? <p className="dialog-hint" role="status">这条记录已被恢复或清理，请关闭详情后查看最新列表。</p> : <>
      {readError && <p className="dialog-error" role="status">列表暂未更新，以下为最近读取的信息：{readError}</p>}
      <dl className="trash-detail-meta"><div><dt>回收状态</dt><dd>{canRestoreTrash(entry) ? "可恢复 · 不会自动执行任务" : entry.state === "purging" ? "永久清理未完成 · 已不可恢复" : "暂不可恢复"}</dd></div><div><dt>移入时间</dt><dd><time dateTime={entry.deletedAt}>{trashDate(entry.deletedAt)}</time></dd></div><div><dt>保留与清理</dt><dd className={"trash-" + deadline.tone}>{deadline.label}{deadline.at && <small>{trashDate(deadline.at)}</small>}<small>{deadline.detail}</small></dd></div><div><dt>回收记录 ID</dt><dd className="trash-record-id">{entry.id}</dd></div></dl>
      {entry.lastError && <div className="trash-detail-error" role="status"><strong>最近操作未完成</strong><p>{entry.lastError}</p></div>}
      <section className="trash-detail-history" aria-labelledby="trash-history-title"><h3 id="trash-history-title">历史记录与日志 <small>{entry.recordCount} 条记录</small></h3><p className="dialog-hint">查看已有日志不会恢复账号或重放任务。原先未保存的日志无法补回。</p>
        {job ? <><label className="trash-history-picker">选择历史记录<select aria-label={"选择历史日志 " + entry.email} value={job.id} onChange={event => setJobId(event.target.value)}>{entry.jobs.map(item => <option key={item.id} value={item.id}>{trashDate(item.createdAt)} · {jobOperation(item).label} · {describeStatus(item.status).label} · {item.id}</option>)}</select></label><div className="trash-history-summary"><span>{jobOperation(job).label} · {describeStatus(job.status).label}</span><span className="trash-record-id">{job.id}</span></div><JobLogs key={entry.id + ":" + job.id} token={token} jobId={job.id} job={job} archiveId={entry.id} /></> : <p className="trash-empty">当前回收记录没有可读取的历史任务。</p>}
      </section>
    </>}
    <div className="dialog-footer"><button type="button" className="secondary-button" onClick={onClose}>关闭</button></div>
  </section></DialogFrame>;
}
