import { useEffect, useRef, useState } from "react";
import { apiFetch } from "../lib/console.js";
import { createPollingTask } from "../lib/polling.js";

export function validateTrashSettings(data) {
  const value = data?.settings || data;
  if (!value || value.schemaVersion !== 1 || !Number.isSafeInteger(value.revision) || value.revision < 1
    || !Number.isInteger(value.retentionDays) || value.retentionDays < 0 || value.retentionDays > 3650) throw new Error("回收站配置响应不完整，请重新读取核实。");
  const persistenceWarning = typeof value.persistenceWarning === "string" ? value.persistenceWarning : typeof data?.persistenceWarning === "string" ? data.persistenceWarning : "";
  return { schemaVersion: 1, revision: value.revision, retentionDays: value.retentionDays, persistenceWarning };
}
export function validateTrashList(data) {
  const object = value => value && typeof value === "object" && !Array.isArray(value);
  if (!object(data) || !Array.isArray(data.entries) || !object(data.status) || typeof data.status.running !== "boolean"
    || data.entries.some(entry => !object(entry) || typeof entry.id !== "string" || !entry.id || typeof entry.email !== "string"
      || !["trashed", "purging"].includes(entry.state) || typeof entry.deletedAt !== "string" || !Number.isFinite(Date.parse(entry.deletedAt))
      || !Number.isSafeInteger(entry.recordCount) || entry.recordCount < 0 || typeof entry.canRestore !== "boolean"
      || !Array.isArray(entry.jobs) || entry.jobs.some(job => !object(job) || typeof job.id !== "string" || !job.id))
    || new Set(data.entries.map(entry => entry.id)).size !== data.entries.length) throw new Error("回收站列表响应不完整，保留上次读取内容。");
  return { entries: data.entries, status: data.status, settings: validateTrashSettings(data.settings) };
}
export function describeTrashReceipt(action, data, error) {
  const key = { move: "moved", restore: "restored", purge: "purged" }[action];
  const label = { move: "移入回收站", restore: "恢复", purge: "彻底删除" }[action];
  const valid = data && Number.isSafeInteger(data[key]) && data[key] >= 0 && Array.isArray(data.affectedIds)
    && data.affectedIds.every(id => typeof id === "string" && id) && ["completed", "failed", "partial", "unknown"].includes(data.outcome);
  const outcome = valid ? data.outcome : "unknown", count = valid ? data[key] : 0;
  const detail = typeof data?.error === "string" ? data.error : error?.message || "";
  const message = outcome === "completed" ? "已" + label + " " + count + " 个账号。"
    : (count ? "已确认" + label + " " + count + " 个账号。" : "") + (outcome === "unknown" ? "其余操作结果尚未确认" : outcome === "partial" ? "其余账号未全部完成" : "本次操作未完成") + "，请核对当前列表，勿重复提交。" + (detail ? " " + detail : "");
  const purgePending = data?.purgePending === true;
  return { action, outcome, count, affectedIds: valid ? [...new Set(data.affectedIds)] : [], purgePending, message: message + (purgePending ? " 部分回收记录已开始永久清理，暂不可恢复，请核对回收站中的清理状态。" : ""), tone: outcome === "completed" && !purgePending ? "success" : "warning" };
}

