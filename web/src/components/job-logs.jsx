import React, { useEffect, useMemo, useRef, useState } from "react";
import {
  Check,
  ChevronRight,
  CircleAlert,
  Copy,
  Download,
  FileText,
  LoaderCircle,
  RefreshCw,
} from "lucide-react";
import { apiFetch, formatDateTime, operationLabel } from "../lib/console.js";
import "./job-logs.css";

const STAGES = {
  start: "开始执行任务",
  starting: "正在启动任务",
  ready: "执行环境已就绪",
  queued: "任务已加入队列",
  login: "正在登录账号",
  bps_login: "正在使用 BPS 登录授权",
  bps_continue: "正在继续 BPS 授权",
  legacy_session_start: "正在建立登录会话",
  legacy_bound_transport: "登录会话已就绪",
  legacy_challenge: "正在处理网站验证",
  security_check: "正在进行网站安全验证",
  password: "验证账号密码",
  password_add: "正在添加密码",
  password_add_starting: "准备添加密码",
  password_recovered: "已确认并恢复保存的密码",
  email_otp: "验证邮箱验证码",
  email_otp_requested: "已请求新的邮箱验证码",
  email_otp_rejected: "邮箱验证码未通过，请输入新验证码或重新发送",
  email_verified: "邮箱验证已完成",
  totp: "验证 2FA 验证码",
  mfa_otp: "验证 2FA 验证码",
  mfa_code_rejected: "2FA 验证码未通过，请输入新验证码",
  totp_starting: "准备设置 2FA",
  totp_setup: "激活新的 2FA",
  totp_setup_otp: "激活新的 2FA",
  totp_status_pending: "2FA 已激活，启用状态仍待确认",
  totp_reset_read: "检查现有 2FA 设置",
  totp_reset_disable: "正在移除旧 2FA",
  totp_reset_complete: "新 2FA 已激活、验证并安全保存",
  totp_logout_pending: "准备退出所有设备",
  totp_logout_running: "正在退出所有设备",
  totp_logout_succeeded: "已退出所有设备",
  totp_logout_failed: "退出所有设备失败",
  totp_logout_unknown: "退出设备的结果尚未确认",
  totp_workflow_totp: "开始 2FA 设置流程",
  totp_workflow_logout: "开始退出所有设备",
  totp_workflow_relogin: "开始重新登录并授权",
  totp_workflow_upload: "开始同步到 Sub2API",
  oauth: "正在获取授权",
  oauth_continue: "继续授权流程",
  callback_ready: "授权回调已就绪",
  tokens_received: "已取得授权数据",
  refreshing: "正在更新授权数据",
  finalizing: "正在保存授权文件",
  phone: "验证手机号",
  phone_sent: "手机验证码已发送",
  sms_otp: "验证手机验证码",
  phone_otp: "验证手机验证码",
  phone_verified: "手机验证已完成",
  phone_changed: "已更换手机号",
  sms_resent: "手机验证码已重新发送",
  sms_otp_rejected: "手机验证码未通过，请输入新验证码",
  phone_rejected: "手机号未通过验证，请更换号码",
  phone_send_deferred: "发送验证码暂未完成，可重新发送或更换号码",
  phone_send_retry_required: "发送验证码需要手动重试",
  sms_send_deferred: "发送验证码暂未完成，可稍后重试",
  sms_resend_deferred: "重发验证码暂未完成，可稍后重试",
  sms_otp_deferred: "验证码验证暂未完成，可稍后重试",
  resume_available: "流程已中断，可继续执行",
  reauth_required: "需要重新登录并授权",
  cancel: "任务已取消",
  canceled: "任务已取消",
  completed: "当前执行阶段已完成",
  succeeded: "操作已完成",
  failed: "操作执行失败",
};

