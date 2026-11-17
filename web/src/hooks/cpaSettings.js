import { useEffect, useRef, useState } from "react";
import { ACCOUNT_PLAN_OPTIONS, apiFetch, normalizeExcludedPlanTypes } from "../lib/console.js";

const validMinutes = (value, minimum = 1) => Number.isInteger(value) && value >= minimum && value <= 1440;
const validPlans = value => Array.isArray(value) && value.every(plan => ACCOUNT_PLAN_OPTIONS.some(option => option.value === plan));

export function validateCPASettings(data) {
  const priority = data?.priority === undefined ? 0 : data.priority, weight = data?.weight === undefined ? 1 : data.weight;
  const monitorEnabled = data?.monitorEnabled === undefined ? false : data.monitorEnabled;
  const excludedPlanTypes = data?.excludedPlanTypes === undefined ? [] : data.excludedPlanTypes;
  const intervalMinutes = data?.intervalMinutes === undefined ? 5 : data.intervalMinutes, cooldownMinutes = data?.cooldownMinutes === undefined ? 5 : data.cooldownMinutes;
  if (!data || typeof data.baseUrl !== "string" || typeof data.configured !== "boolean" || typeof data.hasApiKey !== "boolean"
    || data.configured && (!data.baseUrl || !data.hasApiKey)
    || !Number.isInteger(priority) || priority < -2147483648 || priority > 2147483647
    || !Number.isInteger(weight) || weight < 0 || weight > 1000000
    || typeof monitorEnabled !== "boolean" || !validPlans(excludedPlanTypes) || !validMinutes(intervalMinutes) || !validMinutes(cooldownMinutes, 0)
    || !["secure", "memory_only"].includes(data.credentialPersistence) || !Number.isSafeInteger(data.revision) || data.revision < 0) throw new Error("CPA 配置响应不完整，请重新读取核实。");
  // Copy only public fields: a server regression must never put a returned key
  // into browser state, a form, or browser storage.
  return { baseUrl: data.baseUrl, priority, weight, monitorEnabled, excludedPlanTypes: normalizeExcludedPlanTypes(excludedPlanTypes), intervalMinutes, cooldownMinutes, configured: data.configured, hasApiKey: data.hasApiKey, credentialPersistence: data.credentialPersistence, revision: data.revision, error: typeof data.error === "string" ? data.error : "" };
}

function settingsDraft(settings) {
  return { baseUrl: settings?.baseUrl || "", adminApiKey: "", priority: String(settings?.priority ?? 0), weight: String(settings?.weight ?? 1) };
}

