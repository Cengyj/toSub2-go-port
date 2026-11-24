import { useEffect, useMemo, useRef, useState } from "react";
import { SMS_PROVIDER_SETTINGS_KEY, LUBAN_API_KEY_STORAGE_KEY, LUBAN_SERVICE_ID_STORAGE_KEY, apiFetch, readSmsProviderSettings, withSmsProviderDefaults, resolveSmsProvider, inspectCustomSmsEntries, formatSmsCountryName, writeLocalJson } from "../lib/console.js";
import { SMS_OPTIONS_TIMEOUT_MS } from "../lib/latestRead.js";
import { useLatestRead } from "./serverState.js";
import { useProtectedIntegrationSettings } from "./protectedIntegrationSettings.js";

// Provider options belong to this editor. Changing the provider, draft or page
// cancels its read. Secure configuration persistence never buys a number.
export function useSmsSettings({ token, features, activePage, settingsTab }) {
  const protection = useProtectedIntegrationSettings({ token, enabled: features.protectedIntegrationSettings === true, scope: "sms", legacyKeys: [SMS_PROVIDER_SETTINGS_KEY, LUBAN_API_KEY_STORAGE_KEY, LUBAN_SERVICE_ID_STORAGE_KEY] });
  const identity = useRef(null);
  if (!identity.current || identity.current.token !== token) identity.current = { token, active: true, submitting: false };
  const session = identity.current, current = () => identity.current === session && session.active;
  const [smsSettings, setSmsSettings] = useState(readSmsProviderSettings);
  const [smsSettingsOpen, setSmsSettingsOpen] = useState(false);
  const [smsSettingsDraft, updateSmsSettingsDraft] = useState(readSmsProviderSettings);
  function setSmsSettingsDraft(value) { if (current()) updateSmsSettingsDraft(value); }
  const [smsSettingsError, setSmsSettingsError] = useState("");
  const [smsNumberOptions, setSmsNumberOptions] = useState([]);
  const [smsOptionsLoading, setSmsOptionsLoading] = useState(false);
  const smsProviderDefinitions = Array.isArray(features.smsProviders) ? features.smsProviders : [];
  const activeSmsProvider = useMemo(
    () => resolveSmsProvider(smsProviderDefinitions, smsSettings),
    [smsProviderDefinitions, smsSettings],
  );
  const draftSmsProvider = useMemo(
    () => resolveSmsProvider(smsProviderDefinitions, smsSettingsDraft),
    [smsProviderDefinitions, smsSettingsDraft],
  );
  const readSmsOptions = useLatestRead(JSON.stringify([token, smsSettingsDraft, smsSettingsOpen, activePage, settingsTab]), setSmsOptionsLoading, SMS_OPTIONS_TIMEOUT_MS);
  useEffect(() => { session.active = true; return () => { session.active = false; }; }, [session]);
  useEffect(() => {
    if (!protection.adopted?.config) return;
    const next = withSmsProviderDefaults(smsProviderDefinitions, protection.adopted.config);
    setSmsSettingsDraft(draft => protection.adopted.explicit || JSON.stringify(withSmsProviderDefaults(smsProviderDefinitions, draft)) === JSON.stringify(withSmsProviderDefaults(smsProviderDefinitions, smsSettings)) ? next : draft);
    setSmsSettings(next); setSmsSettingsError("");
  }, [protection.adopted]);

  function openSmsSettings() {
    if (!current()) return;
    const draft = withSmsProviderDefaults(smsProviderDefinitions, smsSettings);
    setSmsSettingsDraft(draft);
    setSmsSettingsError("");
    setSmsSettingsOpen(true);
  }

  async function loadSmsNumberOptions(settings = smsSettingsDraft) {
    const resolved = resolveSmsProvider(smsProviderDefinitions, settings);
    if (!resolved.definition?.optionsEndpoint) return;
    if (!String(resolved.config.apiKey || "").trim()) {
      setSmsSettingsError("请先填写 API Key");
      return;
    }
    setSmsSettingsError("");
    try {
      const data = await readSmsOptions((signal) => apiFetch(token, resolved.definition.optionsEndpoint, {
        signal,
        readOnly: true,
        method: "POST",
        body: JSON.stringify({ config: resolved.config }),
      }));
      const options = Array.isArray(data.options) ? data.options : [];
      setSmsNumberOptions(options);
      const current = resolved.config.maxPrice
        ? options.find((option) => option.country === resolved.config.country) || options[0]
        : options[0];
      if (!current) throw new Error("当前没有可购买的国家号码");
      updateSmsProviderConfig(resolved.id, {
        country: current.country,
        maxPrice: String(current.price),
        countryLabel: formatSmsCountryName(current),
      });
    } catch (requestError) {
      if (requestError.name === "AbortError") return;
      setSmsNumberOptions([]);
      setSmsSettingsError(requestError.message);
    }
  }

  function updateSmsProviderConfig(providerId, values) {
    setSmsSettingsDraft((current) => ({
      ...current,
      configs: {
        ...(current.configs || {}),
        [providerId]: {
          ...(current.configs?.[providerId] || {}),
          ...values,
        },
      },
    }));
  }

  async function saveSmsSettings(event) {
    event.preventDefault();
    if (!current() || session.submitting || protection.enabled && !protection.canSave) return;
    const resolved = resolveSmsProvider(smsProviderDefinitions, smsSettingsDraft);
    if (!resolved.definition) {
      setSmsSettingsError("请选择接码平台");
      return;
    }
    const missing = resolved.definition.fields.find((field) => (
      field.required !== false && !String(resolved.config[field.key] || "").trim()
    ));
    if (missing) {
      setSmsSettingsError(`请填写${missing.label}`);
      return;
    }
    if (resolved.id === "custom") {
      const customEntries = inspectCustomSmsEntries(resolved.config.entries);
      if (customEntries.error) {
        setSmsSettingsError(customEntries.error);
        return;
      }
    }
    const nextSettings = withSmsProviderDefaults(smsProviderDefinitions, smsSettingsDraft);
    if (protection.enabled) {
      session.submitting = true;
      try {
        if (!await protection.save(nextSettings) || !current()) return;
        setSmsSettings(nextSettings); setSmsSettingsError("");
      } finally { session.submitting = false; }
      return;
    }
    if (!writeLocalJson(SMS_PROVIDER_SETTINGS_KEY, nextSettings)) {
      setSmsSettingsError("浏览器未能保存接码配置，请检查站点存储权限后重试。现有配置未变更。");
      return;
    }
    setSmsSettings(nextSettings);
    setSmsSettingsOpen(false);
    setSmsSettingsError("");
  }

  function clear() {
    const next = withSmsProviderDefaults(smsProviderDefinitions, { selectedProviderId: "luban", configs: {} });
    setSmsSettings(next); setSmsSettingsDraft(next); setSmsSettingsOpen(false); setSmsSettingsError(""); setSmsNumberOptions([]);
  }
  return {
    settings: smsSettings, draft: smsSettingsDraft, provider: { ...activeSmsProvider, ready: activeSmsProvider.ready && protection.ready }, definitions: smsProviderDefinitions,
    isOpen: smsSettingsOpen, open: openSmsSettings, close: () => setSmsSettingsOpen(false), clear,
    saving: protection.saving, isSaving: () => session.submitting || protection.isWriting(), protection,
    dirty: JSON.stringify(withSmsProviderDefaults(smsProviderDefinitions, smsSettingsDraft)) !== JSON.stringify(withSmsProviderDefaults(smsProviderDefinitions, smsSettings)),
    editor: { saveSmsSettings, smsProviderDefinitions, draftSmsProvider, setSmsSettingsDraft, setSmsSettingsError, smsNumberOptions, smsOptionsLoading, updateSmsProviderConfig, loadSmsNumberOptions, smsSettingsError, protection },
  };
}
