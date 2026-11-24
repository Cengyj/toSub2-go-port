import { useEffect, useRef, useState } from "react";
import { apiFetch } from "../lib/console.js";

function canonical(value) {
  if (Array.isArray(value)) return value.map(canonical);
  if (value && typeof value === "object") return Object.fromEntries(Object.keys(value).sort().map(key => [key, canonical(value[key])]));
  return value;
}
const equal = (left, right) => JSON.stringify(canonical(left)) === JSON.stringify(canonical(right));

function readLegacyKeys(keys) {
  try { return keys.some(key => window.localStorage.getItem(key) !== null); }
  catch { return true; } // Unreadable storage is never evidence that it is empty.
}

export function validateProtectedSettings(value, scope) {
  if (!value || value.version !== 1 || value.scope !== scope || !Number.isSafeInteger(value.revision) || value.revision < 0
    || value.config !== null && (typeof value.config !== "object" || Array.isArray(value.config))
    || value.credentialPersistence !== "secure" || typeof value.verified !== "boolean" || typeof value.applied !== "boolean") {
    throw new Error("安全配置响应不完整，请重新读取核实。");
  }
  return value;
}

// This is a local persistence boundary, not an integration workflow. It never
// contacts a provider or applies mail/monitor configuration to the process.
export function useProtectedIntegrationSettings({ token, enabled, scope, legacyKeys }) {
  const identity = useRef(null), mounted = useRef(true);
  if (!identity.current || identity.current.token !== token || identity.current.enabled !== enabled || identity.current.scope !== scope) {
    identity.current = { token, enabled, scope, busy: false, writing: false, snapshot: null, error: "", conflict: false, uncertain: false, migrationRequired: readLegacyKeys(legacyKeys), submitted: null, adopted: null, generation: 0 };
  }
  const session = identity.current;
  const [view, setView] = useState({});
  const pending = useRef(new Set());
  const alive = () => mounted.current && identity.current === session;
  const current = view.session === session ? view : {};
  const publish = update => { if (alive()) {
    if (Object.prototype.hasOwnProperty.call(update, "error")) session.error = update.error;
    if (Object.prototype.hasOwnProperty.call(update, "conflict")) session.conflict = update.conflict;
    setView(old => ({ ...(old.session === session ? old : {}), session, ...update }));
  } };
  const isReady = () => alive() && (!enabled || Boolean(session.snapshot && !session.error && !session.conflict && !session.uncertain && !session.migrationRequired && !session.busy));
  const path = `/api/integration-settings/${scope}`;

  async function request(suffix = "", options = {}) {
    const controller = new AbortController();
    let timer;
    const deadline = new Promise((_, reject) => {
      timer = window.setTimeout(() => {
        controller.abort();
        const error = new Error("安全配置请求超时，请读取并核实结果；不会自动重试。");
        error.outcome = "unknown"; reject(error);
      }, 15000);
    });
    pending.current.add(controller);
    try { return validateProtectedSettings(await Promise.race([apiFetch(token, path + suffix, { ...options, signal: controller.signal, cache: "no-store" }), deadline]), scope); }
    finally { window.clearTimeout(timer); pending.current.delete(controller); }
  }
  function accept(snapshot) {
    if (session.snapshot && snapshot.revision < session.snapshot.revision) throw new Error("安全配置版本已过期，请重新读取。");
    session.snapshot = snapshot;
    publish({ loaded: true, snapshot, error: "" });
    return snapshot;
  }
  function cleanupLegacy() {
    try {
      for (const key of legacyKeys) window.localStorage.removeItem(key);
      session.migrationRequired = false;
      return "";
    } catch {
      session.migrationRequired = true;
      return "服务端已确认安全保存，但浏览器旧配置未能全部清除，请检查站点存储权限后再次清理。";
    }
  }
  function adoptConfirmed(snapshot, publishAdoption = true, explicit = false) {
    if (snapshot.verified !== true) throw new Error("服务端尚未确认安全存储读回，已保留浏览器原配置。");
    accept(snapshot);
    const warning = cleanupLegacy();
    session.uncertain = false; session.submitted = null;
    if (publishAdoption) session.adopted = { config: snapshot.config, generation: ++session.generation, applied: snapshot.applied, explicit };
    publish({ adopted: session.adopted, conflict: false, error: warning, notice: warning ? "" : "配置已安全保存并读回确认；浏览器旧秘密已清除。" });
    return snapshot;
  }
  async function reload() {
    if (!alive() || !enabled || !token || session.busy) return null;
    session.busy = true; publish({ loading: true, error: "" });
    try {
      const data = await request();
      if (!alive()) return null;
      accept(data);
      // Fresh browsers can use the current server configuration without
      // modifying it. A browser with an older secret must choose explicitly.
      if (!session.migrationRequired && !session.uncertain && !session.adopted && data.config) {
        session.adopted = { config: data.config, generation: ++session.generation, applied: data.applied, explicit: false };
        publish({ adopted: session.adopted });
      }
      return data;
    } catch (error) { publish({ error: error.message }); return null; }
    finally { session.busy = false; publish({ loading: false }); }
  }
  async function save(config) {
    if (!alive() || !enabled || !token || session.busy || !session.snapshot || session.uncertain) return null;
    session.busy = true; session.writing = true; session.submitted = config; publish({ saving: true, error: "", notice: "" });
    try {
      const data = await request("", { method: "PUT", body: JSON.stringify({ revision: session.snapshot.revision, config }) });
      if (!alive()) return null;
      if (!equal(data.config, config)) throw new Error("安全配置读回与提交内容不同，已保留浏览器原配置。");
      return adoptConfirmed(data, false);
    } catch (error) {
      if (alive()) {
        session.uncertain = error.status === 409 || error.outcome === "unknown" || error.data?.uncertain === true || !error.status || error.status >= 500;
        publish({ conflict: error.status === 409, error: error.status === 409 ? "服务端配置已变化，请读取核实后选择保留哪份配置；不会覆盖其他浏览器的保存。" : error.message });
      }
      return null;
    } finally { session.busy = false; session.writing = false; publish({ saving: false }); }
  }
  function invalidateRevision() {
    if (!alive()) return;
    session.uncertain = true;
    session.submitted = session.snapshot?.config || null;
    publish({ conflict: true, error: "服务端保护配置已被其他操作更新，本次进程应用已拒绝。请读取核实后明确选择配置；不会自动覆盖或重新应用。" });
  }
  async function confirmStored(adoptServer = false) {
    if (!alive() || !enabled || !token || session.busy) return null;
    session.busy = true; session.writing = true; publish({ loading: true, error: "", notice: "" });
    try {
      const snapshot = await request();
      if (!alive()) return null;
      accept(snapshot);
      if (snapshot.config === null) {
        session.uncertain = false; session.submitted = null;
        publish({ error: "服务端尚无此配置，浏览器原配置仍保留；核对后可明确保存。" });
        return null;
      }
      if (!adoptServer && session.submitted && !equal(snapshot.config, session.submitted)) {
        session.uncertain = false;
        publish({ error: "服务端当前配置与上次提交不同，浏览器原配置仍保留。请选择采用服务端配置，或明确保存当前草稿。" });
        return null;
      }
      const data = await request("/confirm", { method: "POST", readOnly: true, body: JSON.stringify({ revision: snapshot.revision }) });
      if (!alive()) return null;
      if (!equal(data.config, snapshot.config) || data.revision !== snapshot.revision) throw new Error("安全配置确认与读取内容不同，浏览器原配置仍保留。");
      return adoptConfirmed(data, true, adoptServer);
    } catch (error) { if (alive()) { session.uncertain = true; publish({ error: error.message }); } return null; }
    finally { session.busy = false; session.writing = false; publish({ loading: false }); }
  }
  useEffect(() => { mounted.current = true; return () => { mounted.current = false; for (const controller of pending.current) controller.abort(); }; }, []);
  useEffect(() => { if (enabled && token) void reload(); return () => { for (const controller of pending.current) controller.abort(); }; }, [session]);
  return {
    ...current, context: session, enabled: Boolean(enabled), loaded: Boolean(current.loaded), saving: Boolean(current.saving), loading: Boolean(current.loading),
    migrationRequired: session.migrationRequired, uncertain: session.uncertain, adopted: current.adopted || session.adopted,
    ready: isReady(),
    canSave: Boolean(enabled && token && session.snapshot && !session.busy && !session.uncertain),
    isBusy: () => session.busy, isWriting: () => session.writing, isReady, save, reload, invalidateRevision, confirm: () => confirmStored(false), adoptServer: () => confirmStored(true),
  };
}
