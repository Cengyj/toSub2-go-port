import { useEffect, useRef, useState } from "react";
import { apiFetch } from "../lib/console.js";
import { createPollingTask } from "../lib/polling.js";

export function validateMaintenanceStatus(value) {
  const count = value => Number.isSafeInteger(value) && value >= 0;
  if (!value || value.schema !== 1 || !count(value.revision) || typeof value.enabled !== "boolean" || typeof value.uncertain !== "boolean"
    || typeof value.drained !== "boolean" || typeof value.recycleRunning !== "boolean" || !value.jobs || typeof value.jobs.drained !== "boolean"
    || !["queued", "running", "queries", "accountChanges"].every(key => count(value.jobs[key]))
    || !["activeRequests", "insights", "probes", "sms"].every(key => count(value[key]))) throw new Error("维护状态响应不完整，请重新核实。");
  return value;
}

export function useMaintenance({ token, enabled = false, active = false }) {
  const identity = useRef(null);
  if (!identity.current || identity.current.token !== token || identity.current.enabled !== enabled) identity.current = { token, enabled, alive: true, status: null, writing: false, reading: false, required: false, version: 0 };
  const session = identity.current, alive = () => identity.current === session && session.alive;
  const [view, setView] = useState({});
  const current = view.session === session ? view : {};
  const controllers = useRef(new Set());
  const publish = update => { if (alive()) setView(old => ({ ...(old.session === session ? old : {}), session, ...update })); };
  function accept(data) {
    const next = validateMaintenanceStatus(data);
    if (session.status && next.revision < session.status.revision) throw new Error("维护状态版本已过期，请重新核实。");
    session.status = next; publish({ status: next, loaded: true, error: next.error || "" });
    return next;
  }
  async function request(options = {}) {
    const controller = new AbortController(); controllers.current.add(controller);
    let timer;
    const deadline = new Promise((_, reject) => { timer = window.setTimeout(() => { controller.abort(); reject(new Error("维护请求超时，请明确读取核实；不会重试修改。")); }, 15000); });
    try { return await Promise.race([apiFetch(token, "/api/maintenance", { ...options, signal: controller.signal, cache: "no-store" }), deadline]); }
    finally { window.clearTimeout(timer); controllers.current.delete(controller); }
  }
  async function reload() {
    if (!alive() || !token || !enabled || session.writing || session.reading) return false;
    session.reading = true; session.version += 1; publish({ loading: true });
    try { const data = await request(); if (!alive()) return false; accept(data); session.required = false; return true; }
    catch (error) { publish({ error: error.message }); return false; }
    finally { session.reading = false; publish({ loading: false }); }
  }
  async function change(nextEnabled) {
    if (!alive() || !token || !enabled || session.writing || session.reading || session.required || !session.status || typeof nextEnabled !== "boolean") return false;
    session.writing = true; session.version += 1; publish({ saving: true, error: "" });
    try {
      const data = await request({ method: "POST", body: JSON.stringify({ enabled: nextEnabled, revision: session.status.revision }) });
      if (!alive()) return false;
      const state = accept(data);
      if (state.uncertain || state.enabled !== nextEnabled) throw new Error("维护修改结果尚未确认，请明确读取核实后再操作。");
      return true;
    } catch (error) {
      if (alive()) { session.required = true; publish({ error: "维护修改未确认，新操作可能仍被暂停；请先读取核实。不会自动重试。" }); }
      return false;
    } finally { session.writing = false; if (alive()) { session.version += 1; publish({ saving: false }); } }
  }
  useEffect(() => { session.alive = true; return () => { session.alive = false; for (const controller of controllers.current) controller.abort(); }; }, [session]);
  useEffect(() => {
    if (!enabled || !token || !active) return;
    const polling = createPollingTask({ intervalMs: 3000,
      request: async signal => { const version = session.version; return { version, data: await apiFetch(token, "/api/maintenance", { signal, cache: "no-store" }) }; },
      onData: ({ version, data }) => { if (alive() && version === session.version && !session.writing && !session.reading) accept(data); },
      onError: error => { if (alive() && !session.writing && !session.reading) publish({ error: error.message }); },
    });
    return polling.stop;
  }, [session, active]);
  return { ...current, enabled, saving: Boolean(current.saving), loading: Boolean(current.loading), verificationRequired: session.required, canChange: Boolean(current.loaded && !session.writing && !session.reading && !session.required), change, reload, isSaving: () => session.writing };
}
