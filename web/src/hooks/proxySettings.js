import { useEffect, useRef, useState } from "react";
import { ACCOUNT_PROXY_STORAGE_KEY, readLocalTextSetting, writeLocalTextSetting } from "../lib/console.js";
import { useProtectedIntegrationSettings } from "./protectedIntegrationSettings.js";

export function useProxySettings({ token, protectedStorage = false }) {
  const protection = useProtectedIntegrationSettings({ token, enabled: protectedStorage, scope: "proxy", legacyKeys: [ACCOUNT_PROXY_STORAGE_KEY] });
  const identity = useRef(null);
  if (!identity.current || identity.current.token !== token || identity.current.protectedStorage !== protectedStorage) identity.current = { token, protectedStorage, active: true };
  const session = identity.current, current = () => session.active && identity.current === session;
  const [value, setValue] = useState(() => readLocalTextSetting(ACCOUNT_PROXY_STORAGE_KEY));
  const [draft, updateDraft] = useState(() => readLocalTextSetting(ACCOUNT_PROXY_STORAGE_KEY));
  const draftRef = useRef(draft);
  function setDraft(next) { if (current()) { draftRef.current = next; updateDraft(next); } }
  useEffect(() => { session.active = true; return () => { session.active = false; }; }, [session]);
  useEffect(() => {
    if (!protection.adopted?.config) return;
    const next = typeof protection.adopted.config.url === "string" ? protection.adopted.config.url : "";
    if (protection.adopted.explicit || draftRef.current === value) setDraft(next);
    setValue(next);
  }, [protection.adopted]);
  function apply(next, submitted) {
    if (!current()) return false;
    setValue(next);
    if (draftRef.current === submitted) setDraft(next);
    return true;
  }
  function save(next) {
    if (!current()) return false;
    const submitted = draftRef.current;
    if (!protection.enabled) return writeLocalTextSetting(ACCOUNT_PROXY_STORAGE_KEY, next) && apply(next, submitted);
    if (!protection.canSave) return Promise.resolve(false);
    return protection.save({ ...(protection.snapshot?.config || {}), url: next }).then(result => Boolean(result) && apply(next, submitted));
  }
  function clear() { if (current()) { setValue(""); setDraft(""); } }
  return { value, draft, setDraft, save, clear, ready: protection.ready, protection, saving: protection.saving, isSaving: protection.isWriting };
}
