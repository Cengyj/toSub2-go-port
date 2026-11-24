import { useEffect, useRef, useState } from "react";
import { SUB2API_UPLOAD_SETTINGS_KEY, apiFetch, normalizeSub2ApiSettings, readSub2ApiSettings, withMonitorSettings, writeLocalJson } from "../lib/console.js";
import { SUB2_OPTIONS_TIMEOUT_MS } from "../lib/latestRead.js";
import { useLatestRead, useMonitorPolling } from "./serverState.js";
import { useProtectedIntegrationSettings } from "./protectedIntegrationSettings.js";

// Connection saves own their readback barrier and invalidate passive monitor
// reads before and after a write. Monitor policy is managed independently.
export function useSub2ApiSettings({ token, enabled, activePage, settingsTab, protectedStorage = false }) {
  const protection = useProtectedIntegrationSettings({ token, enabled: protectedStorage, scope: "sub2api", legacyKeys: [SUB2API_UPLOAD_SETTINGS_KEY] });
  const identity = useRef(null);
  if (!identity.current || identity.current.token !== token || identity.current.protectedStorage !== protectedStorage) identity.current = { token, protectedStorage, active: true, write: { current: false }, read: { current: false }, verify: { current: false } };
  const session = identity.current, current = () => identity.current === session && session.active;
  const [appliedCommit, setAppliedCommit] = useState(null);
  const applied = !protectedStorage || appliedCommit?.session === session && appliedCommit.revision === protection.snapshot?.revision && Boolean(protection.snapshot?.config);
  const [sub2apiSettings, setSub2apiSettings] = useState(readSub2ApiSettings);
  const [sub2apiSettingsDraft, updateSub2apiSettingsDraft] = useState(readSub2ApiSettings);
  const sub2apiDraft = useRef(sub2apiSettingsDraft);
  function setSub2apiSettingsDraft(value) {
    if (!current()) return;
    const next = typeof value === "function" ? value(sub2apiDraft.current) : value;
    sub2apiDraft.current = next;
    updateSub2apiSettingsDraft(next);
  }
  const previousSub2apiSettings = useRef(sub2apiSettings);
  const [sub2apiGroups, setSub2apiGroups] = useState([]);
  const [sub2apiProxies, setSub2apiProxies] = useState([]);
  const [sub2apiSettingsOpen, setSub2apiSettingsOpen] = useState(false);
  const [sub2apiSettingsError, setSub2apiSettingsError] = useState("");
  const [sub2apiGroupsLoading, setSub2apiGroupsLoading] = useState(false);
  const [sub2apiSettingsSaving, setSub2apiSettingsSaving] = useState(false);
  const sub2apiSettingsWrite = session.write;
  const [sub2apiSettingsVerificationRequired, setSub2apiSettingsVerificationRequired] = useState(false);
  const [sub2apiSettingsVerifying, setSub2apiSettingsVerifying] = useState(false);
  const sub2apiSettingsVerify = session.verify, sub2apiSettingsRead = session.read;
  function requireSub2ApiVerification(value) { sub2apiSettingsVerify.current = value; if (current()) setSub2apiSettingsVerificationRequired(value); }
  const sub2apiMonitorConfigVersion = useRef(0);
  const [sub2apiMonitorStatus, setSub2apiMonitorStatus] = useState({
    configured: false,
    enabled: false,
    running: false,
    lastCheckAt: null,
    nextCheckAt: null,
    lastError: null,
    lastResult: null,
    intervalMinutes: 5,
  });
  const readSub2Options = useLatestRead(JSON.stringify([token, sub2apiSettingsDraft, sub2apiSettingsOpen, activePage, settingsTab]), setSub2apiGroupsLoading, SUB2_OPTIONS_TIMEOUT_MS);
  const readMonitorSettings = useLatestRead(JSON.stringify([token, sub2apiSettingsDraft, sub2apiSettingsOpen, activePage, settingsTab]), setSub2apiSettingsVerifying);
  const monitorConnectionError = useMonitorPolling({ token, enabled, version: sub2apiMonitorConfigVersion, setStatus: setSub2apiMonitorStatus, setSettings: setSub2apiSettings });
  useEffect(() => { session.active = true; setSub2apiSettingsSaving(false); setSub2apiSettingsVerificationRequired(false); return () => { session.active = false; }; }, [session]);
  useEffect(() => {
    if (!protection.adopted?.config) return;
    const next = normalizeSub2ApiSettings(protection.adopted.config);
    const previous = previousSub2apiSettings.current;
    previousSub2apiSettings.current = next;
    setSub2apiSettings(next);
    setSub2apiSettingsDraft(draft => protection.adopted.explicit || JSON.stringify(draft) === JSON.stringify(previous) ? next : draft);
    setAppliedCommit(null); setSub2apiSettingsError("");
  }, [protection.adopted]);
  useEffect(() => {
    const previous = previousSub2apiSettings.current;
    previousSub2apiSettings.current = sub2apiSettings;
    setSub2apiSettingsDraft((current) => JSON.stringify(current) === JSON.stringify(previous)
      ? sub2apiSettings
      : current);
  }, [sub2apiSettings]);

  function openSub2ApiSettings() {
    if (!current()) return;
    if (sub2apiSettingsVerify.current) { setSub2apiSettingsOpen(true); return; }
    setSub2apiSettingsDraft(withMonitorSettings(sub2apiSettings, sub2apiMonitorStatus));
    setSub2apiSettingsError("");
    setSub2apiSettingsOpen(true);
  }

  async function loadSub2ApiOptions(settings = sub2apiSettingsDraft) {
    setSub2apiSettingsError("");
    try {
      const data = await readSub2Options((signal) => apiFetch(token, "/api/sub2api/options", {
        signal,
        readOnly: true,
        method: "POST",
        body: JSON.stringify({ config: settings }),
      }));
      setSub2apiGroups(Array.isArray(data.groups) ? data.groups : []);
      setSub2apiProxies(Array.isArray(data.proxies) ? data.proxies : []);
    } catch (requestError) {
      if (requestError.name === "AbortError") return;
      setSub2apiGroups([]);
      setSub2apiProxies([]);
      setSub2apiSettingsError(requestError.message);
    }
  }

  async function saveSub2ApiSettings(event) {
    event.preventDefault();
    if (!current() || sub2apiSettingsWrite.current || sub2apiSettingsRead.current || sub2apiSettingsVerify.current || protection.enabled && !protection.canSave) return;
    const baseUrl = String(sub2apiSettingsDraft.baseUrl || "").trim();
    const adminApiKey = String(sub2apiSettingsDraft.adminApiKey || "").trim();
    if (!baseUrl || !/^https?:\/\//i.test(baseUrl)) {
      setSub2apiSettingsError("请输入 http:// 或 https:// 开头的 Sub2API 后端地址");
      return;
    }
    if (!adminApiKey) {
      setSub2apiSettingsError("请输入 Sub2API 管理员 API Key");
      return;
    }
    const nextSettings = {
      ...readSub2ApiSettings(sub2apiSettingsDraft),
      baseUrl: baseUrl.replace(/\/+$/, ""),
      adminApiKey,
    };
    sub2apiSettingsWrite.current = true;
    setSub2apiSettingsSaving(true);
    sub2apiMonitorConfigVersion.current += 1;
    try {
      let protectedRevision;
      if (protection.enabled) {
        const stored = await protection.save(nextSettings);
        if (!stored || !current()) return;
        protectedRevision = stored.revision;
        previousSub2apiSettings.current = nextSettings;
        setSub2apiSettings(nextSettings); setAppliedCommit(null);
      }
      const { monitorEnabled, excludedPlanTypes, ...connectionConfig } = nextSettings;
      const monitor = await apiFetch(token, "/api/sub2api/monitor", {
        method: "POST",
        body: JSON.stringify({ config: connectionConfig, ...(protection.enabled ? { protectedRevision } : {}) }),
      });
      if (!current()) return;
      requireSub2ApiVerification(false);
      setAppliedCommit({ session, revision: protectedRevision });
      const savedSettings = withMonitorSettings(nextSettings, monitor);
      const persisted = protection.enabled || writeLocalJson(SUB2API_UPLOAD_SETTINGS_KEY, savedSettings);
      const draftUnchanged = JSON.stringify(sub2apiDraft.current) === JSON.stringify(sub2apiSettingsDraft);
      // This explicit save settles its draft below. Do not let passive sync
      // mistake a later edit back to the previous settings for a pristine draft.
      previousSub2apiSettings.current = savedSettings;
      setSub2apiSettings(savedSettings);
      if (draftUnchanged) setSub2apiSettingsDraft(savedSettings);
      setSub2apiMonitorStatus(monitor);
      if (!persisted) {
        setSub2apiSettingsError("号池配置已在本地服务生效，但浏览器未能保存上传偏好。请检查站点存储权限并重新保存。");
        return;
      }
      if (!protection.enabled && draftUnchanged) setSub2apiSettingsOpen(false);
      setSub2apiSettingsError("");
    } catch (requestError) {
      if (!current()) return;
      setAppliedCommit(null);
      if (protection.enabled && requestError.data?.code === "SETTINGS_CONFLICT") protection.invalidateRevision();
      const unknown = requestError.outcome === "unknown" || requestError.data?.uncertain === true || requestError.data?.outcome === "unknown" || !requestError.status || requestError.status >= 500;
      if (unknown) requireSub2ApiVerification(true);
      setSub2apiSettingsError(unknown ? "保存结果尚未确认，请先核对本地服务的监控配置，再明确保存；不会自动重试。" : requestError.message);
    } finally {
      sub2apiSettingsWrite.current = false;
      if (current()) { sub2apiMonitorConfigVersion.current += 1; setSub2apiSettingsSaving(false); }
    }
  }

  async function verifySub2ApiSettings() {
    if (!current() || sub2apiSettingsWrite.current || sub2apiSettingsRead.current) return;
    sub2apiSettingsRead.current = true;
    sub2apiMonitorConfigVersion.current += 1;
    try {
      const monitor = await readMonitorSettings(signal => apiFetch(token, "/api/sub2api/monitor", { signal, cache: "no-store" }));
      if (!current()) return;
      if (!monitor || typeof monitor.enabled !== "boolean" || typeof monitor.configured !== "boolean" || typeof monitor.baseUrl !== "string"
        || monitor.groupIds !== null && (!Array.isArray(monitor.groupIds) || !monitor.groupIds.every(value => typeof value === "string" || Number.isSafeInteger(value)))
        || monitor.excludedPlanTypes !== null && (!Array.isArray(monitor.excludedPlanTypes) || !monitor.excludedPlanTypes.every(value => typeof value === "string"))
        || typeof monitor.openaiRequestTimezone !== "string") throw new Error("监控配置响应不完整，请重新核对。");
      const sameBackend = sub2apiSettingsDraft.baseUrl.trim().replace(/\/+$/, "") === monitor.baseUrl.replace(/\/+$/, "");
      const verified = { ...withMonitorSettings(sub2apiSettingsDraft, monitor), baseUrl: monitor.baseUrl, groupIds: (monitor.groupIds || []).map(String), adminApiKey: sameBackend ? sub2apiSettingsDraft.adminApiKey : "" };
      setSub2apiMonitorStatus(monitor);
      setSub2apiSettingsDraft(verified);
      requireSub2ApiVerification(false);
      setSub2apiSettingsError("已读取当前监控配置，持久化仍未确认。请核对配置与管理员 Key 后明确保存。");
    } catch (requestError) {
      if (current() && requestError.name !== "AbortError") setSub2apiSettingsError(requestError.message);
    } finally {
      if (current()) sub2apiMonitorConfigVersion.current += 1;
      sub2apiSettingsRead.current = false;
    }
  }

  async function applySaved() {
    if (!current() || sub2apiSettingsWrite.current || sub2apiSettingsRead.current || sub2apiSettingsVerify.current || !protection.isReady() || !protection.snapshot?.config) return;
    sub2apiSettingsWrite.current = true; setSub2apiSettingsSaving(true); sub2apiMonitorConfigVersion.current += 1;
    try {
      const next = normalizeSub2ApiSettings(protection.snapshot.config);
      const protectedRevision = protection.snapshot.revision;
      const { monitorEnabled, excludedPlanTypes, ...config } = next;
      const monitor = await apiFetch(token, "/api/sub2api/monitor", { method: "POST", body: JSON.stringify({ config, protectedRevision }) });
      if (!current()) return;
      const saved = withMonitorSettings(next, monitor);
      previousSub2apiSettings.current = saved; setSub2apiSettings(saved); setSub2apiMonitorStatus(monitor); setAppliedCommit({ session, revision: protectedRevision }); setSub2apiSettingsError("");
    } catch (error) {
      if (!current()) return;
      setAppliedCommit(null);
      if (error.data?.code === "SETTINGS_CONFLICT") protection.invalidateRevision();
      const unknown = error.outcome === "unknown" || error.data?.uncertain === true || !error.status || error.status >= 500;
      if (unknown) requireSub2ApiVerification(true);
      setSub2apiSettingsError(unknown ? "安全配置仍保留；当前进程应用结果未确认，请先核对保存结果，不会自动重试。" : "安全配置仍保留；当前进程应用失败：" + error.message);
    } finally { sub2apiSettingsWrite.current = false; if (current()) { sub2apiMonitorConfigVersion.current += 1; setSub2apiSettingsSaving(false); } }
  }

  function setSub2ApiGroupChecked(groupId, checked) {
    setSub2apiSettingsDraft((current) => {
      const selected = new Set(current.groupIds);
      if (checked) selected.add(String(groupId));
      else selected.delete(String(groupId));
      return { ...current, groupIds: [...selected] };
    });
  }

  function clear() {
    const next = normalizeSub2ApiSettings({});
    setSub2apiSettings(next); setSub2apiSettingsDraft(next); setSub2apiSettingsOpen(false); setSub2apiSettingsError(""); setSub2apiGroups([]); setSub2apiProxies([]);
  }
  return {
    settings: sub2apiSettings, isOpen: sub2apiSettingsOpen, open: openSub2ApiSettings, close: () => setSub2apiSettingsOpen(false), clear,
    // Passive reads update saved settings before the draft synchronization
    // effect runs. A draft matching that prior baseline is still pristine.
    dirty: JSON.stringify(sub2apiSettingsDraft) !== JSON.stringify(sub2apiSettings) && JSON.stringify(sub2apiSettingsDraft) !== JSON.stringify(previousSub2apiSettings.current),
    saving: sub2apiSettingsSaving || protection.saving, isSaving: () => sub2apiSettingsWrite.current || protection.isWriting(), protection, applied,
    ready: protection.ready && !sub2apiSettingsVerificationRequired, verificationRequired: sub2apiSettingsVerificationRequired, monitorConnectionError,
    editor: { saveSub2ApiSettings, sub2apiSettingsDraft, setSub2apiSettingsDraft, sub2apiGroups, setSub2ApiGroupChecked, sub2apiProxies, sub2apiSettingsError, loadSub2ApiOptions, sub2apiGroupsLoading, token, sub2apiSettingsSaving, sub2apiSettingsVerificationRequired, sub2apiSettingsVerifying, verifySub2ApiSettings, protection, applied, applySaved },
  };
}
