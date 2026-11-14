import { useEffect, useRef, useState } from "react";
import { apiFetch } from "../lib/console.js";
import { createPollingTask } from "../lib/polling.js";
import { DEFAULT_QUALITY_SETTINGS, qualitySettingsPayload, validateQualitySettings, validateQualityList, validateQualityReceipt, qualityCanSubmit } from "../lib/accountQuality.js";

const idsOf = values => [...new Set((values || []).filter(value => typeof value === "string" && value))].sort();
const sameSettings = (a, b) => JSON.stringify(qualitySettingsPayload(a)) === JSON.stringify(qualitySettingsPayload(b));

export function useAccountQuality({ token, enabled = false, ids = [], selectedIds = [], detailId = "", active = false, context = "" }) {
  const identity = useRef({ token, enabled });
  if (identity.current.token !== token || identity.current.enabled !== enabled) identity.current = { token, enabled };
  const session = identity.current, published = useRef(session);
  const idsKey = idsOf([...ids, ...selectedIds, detailId]).join(",");
  const scopeRef = useRef({ session, active, context, idsKey });
  if (scopeRef.current.session !== session || scopeRef.current.active !== active || scopeRef.current.context !== context || scopeRef.current.idsKey !== idsKey) scopeRef.current = { session, active, context, idsKey };
  const scope = scopeRef.current;
  const [settings, setSettings] = useState(() => ({ ...DEFAULT_QUALITY_SETTINGS }));
  const [draft, setDraft] = useState(() => ({ ...DEFAULT_QUALITY_SETTINGS }));
  const [settingsLoaded, setSettingsLoaded] = useState(false), [settingsLoading, setSettingsLoading] = useState(false), [settingsError, setSettingsError] = useState("");
  const [saving, setSaving] = useState(false), [uncertain, setUncertain] = useState(false), [settingsVerificationRequired, setSettingsVerificationRequired] = useState(false);
  const [snapshots, setSnapshots] = useState({}), [status, setStatus] = useState(null), [error, setError] = useState("");
  const [actionBusy, setActionBusy] = useState(""), [verificationRequired, setVerificationRequired] = useState(false);
  const mounted = useRef(true), authority = useRef(null), draftRef = useRef(draft), latest = useRef(null), requests = useRef(new Set());
  const writing = useRef(false), readingSettings = useRef(false), mutation = useRef(false), verify = useRef(false);
  const settingsVerify = useRef(false);
  const listVersion = useRef(0), settingsVersion = useRef(0), poll = useRef(null);
  draftRef.current = draft;
  const alive = () => mounted.current && identity.current === session && session.enabled;
  const visible = () => alive() && scopeRef.current === scope;
  const current = published.current === session;
  const dirty = current && settingsLoaded && !sameSettings(draft, settings);
  latest.current = { settings, settingsLoaded, settingsLoading, verificationRequired, settingsVerificationRequired, uncertain, saving, actionBusy, status, error };

  async function request(path, options = {}, requestScope = null) {
    const controller = new AbortController(), pending = { controller, scope: requestScope };
    let timer;
    const deadline = new Promise((_, reject) => {
      pending.cancel = () => { controller.abort(); const failure = new Error("页面已离开，操作结果需重新核对。"); failure.outcome = "unknown"; reject(failure); };
      timer = window.setTimeout(() => { controller.abort(); const failure = new Error("请求超时，结果需重新核对。"); failure.outcome = "unknown"; reject(failure); }, 15000);
    });
    requests.current.add(pending);
    try { return await Promise.race([apiFetch(token, path, { ...options, signal: controller.signal }), deadline]); }
    finally { requests.current.delete(pending); window.clearTimeout(timer); }
  }
  function requireVerification(value) { verify.current = value; setVerificationRequired(value); }
  // Admission must change before React publishes the next render: a retained
  // save handler must not replay a write whose result is already unknown.
  function requireSettingsVerification(value) { settingsVerify.current = value; setSettingsVerificationRequired(value); }
  function adoptSettings(data, reset = false) {
    const next = validateQualitySettings(data), previous = authority.current;
    if (previous && next.revision < previous.revision) throw new Error("配置响应已过期，请重新读取核实。");
    authority.current = next; setSettings(next); setSettingsLoaded(true);
    if (reset || !previous || sameSettings(draftRef.current, previous)) { draftRef.current = next; setDraft(next); }
    if (next.persistenceWarning) setUncertain(true);
    return next;
  }
  async function reloadSettings() {
    if (!alive() || !token || readingSettings.current || writing.current) return false;
    readingSettings.current = true; const version = ++settingsVersion.current;
    setSettingsLoading(true); setSettingsError("");
    try {
      const data = await request("/api/account-quality/settings", { cache: "no-store" });
      if (!alive() || version !== settingsVersion.current) return false;
      adoptSettings(data, true); requireSettingsVerification(false); return true;
    } catch (failure) { if (alive() && version === settingsVersion.current) { setSettingsError(failure.message); requireSettingsVerification(true); } return false; }
    finally { if (alive() && version === settingsVersion.current) { readingSettings.current = false; setSettingsLoading(false); } }
  }
  function adoptList(data, version) {
    if (!visible() || version !== listVersion.current || mutation.current) return;
    const next = validateQualityList(data);
    setSnapshots(next.snapshots); setStatus(next.status); setError(""); requireVerification(false);
    if (!next.status.settingsAvailable) { requireSettingsVerification(true); setSettingsError(next.status.lastError || "检测设置暂不可用，请重新读取核实。"); }
    // A settings revision changed in another browser must not silently replace
    // this browser's unsaved policy or continue using obsolete eligibility.
    if (authority.current && next.status.settingsRevision > authority.current.revision) { requireSettingsVerification(true); setSettingsError("检测配置已在其他页面更新，请重新读取核对。"); }
  }
  async function reload() {
    if (!visible() || !token || !active || mutation.current) return;
    ++listVersion.current;
    if (poll.current) { poll.current.refresh(); return; }
    const version = listVersion.current;
    try { adoptList(await request(`/api/account-quality?ids=${encodeURIComponent(idsKey)}`, { cache: "no-store" }, scope), version); }
    catch (failure) { if (visible() && version === listVersion.current) setError(failure.message); }
  }
  async function saveSettings() {
    if (!alive() || !token || !authority.current || writing.current || readingSettings.current || mutation.current || settingsVerify.current) return false;
    let payload;
    try { payload = qualitySettingsPayload({ ...draftRef.current, revision: authority.current.revision }); validateQualitySettings(payload); }
    catch (failure) { setSettingsError(failure.message); return false; }
    writing.current = true; ++settingsVersion.current; ++listVersion.current; setSaving(true); setSettingsError("");
    try {
      const data = await request("/api/account-quality/settings", { method: "PUT", body: JSON.stringify(payload) });
      if (!alive()) return false;
      const next = adoptSettings(data, true); setUncertain(Boolean(next.persistenceWarning)); requireSettingsVerification(Boolean(next.persistenceWarning));
      return !next.persistenceWarning;
    } catch (failure) {
      if (alive()) {
        const unknown = failure.outcome === "unknown" || !failure.status || failure.status >= 500 || failure.data?.uncertain === true;
        setUncertain(unknown); requireSettingsVerification(unknown || failure.status === 409);
        setSettingsError(unknown ? "配置保存结果待核实，请重新读取核对后明确保存；未自动重试。" : failure.message);
      }
      return false;
    } finally { if (alive()) { writing.current = false; setSaving(false); void reload(); } }
  }
  function canSubmit(values) {
    return visible() && active && !writing.current && !mutation.current && !verify.current && !settingsVerify.current
      && qualityCanSubmit(latest.current, snapshots, values === null ? null : idsOf(values));
  }
  async function mutate(action, values, options) {
    const selected = values === null ? null : idsOf(values);
    if (!visible() || !active || !token || mutation.current || writing.current || verify.current || action === "refresh" && !canSubmit(selected) || action === "cancel" && !selected?.length) return null;
    mutation.current = true; ++listVersion.current; setActionBusy(action); setError(""); requireVerification(true);
    try {
      const data = await request(`/api/account-quality/${action}`, { method: "POST", body: JSON.stringify({ ...(selected === null ? { all: true } : { ids: selected }), ...(action === "refresh" && options ? { options } : {}) }) }, scope);
      if (!visible()) return null;
      if (action === "refresh") return validateQualityReceipt(data);
      if (!data || !Array.isArray(data.cancelled) || !Array.isArray(data.unchanged)) throw new Error("取消结果尚未确认，请核对检测状态。");
      return data;
    } catch (failure) {
      if (alive()) setError("操作结果待核实，未自动重试。" + failure.message);
      if (visible()) throw failure;
      return null;
    } finally {
      if (alive()) { mutation.current = false; setActionBusy(""); if (visible()) void reload(); }
    }
  }
  useEffect(() => { mounted.current = true; return () => { mounted.current = false; for (const pending of requests.current) pending.cancel(); }; }, []);
  useEffect(() => {
    if (published.current !== session) {
      published.current = session; authority.current = null; writing.current = false; readingSettings.current = false; mutation.current = false; verify.current = false;
      ++listVersion.current; ++settingsVersion.current; draftRef.current = { ...DEFAULT_QUALITY_SETTINGS };
      setSettings({ ...DEFAULT_QUALITY_SETTINGS }); setDraft(draftRef.current); setSettingsLoaded(false); setSettingsLoading(false); setSettingsError(""); requireSettingsVerification(false);
      setSnapshots({}); setStatus(null); setError(""); setActionBusy(""); setVerificationRequired(false); setSaving(false); setUncertain(false);
    }
    if (enabled && token) void reloadSettings();
    return () => { for (const pending of requests.current) pending.cancel(); };
  }, [session]);
  useEffect(() => {
    if (!enabled || !token || !active) return;
    const task = createPollingTask({ intervalMs: () => latest.current.status?.active || latest.current.status?.queued ? 1500 : 5000,
      request: async signal => { const version = listVersion.current; return { data: await apiFetch(token, `/api/account-quality?ids=${encodeURIComponent(idsKey)}`, { signal, cache: "no-store" }), version }; },
      onData: ({ data, version }) => adoptList(data, version),
      onError: failure => { if (visible()) setError(failure.message); },
    });
    poll.current = task;
    return () => { task.stop(); if (poll.current === task) poll.current = null; ++listVersion.current; for (const pending of requests.current) if (pending.scope === scope) pending.cancel(); };
  }, [scope]);
  const resetDraft = () => { if (alive()) { const next = authority.current || { ...DEFAULT_QUALITY_SETTINGS }; draftRef.current = next; setDraft(next); } };
  const updateDraft = update => { if (alive()) setDraft(value => typeof update === "function" ? update(value) : update); };
  return { settings: current ? settings : { ...DEFAULT_QUALITY_SETTINGS }, draft: current ? draft : { ...DEFAULT_QUALITY_SETTINGS }, setDraft: updateDraft, settingsLoaded: current && settingsLoaded, settingsLoading, settingsError, saving, uncertain, settingsVerificationRequired, dirty,
    manualSession: session, modelsAPI: (id, options = {}) => { if (!alive()) return Promise.reject(new Error("当前连接已失效，请重新打开检测。")); return apiFetch(token, `/api/account-quality/models?id=${encodeURIComponent(id)}`, options); }, manualAPI: (suffix = "", options = {}) => { if (!alive()) return Promise.reject(new Error("当前连接已失效，请重新打开检测。")); return apiFetch(token, `/api/account-quality/manual${suffix}`, options); },
    snapshots: current ? snapshots : {}, status: current ? status : null, error, actionBusy, verificationRequired, reloadSettings, saveSettings, resetDraft, reload, canSubmit, submit: (values, options) => mutate("refresh", values, options), submitAll: () => mutate("refresh", null), cancel: values => mutate("cancel", values) };
}
