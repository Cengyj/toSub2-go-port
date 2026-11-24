import { useEffect, useRef, useState } from "react";
import { apiFetch } from "../lib/console.js";
import { createPollingTask } from "../lib/polling.js";
import { normalizeInsightSettings } from "../lib/accountInsights.js";
import { describeInsightRefreshControl, summarizeInsightRefreshTargets, insightRefreshIds, validateInsightRefreshReceipt, validateInsightRefreshStatus } from "../lib/insightRefresh.js";

// This hook only reads the local service cache. Upstream collection belongs to
// the server scheduler; the browser never derives its interval from settings.
export function useAccountInsights({ token, ids = [], selectedIds = [], active = false, monitorActive = false }) {
  const identity = useRef({ token });
  if (identity.current.token !== token) identity.current = { token };
  const session = identity.current, published = useRef(session);
  const [settings, setSettings] = useState(normalizeInsightSettings);
  const [draft, setDraft] = useState(normalizeInsightSettings);
  const [settingsLoading, updateSettingsLoading] = useState(true);
  const [settingsLoaded, updateSettingsLoaded] = useState(false);
  const [settingsError, setSettingsError] = useState("");
  const [persistenceWarning, setPersistenceWarning] = useState("");
  const [uncertain, setUncertain] = useState(false);
  const [verificationRequired, updateVerificationRequired] = useState(false);
  const [saving, setSaving] = useState(false);
  const [snapshots, setSnapshots] = useState({});
  const [summaryError, setSummaryError] = useState("");
  const [status, setStatus] = useState(null);
  const [statusError, setStatusError] = useState("");
  const [statusLoading, setStatusLoading] = useState(true);
  const [actionBusy, setActionBusy] = useState(false);
  const [refreshPending, setRefreshPending] = useState(false);
  const [revision, setRevision] = useState(0);
  const currentSettings = useRef(settings);
  const settingsAuthority = useRef({ loaded: false, loading: true, verificationRequired: false });
  const writePending = useRef(false);
  const readPending = useRef(false);
  const actionPending = useRef(false);
  const configVersion = useRef(0);
  const mounted = useRef(false);
  const requests = useRef(new Map());
  const summaryPoll = useRef(null);
  const statusPoll = useRef(null);
  const statusVersion = useRef(0);
  const statusAuthority = useRef({ data: null, known: false, loading: true, error: "", token: "", selectionKey: "" });
  const refreshOperation = useRef(null);
  const cachedSnapshots = useRef({});
  const currentView = useRef(null);
  const idsKey = JSON.stringify([...new Set(ids.filter(id => typeof id === "string" && id))]);
  const selection = insightRefreshIds(selectedIds);
  const selectionKey = JSON.stringify(selection);
  currentView.current = { token, session, selection, selectionKey };
  const current = published.current === session;
  // A token round trip is a new connection; old handlers and responses must
  // not adopt its settings or release its in-flight mutation guards.
  const alive = () => mounted.current && identity.current === session && published.current === session && Boolean(token);
  const statusActive = active || monitorActive || current && refreshPending;
  const dirty = current && JSON.stringify(settings) !== JSON.stringify(draft);
  function setSettingsLoading(value) { settingsAuthority.current.loading = value; updateSettingsLoading(value); }
  function setSettingsLoaded(value) { settingsAuthority.current.loaded = value; updateSettingsLoaded(value); }
  function setVerificationRequired(value) { settingsAuthority.current.verificationRequired = value; updateVerificationRequired(value); }

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
    if (published.current !== session) {
      published.current = session;
      const defaults = normalizeInsightSettings();
      currentSettings.current = defaults;
      cachedSnapshots.current = {};
      writePending.current = false; readPending.current = false; actionPending.current = false;
      refreshOperation.current = null;
      ++configVersion.current; ++statusVersion.current;
      statusAuthority.current = { data: null, known: false, loading: true, error: "", token: "", selectionKey: "" };
      setSettings(defaults); setDraft(defaults); setSettingsLoaded(false); setSettingsLoading(Boolean(token));
      setSettingsError(""); setPersistenceWarning(""); setUncertain(false); setVerificationRequired(false); setSaving(false);
      setSnapshots({}); setSummaryError(""); setStatus(null); setStatusError(""); setStatusLoading(Boolean(token));
      setActionBusy(false); setRefreshPending(false);
    }
    return () => { for (const cancel of requests.current.values()) cancel(); };
  }, [session]);
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
      onData: ({ version, data }) => { if (!alive() || version !== configVersion.current) return; adoptSettings(data); setSettingsLoading(false); setSettingsError(""); },
      onError: error => { if (!alive() || readVersion !== configVersion.current) return; setSettingsLoading(false); setSettingsError(error.message); },
    });
    return task.stop;
  }, [token]);
  useEffect(() => {
    if (!token || !active || !JSON.parse(idsKey).length) return;
    setSummaryError("");
    const task = createPollingTask({
      intervalMs: 5000,
      request: signal => apiFetch(token, `/api/account-insights?ids=${encodeURIComponent(JSON.parse(idsKey).join(","))}`, { signal, cache: "no-store" }),
      onData: data => { if (!alive()) return; if (!data?.snapshots || typeof data.snapshots !== "object" || Array.isArray(data.snapshots)) throw new Error("账号查询摘要响应不完整，已保留最近一次结果。"); cachedSnapshots.current = data.snapshots; setSnapshots(data.snapshots); setSummaryError(""); },
      onError: error => { if (alive()) setSummaryError(error.message); },
    });
    summaryPoll.current = task;
    return () => { task.stop(); if (summaryPoll.current === task) summaryPoll.current = null; };
  }, [token, active, idsKey]);
  useEffect(() => {
    if (!token || !statusActive) return;
    const view = currentView.current;
    statusVersion.current += 1;
    statusAuthority.current = { ...statusAuthority.current, known: false, loading: true, error: "" };
    setStatusLoading(true); setStatusError("");
    let readVersion = statusVersion.current;
    const task = createPollingTask({
      intervalMs: 5000,
      request: async signal => {
        const version = readVersion = statusVersion.current;
        return { version, data: await apiFetch(view.token, statusURL(view), { signal, cache: "no-store" }) };
      },
      onData: ({ version, data }) => adoptStatus(data, view, version),
      onError: error => rejectStatus(error, view, readVersion),
    });
    statusPoll.current = task;
    const invalidateHiddenStatus = () => {
      if (document.hidden && view.token === currentView.current.token && view.selectionKey === currentView.current.selectionKey) invalidateStatus();
    };
    // A tab can miss admissions made elsewhere while hidden. Retain its data,
    // but require the scheduler's normal resume GET before offering refresh.
    document.addEventListener("visibilitychange", invalidateHiddenStatus);
    return () => {
      document.removeEventListener("visibilitychange", invalidateHiddenStatus);
      task.stop(); if (statusPoll.current === task) statusPoll.current = null;
    };
  }, [token, statusActive, selectionKey]);

  function statusURL(view) {
    return "/api/account-insights/status" + (view.selection.length && view.selection.length <= 1000 ? `?ids=${encodeURIComponent(view.selection.join(","))}` : "");
  }
  function currentStatusRead(view, version) {
    return alive() && view.session === currentView.current.session && version === statusVersion.current && view.token === currentView.current.token && view.selectionKey === currentView.current.selectionKey;
  }
  function adoptStatus(data, view, version) {
    if (!currentStatusRead(view, version)) return;
    validateInsightRefreshStatus(data, view.selection.length > 0 && view.selection.length <= 1000);
    statusAuthority.current = { data, known: true, loading: false, error: "", token: view.token, selectionKey: view.selectionKey };
    setStatus(data); setStatusLoading(false); setStatusError("");
    if (refreshOperation.current?.phase === "checking") {
      refreshOperation.current = null; setRefreshPending(false);
    }
  }
  function rejectStatus(error, view, version) {
    if (!currentStatusRead(view, version)) return;
    // A scope can fail before it has ever produced a successful snapshot. Its
    // error still belongs to this read, rather than the previous/empty scope.
    statusAuthority.current = { ...statusAuthority.current, known: false, loading: false, error: error.message, token: view.token, selectionKey: view.selectionKey };
    setStatusLoading(false); setStatusError(error.message);
  }
  function reloadStatus() {
    const view = currentView.current;
    if (!alive()) return;
    const version = invalidateStatus();
    if (statusPoll.current) { statusPoll.current.refresh(); return; }
    // Explicit verification outside the accounts/monitor views remains a local
    // cache GET. It neither submits work nor starts an independent retry loop.
    void request(statusURL(view), { cache: "no-store" }, view.token)
      .then(data => adoptStatus(data, view, version))
      .catch(error => rejectStatus(error, view, version));
  }
  function invalidateStatus() {
    const version = ++statusVersion.current;
    statusAuthority.current = { ...statusAuthority.current, known: false, loading: true, error: "" };
    setStatusLoading(true); setStatusError("");
    return version;
  }

  async function request(path, options = {}, requestToken = token) {
    const controller = new AbortController();
    let timer;
    const deadline = new Promise((_, reject) => {
      requests.current.set(controller, () => { controller.abort(); const error = new Error("读取已取消"); error.name = "AbortError"; reject(error); });
      timer = window.setTimeout(() => { controller.abort(); reject(new Error("请求超时，结果尚未确认，请重新读取核实。")); }, 15000);
    });
    try { return await Promise.race([apiFetch(requestToken, path, { ...options, signal: controller.signal }), deadline]); }
    finally { window.clearTimeout(timer); requests.current.delete(controller); }
  }
  function refreshCaches() {
    summaryPoll.current?.refresh(); if (statusPoll.current) reloadStatus();
    setRevision(value => value + 1);
  }
  async function reloadSettings() {
    if (!alive() || writePending.current || readPending.current) return;
    readPending.current = true;
    configVersion.current += 1;
    setSettingsLoading(true); setSettingsError("");
    try {
      const data = await request("/api/account-insights/settings", { cache: "no-store" });
      if (!alive()) return;
      if (!adoptSettings(data, true)) throw new Error("配置响应已过期，请重新读取核实。");
      setVerificationRequired(false);
    } catch (error) { if (alive()) setSettingsError(error.message); }
    finally { if (alive()) { readPending.current = false; setSettingsLoading(false); } }
  }
  async function saveSettings(value = draft) {
    if (!alive() || writePending.current || readPending.current || settingsAuthority.current.verificationRequired || settingsAuthority.current.loading || !settingsAuthority.current.loaded) return false;
    const minutes = Number(value.intervalMinutes);
    if (!Number.isInteger(minutes) || minutes < 1 || minutes > 1440) { setSettingsError("更新间隔须为 1–1440 的整数分钟。"); return false; }
    writePending.current = true; configVersion.current += 1;
    setSaving(true); setSettingsError("");
    try {
      const data = await request("/api/account-insights/settings", { method: "PUT", body: JSON.stringify({ ...normalizeInsightSettings(value), revision: value.revision }) });
      if (!alive()) return false;
      if (!adoptSettings(data, true)) throw new Error("保存响应已过期，请重新读取配置核实。");
      const warning = data?.settings?.persistenceWarning || data?.persistenceWarning;
      setUncertain(Boolean(warning)); setVerificationRequired(Boolean(warning)); refreshCaches();
      return !warning;
    } catch (error) {
      if (alive()) {
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
    } finally { if (alive()) { writePending.current = false; setSaving(false); } }
  }
  async function submit(ids) {
    if (!alive() || actionPending.current || refreshOperation.current) return null;
    const requestedIds = insightRefreshIds(ids);
    const control = refreshControl(true, requestedIds);
    if (control.disabled) throw new Error(control.description);
    const operation = { phase: "submitting" };
    refreshOperation.current = operation; statusVersion.current += 1;
    actionPending.current = true; setActionBusy(true); setRefreshPending(true);
    try {
      const result = await request("/api/account-insights/refresh", { method: "POST", body: JSON.stringify({ ids: requestedIds }) });
      return alive() ? validateInsightRefreshReceipt(result) : null;
    } catch (error) {
      if (!alive()) return null;
      throw error;
    } finally {
      if (alive()) {
        actionPending.current = false;
        setActionBusy(false);
        if (refreshOperation.current === operation) {
          operation.phase = "checking";
          refreshCaches();
          // A pending refresh keeps the shared status effect active even if
          // the user navigates away. Do not race it with a second one-off GET.
          if (!statusPoll.current) invalidateStatus();
        }
      }
    }
  }
  async function cancel(id) {
    if (!alive() || actionPending.current) return null;
    actionPending.current = true; setActionBusy(true);
    try {
      const result = await request(`/api/account-insights/queries/${encodeURIComponent(id)}/cancel`, { method: "POST", body: "{}" });
      if (!alive()) return null;
      refreshCaches();
      return result;
    } catch (error) {
      if (!alive()) return null;
      throw error;
    } finally { if (alive()) { actionPending.current = false; setActionBusy(false); } }
  }
  async function setAutomatic(id, enabled) {
    if (!alive()) return false;
    const current = currentSettings.current;
    const ids = new Set(current.excludedAccountIds);
    if (enabled) ids.delete(id); else ids.add(id);
    return saveSettings({ ...current, excludedAccountIds: [...ids] });
  }
  function refreshControl(selected, requestedIds) {
    const view = currentView.current;
    const targetIds = requestedIds || view.selection;
    const authority = statusAuthority.current;
    const currentScope = current && authority.token === view.token && authority.selectionKey === view.selectionKey;
    let known = authority.known && currentScope;
    let aggregate = selected ? authority.data?.selection : authority.data?.refresh;
    if (selected && JSON.stringify(targetIds) !== view.selectionKey) {
      // Row/detail actions may address a different scope from the selection.
      // Use only already-read account snapshots, never another scope's counts.
      const targets = summarizeInsightRefreshTargets(cachedSnapshots.current, targetIds);
      known = known && targets.known;
      aggregate = targets.aggregate;
    }
    return describeInsightRefreshControl({ selected, selectionCount: targetIds.length, token: view.token,
      configured: current && settingsAuthority.current.loaded && currentSettings.current.enabled,
      configurationPending: settingsAuthority.current.loading || settingsAuthority.current.verificationRequired || writePending.current || readPending.current,
      online: typeof navigator === "undefined" || navigator.onLine !== false,
      statusKnown: known, statusLoading: !currentScope || authority.loading, statusError: currentScope ? authority.error : "", aggregate,
      pending: refreshOperation.current?.phase, actionBusy: actionPending.current });
  }
  const view = current ? { settings, draft, dirty, settingsLoading, settingsLoaded, settingsError, persistenceWarning, uncertain, verificationRequired, saving,
    snapshots, summaryError, status, statusError, statusLoading, actionBusy, refreshPending }
    : { settings: normalizeInsightSettings(), draft: normalizeInsightSettings(), dirty: false, settingsLoading: Boolean(token), settingsLoaded: false, settingsError: "", persistenceWarning: "", uncertain: false, verificationRequired: false, saving: false,
      snapshots: {}, summaryError: "", status: null, statusError: "", statusLoading: Boolean(token), actionBusy: false, refreshPending: false };
  return { ...view, setDraft: value => { if (alive()) setDraft(value); }, revision,
    refreshSummary: refreshControl(false), selectedRefreshControl: refreshControl(true), reloadStatus,
    resetDraft: () => { if (alive()) setDraft(currentSettings.current); }, reloadSettings, saveSettings, submit, cancel, setAutomatic };
}