export function useCPASettings({ token, enabled }) {
  const identity = useRef(null), mounted = useRef(true);
  if (!identity.current || identity.current.token !== token || identity.current.enabled !== enabled) identity.current = { token, enabled, settings: null, draft: settingsDraft(), busy: false, uncertain: false };
  const session = identity.current;
  const [view, setView] = useState({});
  const current = view.session === session ? view : {};
  const alive = () => mounted.current && identity.current === session;
  function publish(values) { if (alive()) setView(previous => ({ ...(previous.session === session ? previous : {}), session, ...values })); }
  async function request(options = {}, path = "/api/cpa/settings") {
    const controller = new AbortController();
    let timer;
    const deadline = new Promise((_, reject) => { timer = window.setTimeout(() => { controller.abort(); const error = new Error("CPA 请求超时，请重新读取核实。"); error.outcome = "unknown"; reject(error); }, 15000); });
    try { return await Promise.race([apiFetch(token, path, { ...options, signal: controller.signal }), deadline]); }
    finally { window.clearTimeout(timer); }
  }
  function adopt(data) {
    const settings = validateCPASettings(data);
    if (session.settings && settings.revision < session.settings.revision) throw new Error("CPA 配置响应已过期，请重新读取核实。");
    session.settings = settings;
    session.draft = settingsDraft(settings);
    // A validated read establishes the current revision even when startup
    // could not restore a key. Keep the diagnostic, but allow explicit repair.
    session.uncertain = false;
    publish({ settings, draft: session.draft, loaded: true, uncertain: session.uncertain, error: settings.error });
  }
  async function reload() {
    if (!alive() || !token || !enabled || session.busy) return false;
    session.busy = true; publish({ loading: true, error: "", notice: "" });
    try {
      const data = await request({ cache: "no-store" });
      if (!alive()) return false;
      adopt(data); return true;
    } catch (error) { publish({ error: error.message }); return false; }
    finally { session.busy = false; publish({ loading: false }); }
  }
  function setDraft(update) {
    if (!alive() || session.busy) return;
    const next = typeof update === "function" ? update(session.draft) : update;
    session.draft = { baseUrl: String(next.baseUrl || ""), adminApiKey: String(next.adminApiKey || ""), priority: String(next.priority ?? session.draft.priority), weight: String(next.weight ?? session.draft.weight) };
    publish({ draft: session.draft, notice: "" });
  }
  function resetDraft() {
    session.draft = settingsDraft(session.settings);
    publish({ draft: session.draft, notice: "" });
  }
  async function save(event) {
    event?.preventDefault();
    if (!alive() || !token || !enabled || !session.settings || session.busy || session.uncertain) return false;
    const baseUrl = session.draft.baseUrl.trim().replace(/\/+$/, ""), adminApiKey = session.draft.adminApiKey.trim();
    if (baseUrl && (!/^https?:\/\/[^\s]+$/i.test(baseUrl) || /[?#]/.test(baseUrl))) { publish({ error: "请输入 http:// 或 https:// 开头的 CPA 管理地址，不含查询参数或片段。" }); return false; }
    if (baseUrl && !adminApiKey && (!session.settings.hasApiKey || baseUrl !== session.settings.baseUrl.replace(/\/+$/, ""))) { publish({ error: "首次配置或更换 CPA 地址时，请填写管理密钥。" }); return false; }
    const priorityText = session.draft.priority.trim() || "0", weightText = session.draft.weight.trim() || "1";
    const priority = baseUrl ? Number(priorityText) : 0, weight = baseUrl ? Number(weightText) : 1;
    if (baseUrl && (!/^-?\d+$/.test(priorityText) || !Number.isInteger(priority) || priority < -2147483648 || priority > 2147483647)) { publish({ error: "CPA 优先级必须为 -2147483648 至 2147483647 的整数。" }); return false; }
    if (baseUrl && (!/^\d+$/.test(weightText) || !Number.isInteger(weight) || weight < 0 || weight > 1000000)) { publish({ error: "CPA 权重必须为 0 至 1000000 的整数。" }); return false; }
    session.busy = true; session.saving = true; publish({ saving: true, error: "", notice: "" });
    try {
      const data = await request({ method: "POST", body: JSON.stringify({ baseUrl, adminApiKey: baseUrl ? adminApiKey : "", priority, weight, revision: session.settings.revision }) });
      if (!alive()) return false;
      adopt(data); publish({ notice: baseUrl ? "CPA 配置已保存。" : "CPA 配置已清除。" }); return true;
    } catch (error) {
      if (alive()) {
        const confirmedFailure = error.data?.outcome === "failed" && error.data?.uncertain === false;
        session.uncertain = error.status === 409 || error.outcome === "unknown" || error.data?.uncertain === true || !confirmedFailure && (!error.status || error.status >= 500);
        // Do not retain a submitted secret after an uncertain write. Reading
        // back reports only whether the service has a key, never its value.
        if (session.uncertain) session.draft = { ...session.draft, adminApiKey: "" };
        publish({ draft: session.draft, uncertain: session.uncertain, error: session.uncertain ? "CPA 保存结果待核实，请点击“读取当前配置”核对后再保存；不会自动重试。" : error.message });
      }
      return false;
    } finally { session.busy = false; session.saving = false; publish({ saving: false }); }
  }
  useEffect(() => { mounted.current = true; return () => { mounted.current = false; }; }, []);
  useEffect(() => { if (token && enabled) void reload(); }, [token, enabled]);
  const draft = current.draft || session.draft, settings = current.settings;
  return { ...current, draft, settings, loaded: Boolean(current.loaded), loading: Boolean(current.loading), saving: Boolean(current.saving), uncertain: Boolean(current.uncertain),
    configured: Boolean(current.loaded && settings?.configured && !settings.error && !current.uncertain), dirty: draftChanged(draft, settings),
    setDraft, resetDraft, reload, save, isSaving: () => Boolean(session.saving) };
}

function draftChanged(draft, settings) {
  const saved = settingsDraft(settings);
  return Object.keys(saved).some(key => key === "excludedPlanTypes" ? JSON.stringify(draft[key]) !== JSON.stringify(saved[key]) : draft[key] !== saved[key]);
}