const WAITING = new Set(["awaiting_input", "waiting_input", "waiting", "password", "email_otp", "mfa_otp", "phone", "phone_otp", "totp_setup_otp"]);
const WARNINGS = new Set([
  "email_otp_rejected", "mfa_code_rejected", "sms_otp_rejected", "phone_rejected",
  "phone_send_deferred", "phone_send_retry_required", "sms_send_deferred", "sms_resend_deferred", "sms_otp_deferred",
  "totp_status_pending", "totp_logout_unknown", "resume_available", "reauth_required",
]);
const LOG_KINDS = {
  error: { label: "失败", abnormal: true },
  warning: { label: "注意", abnormal: true },
  waiting: { label: "待输入", abnormal: false },
  success: { label: "完成", abnormal: false },
  progress: { label: "进度", abnormal: false },
  info: { label: "记录", abnormal: false },
};

function textField(value) {
  return typeof value === "string" ? value.trim() : "";
}

function operationStamp(value) {
  if (!value || String(value).startsWith("0001-")) return "";
  const date = new Date(value);
  // Both API fields use the same RFC3339 encoding. Preserve nanoseconds so
  // two operations started in one millisecond remain distinct.
  return Number.isNaN(date.getTime()) ? "" : String(value);
}

function parseRecord(raw) {
  let record;
  try {
    const decoded = JSON.parse(raw);
    if (decoded && typeof decoded === "object" && !Array.isArray(decoded)) record = decoded;
  } catch { /* Older operation logs are stage/status lines. */ }
  const timestamped = !record && /^(\d{4}-\d{2}-\d{2}T\S+)\s+\[(\w+)\]\s+(.*)$/.exec(raw.trim());
  if (timestamped) {
    record = { at: timestamped[1], level: timestamped[2], message: timestamped[3] };
    const stageLine = /^([a-z0-9_-]+)\s+·\s+([a-z0-9_-]+)$/.exec(timestamped[3]);
    if (stageLine) { record.stage = stageLine[1]; record.status = stageLine[2]; }
  }
  const legacy = !record && /^(\S+)(?:\s+(\S+))?$/.exec(raw.trim());
  const stage = textField(record?.stage) || (legacy ? legacy[1] : "");
  const status = textField(record?.operationStatus) || textField(record?.status) || (legacy ? legacy[2] : "");
  const level = textField(record?.level).toLowerCase();
  const stamp = record?.time || record?.at || record?.timestamp;
  const date = stamp ? new Date(stamp) : null;
  const validDate = date && !Number.isNaN(date.getTime()) && !String(stamp).startsWith("0001-");
  let kind = "info";
  if (["failed", "error"].includes(status)) kind = "error";
  else if (["canceled", "interrupted", "unknown"].includes(status)) kind = "warning";
  else if (["error", "fatal"].includes(level) || ["failed", "totp_logout_failed"].includes(stage)) kind = "error";
  else if (["warn", "warning"].includes(level) || WARNINGS.has(stage) || ["cancel", "canceled"].includes(stage)) kind = "warning";
  else if (WAITING.has(status)) kind = "waiting";
  else if (level === "success" || ["succeeded", "completed"].includes(status) || ["totp_logout_succeeded", "totp_reset_complete", "phone_verified", "email_verified"].includes(stage)) kind = "success";
  else if (["running", "working", "starting", "queued"].includes(status)) kind = "progress";

  const knownStage = STAGES[stage];
  const suppliedMessage = textField(record?.message) || textField(record?.prompt) || textField(record?.msg);
  // Protocol prompts can contain long English technical details. Use a known
  // Chinese stage summary while leaving the exact server record available.
  const humanMessage = suppliedMessage && /[\u3400-\u9fff]/.test(suppliedMessage);
  const requestTimedOut = /context deadline exceeded|(?:request|proxy connection) timed out/i.test(suppliedMessage);
  let message = humanMessage ? suppliedMessage : knownStage || suppliedMessage || raw;
  let translatedMessage = "";
  if (!humanMessage && ["totp_logout_failed", "totp_logout_unknown"].includes(stage)) {
    // Logout can invalidate sessions even when its response never arrives.
    // Keep the response status visible and never suggest replaying this stage.
    const http = suppliedMessage.match(/\bHTTP(?:\s+status)?\s*[:=]?\s*(\d{3})\b/i)?.[1];
    const reason = requestTimedOut ? "请求超时" : /context canceled/i.test(suppliedMessage) ? "请求已取消" : "";
    translatedMessage = [knownStage, http && `HTTP ${http}`, reason].filter(Boolean).join(" · ") + "；后续步骤已停止，不会自动重试";
    if (stage === "totp_logout_unknown") translatedMessage += "，请先核实设备状态";
  } else if (!humanMessage && requestTimedOut) translatedMessage = "请求超时，请检查网络后重试";
  else if (!humanMessage && /context canceled/i.test(suppliedMessage)) translatedMessage = "执行已取消";
  else if (!humanMessage && /incorrect_code/i.test(suppliedMessage)) translatedMessage = "验证码未通过，请使用当前有效验证码";
  if (translatedMessage) message = translatedMessage;
  if (!humanMessage && !translatedMessage && kind === "waiting" && knownStage) message = knownStage.replace(/^验证/, "等待输入");
  if (!humanMessage && !translatedMessage && kind === "error" && knownStage && !["failed", "totp_logout_failed"].includes(stage)) message = `${knownStage}：执行失败`;
  if (!humanMessage && !translatedMessage && ["canceled", "interrupted"].includes(status) && !["cancel", "canceled"].includes(stage)) message = `${knownStage || "操作"}：${status === "canceled" ? "已取消" : "已中断"}`;
  return {
    raw,
    operation: record?.operation === "full" ? "initial_authorization" : textField(record?.operation),
    operationAt: operationStamp(record?.operationAt),
    time: validDate ? formatDateTime(date) : "—",
    fullTime: validDate ? date.toLocaleString("zh-CN", { hour12: false }) : "此记录未提供时间",
    kind,
    message,
    ...LOG_KINDS[kind],
  };
}

