import { useEffect, useRef, useState } from "react";
import { ACCOUNT_PLAN_OPTIONS, apiFetch, normalizeExcludedPlanTypes } from "../lib/console.js";
import { createPollingTask } from "../lib/polling.js";

const emptyDraft = () => ({ target: "sub2api", enabled: false, excludedPlanTypes: [], intervalMinutes: "5", cooldownMinutes: "5" });
const policyDraft = status => status ? ({ target: status.target, enabled: status.enabled, excludedPlanTypes: [...status.excludedPlanTypes], intervalMinutes: String(status.intervalMinutes), cooldownMinutes: String(status.cooldownMinutes) }) : emptyDraft();
const samePolicy = (left, right) => JSON.stringify(left) === JSON.stringify(right);

export function validatePoolMonitor(data) {
  const target = data?.target;
  const plans = data?.excludedPlanTypes ?? [];
  if (!data || ["enabled", "configured", "running"].some(key => typeof data[key] !== "boolean")
    || !Number.isInteger(data.intervalMinutes) || data.intervalMinutes < 1 || data.intervalMinutes > 1440
    || !Number.isInteger(data.cooldownMinutes) || data.cooldownMinutes < 0 || data.cooldownMinutes > 1440
    || !Array.isArray(plans) || plans.some(plan => !ACCOUNT_PLAN_OPTIONS.some(option => option.value === plan))
    || !["sub2api", "cpa"].includes(target) || !Number.isSafeInteger(data.revision) || data.revision < 0
    || data.targets && ["sub2api", "cpa"].some(value => typeof data.targets[value]?.configured !== "boolean")) throw new Error("监控状态响应不完整，请重新读取核实。");
  const rawResult = data.lastResult ?? data.result ?? null;
  if (rawResult !== null && (typeof rawResult !== "object" || Array.isArray(rawResult))) throw new Error("巡检结果响应不完整，请重新读取核实。");
  const result = rawResult === null ? null : {};
  if (result) {
    const keys = target === "cpa" ? ["checked", "started", "excludedPlan", "busy", "cooldown", "ineligible", "missing", "failed"]
      : ["checked", "started", "updated", "excludedPlan", "autoUploadPaused", "blocked", "ineligible", "missingTask", "missingEmail", "busy", "cooldown", "outsideGroups"];
    for (const key of keys) {
      const count = rawResult[key] ?? 0;
      if (!Number.isSafeInteger(count) || count < 0) throw new Error("巡检统计响应不完整，请重新读取核实。");
      result[key] = count;
    }
    if (target !== "cpa" && typeof rawResult.error === "string") result.error = rawResult.error;
  }
  // Only public monitor fields are adopted; integration keys never enter drafts.
  return { target, enabled: data.enabled, configured: data.configured, running: data.running, revision: data.revision,
    uncertain: data.uncertain === true,
    targets: Object.fromEntries(["sub2api", "cpa"].map(value => [value, { configured: data.targets?.[value]?.configured ?? (target === value && data.configured) }])),
    cpaRevision: Number.isSafeInteger(data.cpaRevision) && data.cpaRevision >= 0 ? data.cpaRevision : undefined,
    intervalMinutes: data.intervalMinutes, cooldownMinutes: data.cooldownMinutes, excludedPlanTypes: normalizeExcludedPlanTypes(plans),
    lastCheckAt: typeof data.lastCheckAt === "string" ? data.lastCheckAt : "", nextCheckAt: typeof data.nextCheckAt === "string" ? data.nextCheckAt : "",
    lastResult: result, lastError: typeof data.lastError === "string" ? data.lastError : "",
    configurationError: typeof data.configurationError === "string" ? data.configurationError : "",
    credentialPersistence: data.credentialPersistence,
    groupIds: Array.isArray(data.groupIds) ? data.groupIds.map(String) : [],
  };
}