export function useAccountTrash({ token, enabled = false, active = false, onMutationStart, onMutation }) {
  const identity = useRef({ token, enabled });
  if (identity.current.token !== token || identity.current.enabled !== enabled) identity.current = { token, enabled };
  const session = identity.current, published = useRef(session), currentSession = published.current === session;
  const [entries, setEntries] = useState([]), [status, setStatus] = useState(null);
  const [loaded, setLoaded] = useState(false), [loading, setLoading] = useState(false), [error, setError] = useState("");
  const [notice, setNotice] = useState(null), [actionBusy, setActionBusy] = useState("");
  const [verificationRequired, setVerificationRequired] = useState(false);
  const [settings, setSettings] = useState(null), [retentionDraft, setRetentionDraft] = useState("0");
  const [settingsLoading, setSettingsLoading] = useState(false), [settingsError, setSettingsError] = useState("");
  const [saving, setSaving] = useState(false), [uncertain, setUncertain] = useState(false);
  const [persistenceUnconfirmed, setPersistenceUnconfirmed] = useState(false);
  const authority = useRef(null), draftRef = useRef(retentionDraft), mounted = useRef(true), calls = useRef(new Set()), listPoll = useRef(null);
  const mutation = useRef(false), settingsWrite = useRef(false), settingsRead = useRef(false), verify = useRef(false);
  const settingsUncertain = useRef(false);
  const listVersion = useRef(0), settingsVersion = useRef(0), latest = useRef(null);
  draftRef.current = retentionDraft;
  latest.current = { token, enabled, onMutationStart, onMutation };
  const dirty = Boolean(settings) && String(settings.retentionDays) !== retentionDraft;
  // Object identity also rejects responses after token A → B → A or a feature
  // disable/re-enable cycle. Token string equality alone cannot isolate them.
  const alive = () => mounted.current && identity.current === session && session.enabled;
  const requireVerification = value => { verify.current = value; setVerificationRequired(value); };
  const requireSettingsVerification = value => { settingsUncertain.current = value; setUncertain(value); };
  async function request(path, options = {}) {
    const controller = new AbortController();
    let timer;
    let cancel;
    const timeout = new Promise((_, reject) => {
      cancel = () => { controller.abort(); const failure = new Error("请求已取消"); failure.name = "AbortError"; reject(failure); };
      timer = window.setTimeout(() => { controller.abort(); const failure = new Error("请求超时，请重新读取核实结果。"); failure.outcome = "unknown"; reject(failure); }, 40000);
    });
    calls.current.add(cancel);
    try { return await Promise.race([apiFetch(token, path, { ...options, signal: controller.signal }), timeout]); }
    finally { window.clearTimeout(timer); calls.current.delete(cancel); }
  }
  function adoptSettings(data, reset = false) {
    const next = validateTrashSettings(data), previous = authority.current;
    if (previous && next.revision < previous.revision) return false;
    authority.current = next; setSettings(next); setPersistenceUnconfirmed(Boolean(next.persistenceWarning));
    if (reset || !previous || draftRef.current === String(previous.retentionDays)) { draftRef.current = String(next.retentionDays); setRetentionDraft(draftRef.current); }
    return true;
  }
  function adoptList(data) {
    const next = validateTrashList(data);
    if (!settingsWrite.current) adoptSettings(next.settings);
    setEntries(next.entries); setStatus(next.status); setLoaded(true); setLoading(false); setError(""); requireVerification(false);
  }
  async function reloadList() {
    if (!alive() || published.current !== session || !token || mutation.current) return;
    listVersion.current += 1;
    if (listPoll.current) { listPoll.current.refresh(); return; }
    const version = listVersion.current;
    setLoading(true);
    try { const data = await request("/api/account-trash", { cache: "no-store" }); if (alive() && version === listVersion.current) adoptList(data); }
    catch (failure) { if (alive() && version === listVersion.current) { setLoading(false); setError(failure.message); } }
  }
  async function reloadSettings() {
    if (!alive() || published.current !== session || !token || settingsRead.current || settingsWrite.current) return;
    settingsRead.current = true;
    const version = ++settingsVersion.current;
    setSettingsLoading(true); setSettingsError("");
    try {
      const data = await request("/api/account-trash/settings", { cache: "no-store" });
      if (alive() && version === settingsVersion.current) {
        if (!adoptSettings(data, true)) throw new Error("配置响应已过期，请重新读取。");
        requireSettingsVerification(false);
      }
    } catch (failure) { if (alive() && version === settingsVersion.current) setSettingsError(failure.message); }
    finally { if (alive()) { settingsRead.current = false; setSettingsLoading(false); } }
  }
  async function saveSettings(expected = {}) {
    if (!alive() || published.current !== session || !token || !authority.current || settingsWrite.current || settingsRead.current || settingsUncertain.current || mutation.current || verify.current) return false;
    if (expected.revision !== undefined && (expected.revision !== authority.current.revision || expected.draft !== draftRef.current)) { setSettingsError("配置或草稿已变化，请核对后重新保存。"); return false; }
    const days = Number(draftRef.current);
    if (!/^[0-9]+$/.test(draftRef.current) || !Number.isInteger(days) || days < 0 || days > 3650) { setSettingsError("保留天数须为 0–3650 的整数；0 表示关闭自动清理。"); return false; }
    settingsWrite.current = true; settingsVersion.current += 1; listVersion.current += 1;
    setSaving(true); setSettingsError("");
    try {
      const data = await request("/api/account-trash/settings", { method: "PUT", body: JSON.stringify({ schemaVersion: 1, revision: authority.current.revision, retentionDays: days }) });
      if (!alive()) return false;
      if (!adoptSettings(data, true)) throw new Error("配置响应已过期，请重新读取核实。");
      requireSettingsVerification(false); return true;
    } catch (failure) {
      if (alive()) {
        const reportedFailure = failure.data?.outcome === "failed";
        const unknown = failure.outcome === "unknown" || failure.data?.uncertain === true || failure.data?.outcome === "unknown" || !reportedFailure && (!failure.status || failure.status >= 500);
        if (failure.data?.settings) { try { adoptSettings(failure.data.settings); } catch { /* Keep the last validated settings; the error stays visible. */ } }
        requireSettingsVerification(unknown || failure.status === 409);
        setSettingsError(failure.status === 409 ? "配置已被其他页面修改，请重新读取后确认更改。" : unknown ? "保存结果尚未确认，请重新读取配置核实，再明确保存。" : failure.message);
      }
      return false;
    } finally { if (alive()) { settingsWrite.current = false; setSaving(false); void reloadList(); } }
  }
  async function mutate(action, ids) {
    if (!alive() || published.current !== session || !token || mutation.current || settingsWrite.current || verify.current || !["move", "restore", "purge"].includes(action)) return null;
    const selected = [...new Set(ids.filter(id => typeof id === "string" && id))];
    if (!selected.length) return null;
    mutation.current = true; listVersion.current += 1; setActionBusy(action); setNotice(null); setError("");
    latest.current.onMutationStart?.();
    let receipt;
    try {
      const data = await request("/api/account-trash/" + action, { method: "POST", body: JSON.stringify({ ids: selected, ...(action === "purge" ? { confirmPermanent: true } : {}) }) });
      receipt = describeTrashReceipt(action, data);
    } catch (failure) { receipt = describeTrashReceipt(action, failure.data, failure); }
    finally {
      if (alive()) {
        mutation.current = false;
        setActionBusy(""); requireVerification(true); setNotice(receipt);
        latest.current.onMutation?.(receipt); void reloadList();
      }
    }
    return receipt;
  }
  useEffect(() => { mounted.current = true; return () => { mounted.current = false; for (const cancel of calls.current) cancel(); }; }, []);
  useEffect(() => {
    if (published.current !== session) {
      published.current = session;
      authority.current = null; draftRef.current = "0";
      mutation.current = false; settingsWrite.current = false; settingsRead.current = false; verify.current = false;
      listVersion.current += 1; settingsVersion.current += 1;
      setEntries([]); setStatus(null); setLoaded(false); setLoading(false); setError("");
      setNotice(null); setActionBusy(""); setVerificationRequired(false);
      setSettings(null); setRetentionDraft("0"); setSettingsLoading(false); setSettingsError("");
      setSaving(false); requireSettingsVerification(false); setPersistenceUnconfirmed(false);
    }
    if (token && enabled) void reloadSettings();
    return () => { for (const cancel of calls.current) cancel(); };
  }, [session]);
  useEffect(() => {
    if (!token || !enabled || !active) return;
    listVersion.current += 1;
    let requestVersion;
    const polling = createPollingTask({
      intervalMs: 5000,
      request: async signal => { requestVersion = listVersion.current; return { version: requestVersion, data: await apiFetch(token, "/api/account-trash", { signal, cache: "no-store" }) }; },
      onStart: () => { if (alive()) setLoading(true); },
      onData: ({ version, data }) => { if (alive() && version === listVersion.current && !mutation.current) adoptList(data); },
      onError: failure => { if (alive() && requestVersion === listVersion.current) { setLoading(false); setError(failure.message); } },
    });
    listPoll.current = polling;
    return () => { polling.stop(); if (listPoll.current === polling) listPoll.current = null; };
  }, [token, enabled, active]);
  const view = currentSession ? { entries, status, loaded, loading, error, notice, actionBusy, verificationRequired, settings, retentionDraft, settingsLoading, settingsError, saving, uncertain, persistenceUnconfirmed, dirty }
    : { entries: [], status: null, loaded: false, loading: false, error: "", notice: null, actionBusy: "", verificationRequired: false, settings: null, retentionDraft: "0", settingsLoading: false, settingsError: "", saving: false, uncertain: false, persistenceUnconfirmed: false, dirty: false };
  return { ...view, setRetentionDraft,
    reloadList, reloadSettings, saveSettings, mutate, clearNotice: () => setNotice(null), resetDraft: () => { setRetentionDraft(String(authority.current?.retentionDays ?? 0)); setSettingsError(""); } };
}
