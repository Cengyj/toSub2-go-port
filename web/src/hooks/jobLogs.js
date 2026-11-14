import { useEffect, useRef, useState } from "react";
import { apiFetch } from "../lib/console.js";
import { createPollingTask } from "../lib/polling.js";
import { createLatestRead } from "../lib/latestRead.js";

export function validateLogPage(data) {
  if (!data || typeof data.logs !== "string" || data.entries != null && !Array.isArray(data.entries)
    || data.hasMore !== undefined && typeof data.hasMore !== "boolean"
    || data.hasMore === true && (typeof data.nextBefore !== "string" || !data.nextBefore)) throw new Error("日志响应不完整，保留已读取内容。");
  return { logs: data.logs, entries: data.entries || [], hasMore: data.hasMore ?? null, nextBefore: data.nextBefore || "", warning: typeof data.warning === "string" ? data.warning : "" };
}
const emptyPage = key => ({ key, logs: "", entries: [], loaded: false, loading: true, loadingEarlier: false, error: "", warning: "", hasMore: null, nextBefore: "", historyLoaded: false, pollingPaused: false, updatedAt: null });

export function useJobLogPages({ token, jobId, archiveId = "", job }) {
  const key = JSON.stringify([token, archiveId, jobId]);
  const [state, setState] = useState(() => emptyPage(key)), [revision, setRevision] = useState(0);
  const current = state.key === key ? state : emptyPage(key);
  const stateRef = useRef(current), jobRef = useRef(job), poll = useRef(null), earlier = useRef(null), olderPending = useRef(false), generation = useRef(0);
  stateRef.current = current; jobRef.current = job;
  const path = archiveId ? "/api/account-trash/" + encodeURIComponent(archiveId) + "/logs?jobId=" + encodeURIComponent(jobId) : "/api/jobs/" + encodeURIComponent(jobId) + "/logs";
  useEffect(() => {
    const version = ++generation.current;
    olderPending.current = false;
    if (stateRef.current.key !== key) stateRef.current = emptyPage(key);
    setState(old => old.key === key ? { ...old, loading: true, loadingEarlier: false, pollingPaused: false } : emptyPage(key));
    const gate = createLatestRead({ onLoading: value => { if (version === generation.current) setState(old => old.key === key ? { ...old, loadingEarlier: value } : old); } });
    earlier.current = gate;
    const onData = data => {
      if (version !== generation.current) return;
      const page = validateLogPage(data);
      const next = { ...emptyPage(key), ...page, loaded: true, loading: false, updatedAt: new Date() };
      stateRef.current = next; setState(next);
    };
    const onError = failure => { if (version === generation.current) setState(old => old.key === key ? { ...old, loading: false, error: failure.message || "日志读取失败" } : old); };
    let stop;
    if (archiveId) {
      const first = createLatestRead({ onLoading: value => { if (version === generation.current) setState(old => old.key === key ? { ...old, loading: value } : old); } });
      first.run(signal => apiFetch(token, path, { signal }), () => version === generation.current).then(onData).catch(failure => { if (failure.name !== "AbortError") onError(failure); });
      stop = first.cancel;
    } else {
      const task = createPollingTask({ intervalMs: () => ["imported", "completed", "succeeded", "failed", "canceled", "interrupted", "resume_available", "reauth_required"].includes(jobRef.current?.operationStatus || jobRef.current?.status) ? 5000 : 1800,
        onStart: () => setState(old => old.key === key ? { ...old, loading: true } : old),
        request: signal => apiFetch(token, path, { signal }), onData, onError });
      poll.current = task; stop = task.stop;
    }
    return () => { generation.current += 1; stop(); gate.cancel(); if (earlier.current === gate) earlier.current = null; poll.current = null; };
  }, [token, jobId, archiveId, revision]);
  async function loadEarlier() {
    const saved = stateRef.current, version = generation.current;
    if (saved.key !== key || !saved.hasMore || !saved.nextBefore || olderPending.current || saved.loading) return;
    olderPending.current = true;
    const pollingPaused = Boolean(poll.current) || saved.pollingPaused;
    poll.current?.stop(); poll.current = null;
    stateRef.current = { ...saved, pollingPaused };
    setState(old => old.key === key ? { ...old, pollingPaused } : old);
    try {
      const data = await earlier.current.run(signal => apiFetch(token, path + (path.includes("?") ? "&" : "?") + "before=" + encodeURIComponent(saved.nextBefore), { signal }), () => generation.current === version);
      if (generation.current !== version) return;
      const page = validateLogPage(data);
      if (page.hasMore && page.nextBefore === saved.nextBefore) throw new Error("日志分页未前进，已停止重复读取。");
      const next = { ...saved, ...page, logs: [page.logs, saved.logs].filter(Boolean).join(String.fromCharCode(10)), entries: [...page.entries, ...saved.entries], warning: [...new Set([page.warning, saved.warning].filter(Boolean))].join("；"), loading: false, loadingEarlier: false, error: "", historyLoaded: true, pollingPaused, updatedAt: new Date() };
      stateRef.current = next; setState(next);
    } catch (failure) { if (generation.current === version && failure.name !== "AbortError") setState(old => ({ ...old, loadingEarlier: false, error: failure.message || "更早日志读取失败" })); }
    finally { if (generation.current === version) olderPending.current = false; }
  }
  function refresh() { generation.current += 1; poll.current?.stop(); earlier.current?.cancel(); setRevision(value => value + 1); }
  return { ...current, loadEarlier, refresh };
}