export function parseJobLogs(logs, structuredEntries) {
  let rawLogs = Array.isArray(logs) ? logs.map((line) => typeof line === "string" ? line : JSON.stringify(line)).join("\n") : String(logs || "");
  rawLogs = rawLogs.replace(/\r\n/g, "\n");
  let lines;
  if (Array.isArray(structuredEntries) && structuredEntries.length) {
    // Remove the complete text counterpart of each structured event before
    // splitting legacy records. Event messages may span several physical lines.
    for (const entry of structuredEntries) {
      if (!entry?.at || !entry?.level || typeof entry.message !== "string") continue;
      const at = String(entry.at).replace(/\.\d+(?=Z$|[+-]\d{2}:\d{2}$)/, "");
      const counterpart = `${at} [${entry.level}] ${entry.message}`.replace(/\r\n/g, "\n");
      let start = rawLogs.indexOf(counterpart);
      while (start !== -1) {
        const end = start + counterpart.length;
        if ((start === 0 || rawLogs[start - 1] === "\n") && (end === rawLogs.length || rawLogs[end] === "\n")) {
          rawLogs = rawLogs.slice(0, start) + rawLogs.slice(end);
          break;
        }
        start = rawLogs.indexOf(counterpart, start + 1);
      }
    }
    // Preserve records absent from the structured history, including older
    // timestamped records whose operation cannot be determined.
    lines = [...rawLogs.split("\n").filter((line) => line.trim()), ...structuredEntries.map((entry) => JSON.stringify(entry))].slice(-200);
  } else lines = rawLogs.split("\n");
  const occurrences = new Map();
  return lines.filter((line) => line.trim()).map((raw) => {
    const occurrence = (occurrences.get(raw) || 0) + 1;
    occurrences.set(raw, occurrence);
    return { ...parseRecord(raw), key: `${raw}:${occurrence}` };
  });
}

async function copyText(value) {
  if (navigator.clipboard?.writeText) {
    await navigator.clipboard.writeText(value);
    return;
  }
  const area = document.createElement("textarea");
  area.value = value;
  area.setAttribute("readonly", "");
  area.style.cssText = "position:fixed;left:-9999px;top:0";
  document.body.append(area);
  area.select();
  const copied = document.execCommand("copy");
  area.remove();
  if (!copied) throw new Error("浏览器未允许复制，请展开原始记录后手动复制");
}

