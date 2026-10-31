import { useEffect, useRef, useState } from "react";
import { apiFetch } from "../lib/console.js";
import { createPollingTask } from "../lib/polling.js";
import { normalizeInsightSettings } from "../lib/accountInsights.js";

// This hook only reads the local service cache. Upstream collection belongs to
// the server scheduler; the browser never derives its interval from settings.
export function useAccountInsights({ token, ids = [], active = false, monitorActive = false }) {
  const [settings, setSettings] = useState(normalizeInsightSettings);
  const [draft, setDraft] = useState(normalizeInsightSettings);
  const [settingsLoading, setSettingsLoading] = useState(true);
  const [settingsLoaded, setSettingsLoaded] = useState(false);
  const [settingsError, setSettingsError] = useState("");
  const [persistenceWarning, setPersistenceWarning] = useState("");
  const [uncertain, setUncertain] = useState(false);
  const [verificationRequired, setVerificationRequired] = useState(false);
  const [saving, setSaving] = useState(false);
  const [snapshots, setSnapshots] = useState({});
  const [summaryError, setSummaryError] = useState("");
  const [status, setStatus] = useState(null);
  const [statusError, setStatusError] = useState("");
  const [actionBusy, setActionBusy] = useState(false);
  const [revision, setRevision] = useState(0);
  const currentSettings = useRef(settings);
  const writePending = useRef(false);
  const readPending = useRef(false);
  const actionPending = useRef(false);
  const configVersion = useRef(0);
  const mounted = useRef(false);
  const requests = useRef(new Map());
  const summaryPoll = useRef(null);
  const statusPoll = useRef(null);
  const idsKey = JSON.stringify([...new Set(ids.filter(id => typeof id === "string" && id))]);
  const dirty = JSON.stringify(settings) !== JSON.stringify(draft);

  function adoptSettings(data, resetDraft = false) {
    const raw = data?.settings || data;
    if (!raw || !Number.isSafeInteger(raw.revision) || raw.revision < 0 || typeof raw.enabled !== "boolean" || typeof raw.automatic !== "boolean" || !Number.isInteger(raw.intervalMinutes) || raw.intervalMinutes < 1 || raw.intervalMinutes > 1440) throw new Error("账号查询配置响应不完整，请重新读取核实。");
    const next = normalizeInsightSettings(raw);
    if (next.revision < currentSettings.current.revision) return false;
    const previous = currentSettings.current;
    currentSettings.current = next;
    setSettings(next);
    setSettingsLoaded(true);
    setDraft(current => resetDraft || JSON.stringify(current) === JSON.stringify(previous) ? next : current);
    const warning = typeof raw.persistenceWarning === "string" ? raw.persistenceWarning : typeof data?.persistenceWarning === "string" ? data.persistenceWarning : "";
    setPersistenceWarning(warning);
    if (warning) setUncertain(true);
    return true;
  }
  useEffect(() => {
    mounted.current = true;
    return () => { mounted.current = false; for (const cancel of requests.current.values()) cancel(); requests.current.clear(); };
  }, []);
  useEffect(() => {
    if (!token) return;
    setSettingsLoading(true);
    let readVersion = configVersion.current;
    const task = createPollingTask({
      intervalMs: 30000,
      request: async signal => {
        const version = readVersion = configVersion.current;
        return { version, data: await apiFetch(token, "/api/account-insights/settings", { signal, cache: "no-store" }) };
      },
      onData: ({ version, data }) => { if (version !== configVersion.current) return; adoptSettings(data); setSettingsLoading(false); setSettingsError(""); },
      onError: error => { if (readVersion !== configVersion.current) return; setSettingsLoading(false); setSettingsError(error.message); },
    });
    return task.stop;
  }, [token]);
  useEffect(() => {
    if (!token || !active || !JSON.parse(idsKey).length) return;
    setSummaryError("");
    const task = createPollingTask({
      intervalMs: 5000,
      request: signal => apiFetch(token, `/api/account-insights?ids=${encodeURIComponent(JSON.parse(idsKey).join(","))}`, { signal, cache: "no-store" }),
      onData: data => { if (!data?.snapshots || typeof data.snapshots !== "object" || Array.isArray(data.snapshots)) throw new Error("账号查询摘要响应不完整，已保留最近一次结果。"); setSnapshots(data.snapshots); setSummaryError(""); },
      onError: error => setSummaryError(error.message),
    });
    summaryPoll.current = task;
    return () => { task.stop(); if (summaryPoll.current === task) summaryPoll.current = null; };
  }, [token, active, idsKey]);
  useEffect(() => {
    if (!token || !monitorActive) return;
    const task = createPollingTask({
      intervalMs: 5000,
      request: signal => apiFetch(token, "/api/account-insights/status", { signal, cache: "no-store" }),
      onData: data => { if (typeof data?.enabled !== "boolean" || typeof data?.automatic !== "boolean") throw new Error("账号查询概览响应不完整，请稍后重试。"); setStatus(data); setStatusError(""); },
      onError: error => setStatusError(error.message),
    });
    statusPoll.current = task;
    return () => { task.stop(); if (statusPoll.current === task) statusPoll.current = null; };
  }, [token, monitorActive]);

  async function request(path, options = {}) {
    const controller = new AbortController();
    let timer;
    const deadline = new Promise((_, reject) => {
      requests.current.set(controller, () => { controller.abort(); const error = new Error("读取已取消"); error.name = "AbortError"; reject(error); });
      timer = window.setTimeout(() => { controller.abort(); reject(new Error("请求超时，结果尚未确认，请重新读取核实。")); }, 15000);
    });
    try { return await Promise.race([apiFetch(token, path, { ...options, signal: controller.signal }), deadline]); }
    finally { window.clearTimeout(timer); requests.current.delete(controller); }
  }
  function refreshCaches() {
    summaryPoll.current?.refresh(); statusPoll.current?.refresh();
    setRevision(value => value + 1);
  }
  async function reloadSettings() {
    if (writePending.current || readPending.current) return;
    readPending.current = true;
    configVersion.current += 1;
    setSettingsLoading(true); setSettingsError("");
    try {
      const data = await request("/api/account-insights/settings", { cache: "no-store" });
      if (!mounted.current) return;
      adoptSettings(data, true); setVerificationRequired(false);
    } catch (error) { if (mounted.current) setSettingsError(error.message); }
    finally { readPending.current = false; if (mounted.current) setSettingsLoading(false); }
  }
  async function saveSettings(value = draft) {
    if (writePending.current || readPending.current || verificationRequired || settingsLoading || !settingsLoaded) return false;
    const minutes = Number(value.intervalMinutes);
    if (!Number.isInteger(minutes) || minutes < 1 || minutes > 1440) { setSettingsError("更新间隔须为 1–1440 的整数分钟。"); return false; }
    writePending.current = true; configVersion.current += 1;
    setSaving(true); setSettingsError("");
    try {
      const data = await request("/api/account-insights/settings", { method: "PUT", body: JSON.stringify({ ...normalizeInsightSettings(value), revision: value.revision }) });
      if (!mounted.current) return false;
      if (!adoptSettings(data, true)) throw new Error("保存响应已过期，请重新读取配置核实。");
      setUncertain(false); setVerificationRequired(false); refreshCaches();
      return true;
    } catch (error) {
      if (mounted.current) {
        const warning = error.data?.settings?.persistenceWarning;
        const reportedFailure = error.data?.outcome === "failed";
        const unknown = error.outcome === "unknown" || error.data?.outcome === "unknown" || error.data?.uncertain === true || !reportedFailure && (!error.status || error.status >= 500 || Boolean(warning));
        if (warning) {
          // A failed disable may still stop collection in memory. Reflect that
          // safe server state without claiming that the disk write succeeded.
          try { adoptSettings(error.data); } catch { setPersistenceWarning(warning); }
        }
        setSettingsError(error.status === 409 ? "配置已被其他页面修改，请重新读取后确认更改。" : unknown ? "保存结果尚未确认，请重新读取配置核对，再明确保存。" : error.message);
        if (unknown) { setUncertain(true); setVerificationRequired(true); }
        if (error.status === 409) setVerificationRequired(true);
      }
      return false;
    } finally { writePending.current = false; if (mounted.current) setSaving(false); }
  }
  async function submitQuery(scope) {
    if (actionPending.current) return null;
    if (!settingsLoaded || settingsLoading || verificationRequired || !currentSettings.current.enabled) throw new Error("请先确认并启用账号查询配置。");
    actionPending.current = true; setActionBusy(true);
    try {
      const result = await request("/api/account-insights/refresh", { method: "POST", body: JSON.stringify(scope) });
      if (mounted.current) refreshCaches();
      return result;
    } finally { actionPending.current = false; if (mounted.current) setActionBusy(false); }
  }
  function submit(ids) {
    return submitQuery({ ids: [...new Set(ids)] });
  }
  function submitAll() {
    // Resolve the complete inventory on the server, independent of this page's
    // cached rows, search filter, selection or automatic-query exclusions.
    return submitQuery({ all: true });
  }
  async function cancel(id) {
    if (actionPending.current) return null;
    actionPending.current = true; setActionBusy(true);
    try {
      const result = await request(`/api/account-insights/queries/${encodeURIComponent(id)}/cancel`, { method: "POST", body: "{}" });
      if (mounted.current) refreshCaches();
      return result;
    } finally { actionPending.current = false; if (mounted.current) setActionBusy(false); }
  }
  async function setAutomatic(id, enabled) {
    const current = currentSettings.current;
    const ids = new Set(current.excludedAccountIds);
    if (enabled) ids.delete(id); else ids.add(id);
    return saveSettings({ ...current, excludedAccountIds: [...ids] });
  }
  return { settings, draft, setDraft, dirty, settingsLoading, settingsLoaded, settingsError, persistenceWarning, uncertain, verificationRequired, saving,
    snapshots, summaryError, status, statusError, actionBusy, revision,
    resetDraft: () => setDraft(currentSettings.current), reloadSettings, saveSettings, submit, submitAll, cancel, setAutomatic };
}
