import React, { useEffect, useRef, useState } from "react";
import { Activity, Ban, Check, CircleAlert, LoaderCircle, RefreshCw } from "lucide-react";
import { createPollingTask } from "../lib/polling.js";
import { QUALITY_REASONING_EFFORTS, qualityDate } from "../lib/accountQuality.js";
import { ProbeFailure } from "./probe-failure.jsx";
import { isRateLimited } from "../lib/accountHTTP.js";
import { pelicanOutput, validPelicanOutput } from "../lib/pelicanPreview.js";
import { PelicanPreview } from "./pelican-preview.jsx";

export function validateManualProbe(value, id) {
  if (!value || value.id !== id || value.task !== "pelican-svg" || !["idle", "running", "success", "failed", "cancelled"].includes(value.state)
    || value.state !== "idle" && (typeof value.requestId !== "string" || !value.requestId)
    || value.state === "success" && !validPelicanOutput(value)) throw new Error("手动检测状态不完整，请重新核对。");
  return value;
}

export function ManualProbePanel({ job, quality, options = {}, onActivityChange }) {
  const identity = useRef({ id: job.id, session: quality.manualSession });
  if (identity.current.id !== job.id || identity.current.session !== quality.manualSession) identity.current = { id: job.id, session: quality.manualSession };
  const scope = identity.current;
  const [savedRecord, setRecord] = useState(null), [loaded, setLoaded] = useState(false), [error, setError] = useState("");
  const [busy, setBusy] = useState(""), [verify, setVerify] = useState(false), [clock, setClock] = useState(Date.now());
  const latest = useRef(quality), mounted = useRef(false), poll = useRef(null), writing = useRef(false), pending = useRef(null), epoch = useRef(0);
  const verification = useRef(false), currentRecord = useRef(null), attempt = useRef(null);
  const record = savedRecord?.scope === scope ? savedRecord.value : null;
  function requireVerification(value) { verification.current = value; setVerify(value); }
  function adoptRecord(value) { currentRecord.current = { scope, value }; setRecord(currentRecord.current); }
  latest.current = quality;
  const activityListener = useRef(onActivityChange); activityListener.current = onActivityChange;
  useEffect(() => {
    mounted.current = true; ++epoch.current; writing.current = false; attempt.current = null; setBusy(""); adoptRecord(null); setLoaded(false); setError(""); requireVerification(false);
    let readGeneration = epoch.current;
    const task = createPollingTask({ intervalMs: 1500,
      request: async signal => { const generation = readGeneration = epoch.current; return { data: await latest.current.manualAPI(`?id=${encodeURIComponent(job.id)}`, { signal, cache: "no-store" }), generation }; },
      onData: ({ data, generation }) => {
        if (writing.current || identity.current !== scope || generation !== epoch.current) return;
        const next = validateManualProbe(data, job.id), expected = attempt.current;
        if (expected && !expected.rejected && (next.requestId !== expected.requestId || next.state === "idle")) {
          setError("本次操作结果待核实，当前读取尚未确认对应请求。请继续核对，避免重复提交。");
          requireVerification(true);
          return;
        }
        if (expected && next.requestId === expected.requestId && next.state !== "idle") attempt.current = null;
        adoptRecord(next); setLoaded(true); requireVerification(false); setError(attempt.current?.rejected || ""); setClock(Date.now());
      },
      onError: failure => { if (writing.current || identity.current !== scope || readGeneration !== epoch.current) return; setError(failure.message); requireVerification(true); },
    });
    poll.current = task;
    return () => { mounted.current = false; ++epoch.current; task.stop(); pending.current?.abort(); poll.current = null; };
  }, [job.id, quality.manualSession]);
  const running = record?.state === "running", cooling = !running && Date.parse(record?.nextAllowedAt) > clock;
  const remainingSeconds = cooling ? Math.ceil((Date.parse(record.nextAllowedAt) - clock) / 1000) : 0;
  const selectedOptions = { model: typeof options.model === "string" ? options.model.trim() : "", reasoningEffort: typeof options.reasoningEffort === "string" ? options.reasoningEffort : "" };
  const enabled = quality.settingsLoaded && !quality.settingsLoading && !quality.settingsVerificationRequired && !quality.uncertain && quality.settings.enabled && Boolean(selectedOptions.model) && QUALITY_REASONING_EFFORTS.includes(selectedOptions.reasoningEffort);
  useEffect(() => { activityListener.current?.(Boolean(running || busy || verify)); }, [running, busy, verify]);
  const otherProbeBusy = quality.status?.active > 0 || quality.status?.queued > 0;
  const blocked = !loaded || !enabled || running || cooling || Boolean(busy) || verify || Boolean(quality.actionBusy) || otherProbeBusy;
  async function act(action) {
    const observed = currentRecord.current?.scope === scope ? currentRecord.current.value : null;
    if (!mounted.current || identity.current !== scope || writing.current || verification.current || !observed
      || action === "start" && (blocked || observed.state === "running" || Date.parse(observed.nextAllowedAt) > Date.now())
      || action === "cancel" && observed.state !== "running") return;
    writing.current = true; setBusy(action); setError(""); requireVerification(true);
    const generation = ++epoch.current, current = () => mounted.current && identity.current === scope && epoch.current === generation;
    const controller = new AbortController(); pending.current = controller;
    const timer = window.setTimeout(() => controller.abort(), 15000);
    try {
      const body = action === "start" ? { id: job.id, task: "pelican-svg", requestId: crypto.randomUUID(), options: selectedOptions } : { id: job.id, requestId: observed.requestId };
      attempt.current = { scope, action, requestId: body.requestId, rejected: "" };
      const data = await latest.current.manualAPI(action === "cancel" ? "/cancel" : "", { method: "POST", body: JSON.stringify(body), signal: controller.signal });
      if (!current()) return;
      const next = validateManualProbe(data, job.id);
      if (next.requestId !== body.requestId || next.state === "idle") throw new Error("回执与本次检测请求不一致。");
      attempt.current = null;
      adoptRecord(next); setLoaded(true); requireVerification(false); setClock(Date.now());
    } catch (failure) {
      if (current()) {
        // Only an explicit local API refusal establishes that admission failed.
        // A lost response or an older successful read cannot establish that.
        if (attempt.current && failure.status >= 400 && failure.status < 500 && ![408, 425].includes(failure.status) && typeof failure.data?.error === "string") {
          attempt.current.rejected = `本次请求未受理。${failure.message}`;
          setError(attempt.current.rejected);
        } else setError(`操作结果待核实。${failure.name === "AbortError" ? "请求已超时。" : failure.message}`);
      }
    } finally {
      window.clearTimeout(timer); if (pending.current === controller) pending.current = null;
      // Reads started before or during this write cannot settle its outcome.
      // Only the explicit readback below may release unknown-result protection.
      if (current()) { ++epoch.current; writing.current = false; setBusy(""); poll.current?.refresh(); void latest.current.reload(); }
    }
  }
  const labels = { idle: "等待检测", running: "正在绘制", success: "绘制完成", failed: isRateLimited(record?.httpStatus, record?.errorCode) ? "请求限流" : "检测失败", cancelled: "已取消" };
  const Icon = running ? LoaderCircle : record?.state === "success" ? Check : record?.state === "failed" ? CircleAlert : Activity;
  const submittedStart = attempt.current?.scope === scope && attempt.current.action === "start";
  const previousResult = submittedStart && record?.state === "success" && record.requestId !== attempt.current.requestId;
  const awaitingStart = submittedStart && !attempt.current.rejected;
  const showAdmission = busy === "start" || submittedStart;
  return <div className="manual-probe">
    <div className="probe-runbar"><p>视觉与指令遵循<span>不影响糖果题判定和上传策略</span></p><div><button type="button" className="primary-button" disabled={blocked} onClick={() => void act("start")}>{busy === "start" || running ? <LoaderCircle size={15} className="spin" /> : <Activity size={15} />}{running ? "正在绘制…" : "开始手动检测"}</button>{running && <button type="button" className="secondary-button" disabled={Boolean(busy) || verify} onClick={() => void act("cancel")}><Ban size={14} />取消</button>}</div></div>
    <div className="probe-result-area">
      {cooling && <p className="dialog-hint" role="status">冷却中，还需 {remainingSeconds} 秒 · 下次可检测：{qualityDate(record.nextAllowedAt)}</p>}
      {!running && otherProbeBusy && <p className="dialog-hint" role="status">其他探针正在执行或排队，完成后可开始手动检测。</p>}
      {!enabled && <p className="dialog-hint">{quality.settings.enabled ? "请选择模型与推理强度。" : "请先启用推理探针。"}</p>}
      {(error || verify) && <div className="quality-warning" role="status"><span>{error || "正在核对检测状态…"}</span><button type="button" className="text-action" disabled={Boolean(busy)} onClick={() => poll.current?.refresh()}><RefreshCw size={13} />重新核对</button></div>}
      <div className={`manual-probe-canvas is-${showAdmission ? "pending" : record?.state || "idle"}`} aria-busy={running || busy === "start"}>
        {showAdmission ? <div className="manual-probe-placeholder" role="status"><Activity size={25} aria-hidden="true" /><strong>{busy === "start" ? "正在提交检测请求" : awaitingStart ? "本次检测结果待核实" : "本次请求未受理"}</strong><p>{awaitingStart ? "确认本次请求状态后展示生成结果。" : "核对原因后，可明确发起一次新检测。"}</p></div> : record?.state === "failed" ? <ProbeFailure message={record.message} code={record.errorCode} httpStatus={record.httpStatus} /> : record?.state === "success" ? <PelicanPreview output={pelicanOutput(record)} /> : <div className="manual-probe-placeholder"><Icon size={25} className={running ? "spin" : undefined} aria-hidden="true" /><strong>{loaded ? labels[record?.state] : "正在读取检测状态"}</strong><p>{record?.message || (running ? "正在生成鹈鹕骑自行车的 HTML 动画，通常约需 3 分钟，最长等待 5 分钟。" : "生成 SVG 绘制的 2D 动画，支持放大预览、查看源码和下载 HTML。通常约需 3 分钟。")}</p></div>}
      </div>
      {previousResult && <div className="manual-probe-previous"><p className="dialog-hint">上次作品（不代表本次检测结果）</p><PelicanPreview output={pelicanOutput(record)} />{record.finishedAt && <div className="manual-probe-caption"><span>上次生成时间</span><time dateTime={record.finishedAt}>{qualityDate(record.finishedAt)}</time></div>}</div>}
      {!showAdmission && record?.finishedAt && <div className="manual-probe-caption"><span>{labels[record.state]}</span><time dateTime={record.finishedAt}>{qualityDate(record.finishedAt)}</time></div>}
    </div>
  </div>;
}