export function usePoolMonitor({ token, enabled, active }) {
  const cache = useRef(null), identity = useRef(null), mounted = useRef(true);
  if (!cache.current || cache.current.token !== token) cache.current = { token, status: null, saved: null, draft: emptyDraft(), busy: false, uncertain: false, conflict: false, generation: 0 };
  identity.current = cache.current;
  const session = identity.current;
  const [, setView] = useState({});
  const current = session.view || {};
  const alive = () => mounted.current && identity.current === session;
  const dirty = () => Boolean(session.saved && !samePolicy(session.draft, policyDraft(session.saved)));
  const path = "/api/pool-monitor";
  function publish(values) {
    session.view = { ...session.view, ...values };
    if (alive()) setView({ ...session.view, session });
  }
  function adopt(data, explicit = false) {
    const status = validatePoolMonitor(data);
    if (session.status && status.revision < session.status.revision) throw new Error("监控配置响应已过期，请重新读取。");
    const changed = dirty();
    if (explicit || !session.saved || !changed && !session.uncertain) {
      session.saved = status;
      session.draft = policyDraft(status);
      session.conflict = false;
    } else if (changed && (status.revision !== session.saved.revision || !samePolicy(policyDraft(status), policyDraft(session.saved)))) {
      session.conflict = true;
    }
    session.status = status;
    if (explicit) session.uncertain = false;
    publish({ status, draft: session.draft, loaded: true, conflict: session.conflict, uncertain: session.uncertain,
      ...(!session.uncertain ? { error: "" } : {}), readError: "" });
    return status;
  }
  async function request(suffix = "", options = {}) {
    const controller = new AbortController();
    let timer;
    const deadline = new Promise((_, reject) => { timer = window.setTimeout(() => {
      controller.abort(); const error = new Error("监控请求超时，请读取状态核实。"); error.outcome = "unknown"; reject(error);
    }, options.method ? 130000 : 15000); });
    try { return await Promise.race([apiFetch(token, path + suffix, { ...options, signal: controller.signal }), deadline]); }
    finally { window.clearTimeout(timer); }
  }
  async function read(explicit) {
    if (!alive() || !token || !enabled || session.busy) return false;
    session.busy = true; session.generation++;
    publish({ loading: true, readError: "", notice: "", ...(!session.uncertain ? { error: "" } : {}) });
    try {
      const data = await request("", { cache: "no-store" });
      if (!alive()) return false;
      adopt(data, explicit);
      return true;
    } catch (error) { publish({ readError: error.message }); return false; }
    finally { session.busy = false; publish({ loading: false }); }
  }
  function setDraft(update) {
    if (!alive() || session.busy || !session.saved || session.uncertain || session.conflict) return;
    const next = typeof update === "function" ? update(session.draft) : update;
    session.draft = { target: ["sub2api", "cpa"].includes(next.target) ? next.target : session.draft.target,
      enabled: typeof next.enabled === "boolean" ? next.enabled : session.draft.enabled,
      excludedPlanTypes: normalizeExcludedPlanTypes(next.excludedPlanTypes ?? session.draft.excludedPlanTypes),
      intervalMinutes: String(next.intervalMinutes ?? session.draft.intervalMinutes), cooldownMinutes: String(next.cooldownMinutes ?? session.draft.cooldownMinutes) };
    publish({ draft: session.draft, error: "", notice: "" });
  }
  function resetDraft() {
    if (session.busy) return;
    // Discarding edits does not acknowledge an unknown write or remote check.
    session.draft = policyDraft(session.saved);
    publish({ draft: session.draft, notice: "" });
  }
  async function mutate(check = false, event) {
    event?.preventDefault();
    if (!alive() || !token || !enabled || session.busy || session.uncertain || session.conflict || !session.saved) return false;
    if (check && (dirty() || !session.status?.enabled || !session.status?.configured || session.status?.running || session.status?.uncertain || session.status?.configurationError)) return false;
    if (!check && !dirty() && !session.status?.uncertain && !session.status?.configurationError) return false;
    if (!check && session.draft.enabled && !session.status?.targets?.[session.draft.target]?.configured) { publish({ error: "请先到服务配置填写所选目标的连接地址和管理密钥。" }); return false; }
    let body = { revision: session.saved.revision };
    if (!check) {
      const interval = session.draft.intervalMinutes.trim(), cooldown = session.draft.cooldownMinutes.trim();
      if (!/^\d+$/.test(interval) || Number(interval) < 1 || Number(interval) > 1440
        || !/^\d+$/.test(cooldown) || Number(cooldown) < 0 || Number(cooldown) > 1440) {
        publish({ error: "巡检间隔必须为 1 至 1440 分钟的整数，重登冷却必须为 0 至 1440 分钟的整数；0 表示不限制重登冷却。" }); return false;
      }
      body = { ...body, target: session.draft.target, enabled: session.draft.enabled, excludedPlanTypes: session.draft.excludedPlanTypes,
        intervalMinutes: Number(interval), cooldownMinutes: Number(cooldown) };
    }
    session.busy = true; session.writing = true; session.generation++;
    publish({ [check ? "checking" : "saving"]: true, error: "", notice: "" });
    try {
      const data = await request(check ? "/check" : "", { method: "POST", body: JSON.stringify(body) });
      if (!alive()) return false;
      const status = adopt(data, true);
      publish({ notice: check ? status.running ? "巡检正在进行，状态将自动更新。" : "巡检已完成，重登与上传结果请在账号列表查看。" : "巡检策略已保存。" });
      return true;
    } catch (error) {
      if (alive()) {
        const confirmed = error.data?.outcome === "failed" && error.data?.uncertain === false;
        session.uncertain = !confirmed && (error.outcome === "unknown" || error.data?.uncertain === true || !error.status || error.status >= 500);
        session.conflict = error.status === 409;
        publish({ uncertain: session.uncertain, conflict: session.conflict,
          error: session.uncertain ? `${check ? "巡检" : "保存"}结果待核实，请点击“读取并核实”；不会自动重试。` : error.message });
      }
      return false;
    } finally { session.busy = false; session.writing = false; publish({ checking: false, saving: false }); }
  }
  useEffect(() => { mounted.current = true; return () => { mounted.current = false; }; }, []);
  useEffect(() => {
    if (!token || !enabled || !active) return;
    let readGeneration = session.generation;
    const polling = createPollingTask({ intervalMs: 5000,
      request: async signal => {
        if (session.busy) return null;
        readGeneration = session.generation;
        return { data: await apiFetch(token, path, { signal, cache: "no-store" }), generation: readGeneration };
      },
      onData: result => { if (result && alive() && !session.busy && result.generation === session.generation) {
        try { adopt(result.data); } catch (error) { publish({ readError: error.message }); }
      } },
      onError: error => { if (alive() && !session.busy && readGeneration === session.generation) publish({ readError: error.message }); },
    });
    return polling.stop;
  }, [session, active, enabled]);
  return { ...current, status: current.status || session.status, draft: current.draft || session.draft,
    loaded: Boolean(session.saved), dirty: dirty(), loading: Boolean(current.loading), saving: Boolean(current.saving), checking: Boolean(current.checking),
    uncertain: session.uncertain, conflict: session.conflict,
    repairRequired: Boolean(session.status?.uncertain || session.status?.configurationError),
    error: current.error || current.readError || (session.conflict ? "巡检配置已变化，请读取当前策略后再编辑。" : session.status?.configurationError || ""),
    setDraft, resetDraft, reload: () => read(true), refresh: () => read(false), save: event => mutate(false, event), check: () => mutate(true),
    isSaving: () => Boolean(session.writing),
  };
}