export function JobLogs({ token, jobId, job }) {
  const [state, setState] = useState({ jobId, logs: "", entries: null, loaded: false, loading: true, error: "", updatedAt: null });
  const [filter, setFilter] = useState("all");
  const [scope, setScope] = useState("current");
  const [followLatest, setFollowLatest] = useState(true);
  const [copyStatus, setCopyStatus] = useState("");
  const viewportRef = useRef(null);
  const refreshRef = useRef(null);
  const copyTimerRef = useRef(null);
  const jobRef = useRef(job);
  const mountedRef = useRef(false);
  const generationRef = useRef(0);
  jobRef.current = job;
  const current = state.jobId === jobId ? state : { logs: "", entries: null, loaded: false, loading: true, error: "", updatedAt: null };
  const entries = useMemo(() => parseJobLogs(current.logs, current.entries), [current.logs, current.entries]);
  const latestOperation = operationStamp(job?.lastOperationAt);
  const canScope = Boolean(latestOperation && entries.some((entry) => entry.operationAt));
  const scoped = canScope && scope === "current" ? entries.filter((entry) => entry.operationAt === latestOperation) : entries;
  const abnormalCount = scoped.filter((entry) => entry.abnormal).length;
  const visible = filter === "abnormal" ? scoped.filter((entry) => entry.abnormal) : scoped;

  useEffect(() => {
    let stopped = false;
    let timer;
    let requestTimer;
    let controller;
    let loading = false;
    mountedRef.current = true;
    generationRef.current += 1;
    setState({ jobId, logs: "", entries: null, loaded: false, loading: true, error: "", updatedAt: null });
    setFilter("all");
    setScope("current");
    setFollowLatest(true);
    setCopyStatus("");

    const load = async () => {
      if (stopped || loading) return;
      loading = true;
      window.clearTimeout(timer);
      controller = new AbortController();
      requestTimer = window.setTimeout(() => controller?.abort(), 10_000);
      setState((old) => ({ ...old, loading: true }));
      try {
        const data = await apiFetch(token, `/api/jobs/${encodeURIComponent(jobId)}/logs`, { signal: controller.signal });
        if (!stopped) setState({ jobId, logs: data.logs || "", entries: data.entries || null, loaded: true, loading: false, error: "", updatedAt: new Date() });
      } catch (error) {
        if (!stopped) {
          const message = error.name === "AbortError" ? "日志请求超时" : error.message || "日志读取失败";
          setState((old) => ({ ...old, loading: false, error: message }));
        }
      } finally {
        window.clearTimeout(requestTimer);
        loading = false;
        if (!stopped) {
          const status = jobRef.current?.operationStatus || jobRef.current?.status;
          const terminal = ["imported", "completed", "succeeded", "failed", "canceled", "interrupted", "resume_available", "reauth_required"].includes(status);
          timer = window.setTimeout(load, terminal ? 5_000 : 1_800);
        }
      }
    };
    refreshRef.current = load;
    load();
    return () => {
      stopped = true;
      mountedRef.current = false;
      generationRef.current += 1;
      refreshRef.current = null;
      window.clearTimeout(timer);
      window.clearTimeout(requestTimer);
      window.clearTimeout(copyTimerRef.current);
      controller?.abort();
    };
  }, [jobId, token]);

  useEffect(() => {
    if (followLatest && viewportRef.current) viewportRef.current.scrollTop = viewportRef.current.scrollHeight;
  }, [current.logs, current.entries, latestOperation, filter, scope, followLatest]);

  useEffect(() => {
    setScope("current");
    setFollowLatest(true);
  }, [latestOperation]);

  function onScroll() {
    const viewport = viewportRef.current;
    if (viewport && viewport.scrollHeight - viewport.scrollTop - viewport.clientHeight > 36) setFollowLatest(false);
  }

  async function copy() {
    const generation = generationRef.current;
    try {
      await copyText(visible.map((entry) => entry.raw).join("\n"));
      if (!mountedRef.current || generation !== generationRef.current) return;
      setCopyStatus("已复制当前显示日志");
    } catch (error) {
      if (!mountedRef.current || generation !== generationRef.current) return;
      setCopyStatus(error.message || "复制失败");
    }
    window.clearTimeout(copyTimerRef.current);
    copyTimerRef.current = window.setTimeout(() => setCopyStatus(""), 3_000);
  }

  return (
    <section className="job-log-panel" aria-label="操作日志">
      <div className="job-log-header">
        <div className="job-log-heading"><FileText size={15} /><strong>操作日志</strong><span className="job-log-count">{scoped.length} 条</span></div>
        <div className="job-log-tools">
          {canScope && <div className="job-log-filters job-log-scopes" role="group" aria-label="日志执行范围">
            <button type="button" aria-pressed={scope === "current"} onClick={() => setScope("current")}>本次</button>
            <button type="button" aria-pressed={scope === "history"} onClick={() => setScope("history")}>全部历史</button>
          </div>}
          <div className="job-log-filters" role="group" aria-label="筛选操作日志">
            <button type="button" aria-pressed={filter === "all"} onClick={() => setFilter("all")}>全部</button>
            <button type="button" aria-pressed={filter === "abnormal"} onClick={() => setFilter("abnormal")}>异常{abnormalCount > 0 && <span>{abnormalCount}</span>}</button>
          </div>
          <button type="button" className="job-log-tool" onClick={() => refreshRef.current?.()} disabled={current.loading} aria-label="刷新操作日志" title="刷新日志">{current.loading ? <LoaderCircle size={14} className="spin" /> : <RefreshCw size={14} />}</button>
          <button type="button" className="job-log-tool" onClick={copy} disabled={!visible.length} aria-label="复制当前显示的日志" title="复制当前显示的原始日志">{copyStatus.startsWith("已复制") ? <Check size={14} /> : <Copy size={14} />}</button>
        </div>
      </div>
      {current.error && <div className="job-log-read-error" role="status"><CircleAlert size={14} /><span>{current.loaded ? "日志更新暂时失败，保留上次读取内容。" : "日志暂时无法读取。"}<small>{current.error}</small></span></div>}
      <div className="job-log-column-head" aria-hidden="true"><span>时间</span><span>类别</span><span>执行内容</span></div>
      <div className="job-log-viewport" ref={viewportRef} onScroll={onScroll} tabIndex={0} aria-label="操作日志记录">
        {visible.length > 0 ? visible.map((entry, index) => (
          <React.Fragment key={entry.key}>
          {canScope && scope === "history" && (!index || entry.operationAt !== visible[index - 1].operationAt) && <div className="job-log-operation-group"><strong>{entry.operationAt ? operationLabel(entry.operation) : "早期日志"}</strong><span>{entry.operationAt ? formatDateTime(entry.operationAt) : "未记录执行时间"}</span>{entry.operationAt === latestOperation && <b>本次</b>}</div>}
          <div className={`job-log-entry kind-${entry.kind}`}>
            <time className="job-log-time" title={entry.fullTime}>{entry.time}</time>
            <span className="job-log-kind">{entry.label}</span>
            <details className="job-log-message" onToggle={(event) => { if (event.currentTarget.open) setFollowLatest(false); }}>
              <summary><ChevronRight size={12} /><span>{entry.message}</span></summary>
              <pre aria-label="原始日志记录">{entry.raw}</pre>
            </details>
          </div>
          </React.Fragment>
        )) : <div className="job-log-empty">{!current.loaded && current.loading ? <><LoaderCircle size={16} className="spin" />正在读取日志</> : filter === "abnormal" ? "当前范围没有异常记录" : canScope && scope === "current" && entries.length ? "本次执行暂无日志，可查看全部历史" : "暂无操作日志"}</div>}
      </div>
      <div className="job-log-footer">
        <span className="job-log-update" role="status">{copyStatus || (current.updatedAt ? `更新于 ${formatDateTime(current.updatedAt)}` : "打开时自动更新")}</span>
        <button type="button" className={`job-log-follow${followLatest ? " active" : ""}`} aria-pressed={followLatest} onClick={() => setFollowLatest((old) => !old)} title={followLatest ? "查看历史时会自动暂停跟随" : "点击跟随最新日志"}><Download size={13} />{followLatest ? "跟随最新" : "继续跟随"}</button>
      </div>
    </section>
  );
}
