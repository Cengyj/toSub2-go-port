import { useEffect, useRef, useState } from "react";
import { MAIL_REQUEST_SETTINGS_KEY, apiFetch, normalizeMailRequestSettings, readMailRequestSettings, buildMailRequestConfig, writeLocalJson } from "../lib/console.js";
import { useProtectedIntegrationSettings } from "./protectedIntegrationSettings.js";

// Applying a mail configuration to the running process and saving the browser
// preference are distinct outcomes. An in-flight save only settles its draft.
export function useMailSettings({ token, onError, protectedStorage = false }) {
  const protection = useProtectedIntegrationSettings({ token, enabled: protectedStorage, scope: "mail", legacyKeys: [MAIL_REQUEST_SETTINGS_KEY] });
  const identity = useRef(null);
  if (!identity.current || identity.current.token !== token || identity.current.protectedStorage !== protectedStorage) identity.current = { token, protectedStorage, active: true, writing: false };
  const session = identity.current, current = () => identity.current === session && session.active;
  const [appliedCommit, setAppliedCommit] = useState(null);
  const applied = !protectedStorage || appliedCommit?.session === session && appliedCommit.revision === protection.snapshot?.revision && Boolean(protection.snapshot?.config);
  const [mailRequestSettings, setMailRequestSettings] = useState(readMailRequestSettings);
  const [mailRequestSettingsDraft, updateMailRequestSettingsDraft] = useState(readMailRequestSettings);
  const mailRequestDraft = useRef(mailRequestSettingsDraft);
  function setMailRequestSettingsDraft(value) {
    if (!current()) return;
    const next = typeof value === "function" ? value(mailRequestDraft.current) : value;
    mailRequestDraft.current = next;
    updateMailRequestSettingsDraft(next);
  }
  const [mailRequestSettingsOpen, setMailRequestSettingsOpen] = useState(false);
  const [mailRequestSettingsError, setMailRequestSettingsError] = useState("");
  const [mailRequestSettingsSaving, setMailRequestSettingsSaving] = useState(false);
  useEffect(() => { session.active = true; setMailRequestSettingsSaving(false); return () => { session.active = false; }; }, [session]);
  useEffect(() => {
    if (!protection.adopted?.config) return;
    const next = normalizeMailRequestSettings(protection.adopted.config);
    setMailRequestSettingsDraft(draft => protection.adopted.explicit || JSON.stringify(draft) === JSON.stringify(mailRequestSettings) ? next : draft);
    setMailRequestSettings(next); setAppliedCommit(null); setMailRequestSettingsError("");
  }, [protection.adopted]);
  useEffect(() => {
    if (!token || protectedStorage) return;
    try {
      void apiFetch(token, "/api/mail-request-config", {
        method: "POST", body: JSON.stringify({ config: buildMailRequestConfig(mailRequestSettings) }),
      }).catch(requestError => { if (current()) onError(requestError.message); });
    } catch (requestError) { if (current()) onError(requestError.message); }
  }, [session, protectedStorage]);

  function openMailRequestSettings() {
    if (!current()) return;
    setMailRequestSettingsDraft({ ...mailRequestSettings });
    setMailRequestSettingsError("");
    setMailRequestSettingsOpen(true);
  }

  async function saveMailRequestSettings(event) {
    event.preventDefault();
    if (!current() || session.writing || protection.enabled && !protection.canSave) return;
    let config;
    try {
      config = buildMailRequestConfig(mailRequestSettingsDraft);
    } catch (requestError) {
      setMailRequestSettingsError(requestError.message);
      return;
    }
    session.writing = true;
    setMailRequestSettingsSaving(true);
    setMailRequestSettingsError("");
    try {
      const nextSettings = normalizeMailRequestSettings(mailRequestSettingsDraft);
      let protectedRevision;
      if (protection.enabled) {
        const stored = await protection.save(nextSettings);
        if (!stored || !current()) return;
        protectedRevision = stored.revision;
        setMailRequestSettings(nextSettings); setAppliedCommit(null);
      }
      await apiFetch(token, "/api/mail-request-config", {
        method: "POST",
        body: JSON.stringify({ config, ...(protection.enabled ? { protectedRevision } : {}) }),
      });
      if (!current()) return;
      setAppliedCommit({ session, revision: protectedRevision });
      const persisted = protection.enabled || writeLocalJson(MAIL_REQUEST_SETTINGS_KEY, nextSettings);
      setMailRequestSettings(nextSettings);
      if (!persisted) {
        setMailRequestSettingsError("邮件配置已在本地服务生效，但浏览器未能保存。刷新后可能恢复旧配置，请检查站点存储权限并重新保存。");
        return;
      }
      // A successful save confirms only its submitted draft. Keep edits made
      // while it was in flight visible and protected by the dirty-state guard.
      if (!protection.enabled && JSON.stringify(mailRequestDraft.current) === JSON.stringify(mailRequestSettingsDraft)) setMailRequestSettingsOpen(false);
    } catch (requestError) {
      if (current()) {
        setAppliedCommit(null);
        if (protection.enabled && requestError.data?.code === "SETTINGS_CONFLICT") protection.invalidateRevision();
        setMailRequestSettingsError(protection.enabled ? "安全保存与当前进程应用是独立结果。当前应用未确认：" + requestError.message : requestError.message);
      }
    } finally {
      session.writing = false;
      if (current()) setMailRequestSettingsSaving(false);
    }
  }

  async function applySaved() {
    if (!current() || session.writing || !protection.snapshot?.config || !protection.isReady()) return;
    session.writing = true; setMailRequestSettingsSaving(true); setMailRequestSettingsError("");
    try {
      const settings = normalizeMailRequestSettings(protection.snapshot.config);
      const protectedRevision = protection.snapshot.revision;
      await apiFetch(token, "/api/mail-request-config", { method: "POST", body: JSON.stringify({ config: buildMailRequestConfig(settings), protectedRevision }) });
      if (current()) { setMailRequestSettings(settings); setAppliedCommit({ session, revision: protectedRevision }); }
    } catch (error) { if (current()) {
      setAppliedCommit(null);
      if (error.data?.code === "SETTINGS_CONFLICT") protection.invalidateRevision();
      setMailRequestSettingsError("当前进程应用未确认：" + error.message);
    } }
    finally { session.writing = false; if (current()) setMailRequestSettingsSaving(false); }
  }

  function clear() {
    const next = normalizeMailRequestSettings({});
    setMailRequestSettings(next); setMailRequestSettingsDraft(next); setMailRequestSettingsOpen(false); setMailRequestSettingsError("");
  }
  return {
    settings: mailRequestSettings, isOpen: mailRequestSettingsOpen, open: openMailRequestSettings, close: () => setMailRequestSettingsOpen(false), clear,
    dirty: JSON.stringify(mailRequestSettingsDraft) !== JSON.stringify(mailRequestSettings),
    saving: mailRequestSettingsSaving || protection.saving, isSaving: () => session.writing || protection.isWriting(), protection, applied,
    ready: protection.ready && applied,
    editor: { mailRequestSettingsSaving, saveMailRequestSettings, mailRequestSettingsDraft, setMailRequestSettingsDraft, mailRequestSettingsError, token, protection, applied, applySaved },
  };
}
