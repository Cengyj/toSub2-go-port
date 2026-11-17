import React, { useEffect, useMemo, useRef, useState } from "react";
import {
  createRoot,
} from "react-dom/client";
import {
  Ban,
  Check,
  ChevronLeft,
  ChevronRight,
  CircleAlert,
  Download,
  FileText,
  Filter,
  KeyRound,
  ListPlus,
  LoaderCircle,
  LogIn,
  Mail,
  Plus,
  RefreshCw,
  RotateCcw,
  Send,
  ShieldCheck,
  Trash2,
  X,
} from "lucide-react";
import "./styles.css";
import {
  SMS_PROVIDER_SETTINGS_KEY,
  MAIL_REQUEST_SETTINGS_KEY,
  SUB2API_UPLOAD_SETTINGS_KEY,
  ACCOUNT_PROXY_STORAGE_KEY,
  BROWSER_SETTINGS_KEYS,
  saveDownloadResponse,
  localTimestamp,
  apiFetch,
  readResponse,
  countBatchLines,
  parseEmailFilter,
  readLocalTextSetting,
  readSmsProviderSettings,
  normalizeMailRequestSettings,
  readMailRequestSettings,
  buildMailRequestConfig,
  normalizeSub2ApiSettings,
  withMonitorSettings,
  readSub2ApiSettings,
  withSmsProviderDefaults,
  resolveSmsProvider,
  inspectCustomSmsEntries,
  formatSmsCountryName,
  writeLocalJson,
  writeLocalTextSetting,
  mergeJobs,
} from "./lib/console.js";
import { pendingTotpIntent, acknowledgeTotpIntent } from "./lib/totpIntent.js";
import { createPollingTask } from "./lib/polling.js";
import { browserTimeZone } from "./lib/accountExpiration.js";
import { normalizeUploadTarget, readUploadTargetPreference, saveUploadTargetPreference, uploadTargetLabel, uploadTargetConflict, confirmedCPAUploadIds, CPA_AUTHORIZATION_HELP } from "./lib/uploadTarget.js";
import { CPASettings } from "./components/cpa-settings.jsx";
import { useCPASettings } from "./hooks/cpaSettings.js";
import { usePoolMonitor } from "./hooks/poolMonitor.js";
import { SMS_OPTIONS_TIMEOUT_MS, SUB2_OPTIONS_TIMEOUT_MS } from "./lib/latestRead.js";
import { useAccountPolling, useMonitorPolling, useLatestRead } from "./hooks/serverState.js";
import { useAccountInsights } from "./hooks/accountInsights.js";
import { useAccountSubscription } from "./hooks/accountSubscription.js";
import { subscriptionResponseIsCurrent } from "./lib/accountSubscription.js";
import { useAccountQuality } from "./hooks/accountQuality.js";
import { describeQualityReceipt } from "./lib/accountQuality.js";
import { AccountQualitySettings, AccountQualityDialog } from "./components/account-quality.jsx";
import { useNotice } from "./hooks/notice.js";
import { useAccountTrash } from "./hooks/accountTrash.js";
import { useRemovedRowFocus } from "./hooks/removalFocus.js";
import { AccountTrashPage, AccountTrashSettings } from "./components/account-trash.jsx";
import { describeInsightSubmission, insightSnapshotForJob } from "./lib/accountInsights.js";
import { AccountInsightsSettings, AccountInsightsDialog, AccountInsightsMonitor } from "./components/account-insights.jsx";
import {
  EmptyState,
  TotpActionDialog,
  ReloginActionDialog,
  AccountInfoDialog,
  JobRow,
} from "./components/accounts.jsx";
import { JobLogs } from "./components/job-logs.jsx";
import { authorizationMethodLabel, normalizeAuthorizationMethod } from "./components/authorization-method.jsx";
import { uploadBlockedJob, describeUpload, describeBatchSubmission } from "./lib/jobPresentation.js";
import {
  SmsSettingsDialog,
  MailSettingsDialog,
  Sub2ApiSettingsDialog,
} from "./components/SettingsDialogs.jsx";
import {
  AppShell,
  AccountsOverview,
  ConfigurationPage,
  MonitorPage,
  useTheme,
} from "./components/shell.jsx";
import {
  DialogFrame,
  ActionMenu,
  ConfirmationProvider,
  useConfirmation,
} from "./components/ui.jsx";


function App() {
  const confirm = useConfirmation();
  const [activePage, setActivePage] = useState(() => ["accounts", "trash", "settings", "monitor"].includes(location.hash.slice(1)) ? location.hash.slice(1) : "accounts");
  const [settingsTab, setSettingsTab] = useState("overview");
  const [pendingNavigation, setPendingNavigation] = useState(null);
  const [theme, setTheme, themeStorageError] = useTheme();
  const [connected, setConnected] = useState(false);
  const [token, setToken] = useState("");
  const [features, setFeatures] = useState({});
  function selectMonitorTarget(value) {
    if (!["sub2api", "cpa"].includes(value) || poolMonitor.isSaving()) return;
    poolMonitor.setDraft(current => ({ ...current, target: value }));
  }
  const cpa = useCPASettings({ token, enabled: features.cpaUpload === true });
  const anyMonitorAvailable = Boolean(features.sub2apiMonitor || features.cpaUpload);
  const poolMonitor = usePoolMonitor({ token, enabled: anyMonitorAvailable, active: activePage === "monitor" });
  const monitorTarget = poolMonitor.draft?.target || poolMonitor.status?.target || "sub2api";
  const monitorAvailable = monitorTarget === "cpa" ? features.cpaUpload === true : features.sub2apiMonitor === true;
  const [uploadTarget, setUploadTarget] = useState(readUploadTargetPreference);
  const [uploadTargetSaved, setUploadTargetSaved] = useState(true);
  const targetName = uploadTargetLabel(uploadTarget);
  const uploadAvailable = uploadTarget === "cpa" ? features.cpaUpload === true : features.sub2apiUpload === true;
  const anyUploadAvailable = Boolean(features.sub2apiUpload || features.cpaUpload);
  function selectUploadTarget(value) {
    const target = normalizeUploadTarget(value);
    setUploadTarget(target);
    setUploadTargetSaved(saveUploadTargetPreference(target));
  }
  const [jobs, setJobs] = useState([]);
  const accountsHeadingRef = useRef(null);
  const [email, setEmail] = useState("");
  const [busy, setBusy] = useState(false);
  const [batchOpen, setBatchOpen] = useState(false);
  const [batchText, setBatchText] = useState("");
  const [batchBusy, setBatchBusy] = useState(false);
  const batchSubmission = useRef(false);
  const trashMovePending = useRef(false);
  const [batchError, setBatchError] = useState("");
  const [filterOpen, setFilterOpen] = useState(false);
  const [filterText, setFilterText] = useState("");
  const [filterError, setFilterError] = useState("");
  const [emailFilter, setEmailFilter] = useState([]);
  const [error, setActionError] = useState("");
  function setError(message) {
    setActionError(message);
    if (message) clearNotice();
  }
  const [connectionError, setConnectionError] = useState("");
  const [expandedJobId, setExpandedJobId] = useState(null);
  const [selectedJobIds, setSelectedJobIds] = useState(() => new Set());
  const [jobSelectionIndex, setJobSelectionIndex] = useState([]);
  const [batchAction, publishBatchAction] = useState("");
  const batchActionRef = useRef("");
  function setBatchAction(action) { batchActionRef.current = action; publishBatchAction(action); }
  const [totpAction, setTotpAction] = useState(null);
  const totpSubmission = useRef(false);
  const [reloginAction, setReloginAction] = useState(null);
  const reloginSubmission = useRef(false);
  const [accountInfoJob, setAccountInfoJob] = useState(null);
  const [insightJob, setInsightJob] = useState(null);
  const [qualityJob, setQualityJob] = useState(null);
  const [page, setPage] = useState(1);
  const [pagination, setPagination] = useState({ page: 1, pageSize: 20, total: 0, totalPages: 1 });
  const [stats, setStats] = useState({ active: 0, queued: 0, completed: 0 });
  const [smsSettings, setSmsSettings] = useState(readSmsProviderSettings);
  const [smsSettingsOpen, setSmsSettingsOpen] = useState(false);
  const [smsSettingsDraft, setSmsSettingsDraft] = useState(readSmsProviderSettings);
  const [smsSettingsError, setSmsSettingsError] = useState("");
  const [smsNumberOptions, setSmsNumberOptions] = useState([]);
  const [smsOptionsLoading, setSmsOptionsLoading] = useState(false);
  const [mailRequestSettings, setMailRequestSettings] = useState(readMailRequestSettings);
  const [mailRequestSettingsDraft, updateMailRequestSettingsDraft] = useState(readMailRequestSettings);
  const mailRequestDraft = useRef(mailRequestSettingsDraft);
  function setMailRequestSettingsDraft(value) {
    const next = typeof value === "function" ? value(mailRequestDraft.current) : value;
    mailRequestDraft.current = next;
    updateMailRequestSettingsDraft(next);
  }
  const [mailRequestSettingsOpen, setMailRequestSettingsOpen] = useState(false);
  const [mailRequestSettingsError, setMailRequestSettingsError] = useState("");
  const [mailRequestSettingsSaving, setMailRequestSettingsSaving] = useState(false);
  const mailSettingsWrite = useRef(false);
  const [sub2apiSettings, setSub2apiSettings] = useState(readSub2ApiSettings);
  const [sub2apiSettingsDraft, updateSub2apiSettingsDraft] = useState(readSub2ApiSettings);
  const sub2apiDraft = useRef(sub2apiSettingsDraft);
  function setSub2apiSettingsDraft(value) {
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
  const sub2apiSettingsWrite = useRef(false);
  const [sub2apiSettingsVerificationRequired, setSub2apiSettingsVerificationRequired] = useState(false);
  const [sub2apiSettingsVerifying, setSub2apiSettingsVerifying] = useState(false);
  const sub2apiSettingsVerify = useRef(false), sub2apiSettingsRead = useRef(false);
  function requireSub2ApiVerification(value) { sub2apiSettingsVerify.current = value; setSub2apiSettingsVerificationRequired(value); }
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
  const { message: uploadNotice, tone: noticeTone, showNotice, clearNotice } = useNotice();
  const [accountProxyUrl, setAccountProxyUrl] = useState(() => readLocalTextSetting(ACCOUNT_PROXY_STORAGE_KEY));
  const [proxyDraft, setProxyDraft] = useState(() => readLocalTextSetting(ACCOUNT_PROXY_STORAGE_KEY));
  const [browserSettingsError, setBrowserSettingsError] = useState("");
  const readSmsOptions = useLatestRead(JSON.stringify([token, smsSettingsDraft, smsSettingsOpen, activePage, settingsTab]), setSmsOptionsLoading, SMS_OPTIONS_TIMEOUT_MS);
  const readSub2Options = useLatestRead(JSON.stringify([token, sub2apiSettingsDraft, sub2apiSettingsOpen, activePage, settingsTab]), setSub2apiGroupsLoading, SUB2_OPTIONS_TIMEOUT_MS);
  const readMonitorSettings = useLatestRead(JSON.stringify([token, sub2apiSettingsDraft, sub2apiSettingsOpen, activePage, settingsTab]), setSub2apiSettingsVerifying);

  useEffect(() => {
    const previous = previousSub2apiSettings.current;
    previousSub2apiSettings.current = sub2apiSettings;
    setSub2apiSettingsDraft((current) => JSON.stringify(current) === JSON.stringify(previous)
      ? sub2apiSettings
      : current);
  }, [sub2apiSettings]);

  async function clearBrowserSettings() {
    if (!await confirm("请先备份需要保留的配置。确定清除当前浏览器保存的接码平台、邮件 API、Sub2API、代理、主题和账号操作偏好吗？服务端任务及号池监控配置需通过本地服务清理操作删除。")) return;
    try {
      for (const key of BROWSER_SETTINGS_KEYS) window.localStorage.removeItem(key);
    } catch {
      setBrowserSettingsError("网页配置未能全部清除，请检查浏览器的站点存储权限后重试。");
      return;
    }
    const sms = withSmsProviderDefaults(smsProviderDefinitions, { selectedProviderId: "luban", configs: {} });
    const mail = normalizeMailRequestSettings({});
    const sub2api = normalizeSub2ApiSettings({});
    setSmsSettings(sms);
    setSmsSettingsDraft(sms);
    setSmsSettingsOpen(false);
    setSmsSettingsError("");
    setSmsNumberOptions([]);
    setMailRequestSettings(mail);
    setMailRequestSettingsDraft(mail);
    setMailRequestSettingsOpen(false);
    setMailRequestSettingsError("");
    setSub2apiSettings(sub2api);
    setUploadTarget("sub2api");
    setUploadTargetSaved(true);
    setSub2apiSettingsDraft(sub2api);
    setSub2apiSettingsOpen(false);
    setSub2apiSettingsError("");
    setSub2apiGroups([]);
    setSub2apiProxies([]);
    setAccountProxyUrl("");
    setProxyDraft("");
    setTheme("light", false);
    setEmail("");
    setBatchText("");
    setBatchOpen(false);
    setBatchError("");
    setTotpAction(null);
    setReloginAction(null);
    setAccountInfoJob(null);
    setExpandedJobId(null);
    setBrowserSettingsError("");
    showNotice("已清除当前浏览器保存的网页配置及账号操作偏好。服务端数据请通过本地服务清理操作删除。", "success");
  }

  useEffect(() => {
    const polling = createPollingTask({
      intervalMs: 2000,
      request: (signal) => fetch("/api/bootstrap", { signal }).then(readResponse),
      onData: (data) => {
        setToken(data.token);
        setFeatures(data.features || {});
        setConnectionError("");
        polling.stop();
      },
      onError: (error) => setConnectionError(error.message),
    });
    return polling.stop;
  }, []);

  useEffect(() => {
    if (!token) return;
    void apiFetch(token, "/api/mail-request-config", {
      method: "POST",
      body: JSON.stringify({ config: buildMailRequestConfig(mailRequestSettings) }),
    }).catch((requestError) => setError(requestError.message));
  }, [token]);

  const refreshAccounts = useAccountPolling({ token, page, emailFilter, setJobs, setJobSelectionIndex, setPagination, setStats, setConnected, setPage, setConnectionError });
  const trash = useAccountTrash({ token, enabled: features.accountTrash === true, active: activePage === "trash",
    onMutationStart: () => refreshAccounts.invalidate?.(),
    onMutation: receipt => {
      if (receipt?.action === "move" && receipt.affectedIds.length) {
        const affected = new Set(receipt.affectedIds);
        setJobs(current => current.filter(job => !affected.has(job.id)));
        setJobSelectionIndex(current => current.filter(job => !affected.has(job.id)));
        setSelectedJobIds(current => new Set([...current].filter(id => !affected.has(id))));
        setExpandedJobId(current => affected.has(current) ? null : current);
        setAccountInfoJob(current => current && affected.has(current.id) ? null : current);
        setInsightJob(current => current && affected.has(current.id) ? null : current);
        setQualityJob(current => current && affected.has(current.id) ? null : current);
      }
      refreshAccounts();
    },
  });
  const removalFocus = useRemovedRowFocus(jobs, accountsHeadingRef, Boolean(batchAction) || Boolean(trash.actionBusy) || trash.verificationRequired);
  const monitorConnectionError = useMonitorPolling({ token, enabled: features.sub2apiMonitor, version: sub2apiMonitorConfigVersion, setStatus: setSub2apiMonitorStatus, setSettings: setSub2apiSettings });
  const insights = useAccountInsights({ token, ids: jobs.map(job => job.id), selectedIds: [...selectedJobIds], active: activePage === "accounts", monitorActive: activePage === "monitor" });
  const subscriptions = useAccountSubscription({ token, enabled: features.accountSubscription === true, jobs, onVerify() { refreshAccounts.invalidate?.(); refreshAccounts(); }, onUpdated(updated) {
    refreshAccounts.invalidate?.();
    const adopt = current => current?.id === updated.id && subscriptionResponseIsCurrent(updated, current, current) ? updated : current;
    setJobs(current => current.map(adopt));
    setJobSelectionIndex(current => current.map(adopt));
    setInsightJob(adopt);
    setAccountInfoJob(adopt);
    refreshAccounts();
  } });
  const quality = useAccountQuality({ token, enabled: features.accountQuality === true, ids: jobs.map(job => job.id), selectedIds: [...selectedJobIds], detailId: qualityJob?.id, active: activePage === "accounts", context: activePage });
  async function checkQuality(ids = null, options) {
    setError("");
    try {
      const data = ids === null ? await quality.submitAll() : await quality.submit(ids, options);
      if (data) { const notice = describeQualityReceipt(data); showNotice(notice.text, notice.tone, notice); }
    } catch (requestError) { setError(requestError.message); }
  }
  async function refreshInsights(ids) {
    setError("");
    try {
      const data = await insights.submit(ids);
      if (data) { const notice = describeInsightSubmission(data); showNotice(notice.text, notice.tone, notice); }
    } catch (requestError) { setError(requestError.message); }
  }

  const pageJobIds = useMemo(() => jobs.map((job) => job.id), [jobs]);
  const smsProviderDefinitions = Array.isArray(features.smsProviders) ? features.smsProviders : [];
  const activeSmsProvider = useMemo(
    () => resolveSmsProvider(smsProviderDefinitions, smsSettings),
    [smsProviderDefinitions, smsSettings],
  );
  const draftSmsProvider = useMemo(
    () => resolveSmsProvider(smsProviderDefinitions, smsSettingsDraft),
    [smsProviderDefinitions, smsSettingsDraft],
  );
  const selectedJobs = useMemo(
    () => jobSelectionIndex.filter((job) => selectedJobIds.has(job.id)),
    [jobSelectionIndex, selectedJobIds],
  );
  const downloadableSelectedCount = selectedJobs.filter((job) => job.canDownload).length;
  const selectedAuthorizationMethod = selectedJobs.length && selectedJobs.every((job) => normalizeAuthorizationMethod(job.authorizationMethod) === normalizeAuthorizationMethod(selectedJobs[0].authorizationMethod)) ? normalizeAuthorizationMethod(selectedJobs[0].authorizationMethod) : "chatgpt";
  const allPageSelected = pageJobIds.length > 0 && pageJobIds.every((id) => selectedJobIds.has(id));
  const canDownloadSelected = selectedJobs.length > 0 && selectedJobs.length === selectedJobIds.size
    && downloadableSelectedCount > 0 && selectedJobs.every((job) => !job.totpExportBlocked);
  const canReauthorizeSelected = selectedJobs.length > 0 && selectedJobs.length === selectedJobIds.size
    && selectedJobs.every((job) => job.canRegenerate || job.canRetry);
  const hasImportedSelected = selectedJobs.some((job) => job.status === "imported");
  const forceReloginSelectedCount = selectedJobs.filter((job) => job.canForceRelogin).length;
  const canForceReloginSelected = selectedJobs.length > 0 && selectedJobs.length === selectedJobIds.size
    && forceReloginSelectedCount > 0;
  const uploadBlockedSelected = selectedJobs.some(uploadBlockedJob);
  const canUploadSelected = selectedJobs.length > 0 && selectedJobs.length === selectedJobIds.size
    && downloadableSelectedCount > 0 && !uploadBlockedSelected;
  const totpSetupSelectedCount = selectedJobs.filter((job) => job.canSetupTotp).length;
  const canSetupTotpSelected = selectedJobs.length > 0 && selectedJobs.length === selectedJobIds.size
    && totpSetupSelectedCount > 0;
  const totpResetSelectedCount = selectedJobs.filter((job) => job.canResetTotp).length;
  const canResetTotpSelected = selectedJobs.length > 0 && selectedJobs.length === selectedJobIds.size
    && totpResetSelectedCount > 0;
  const passwordAddSelectedCount = selectedJobs.filter((job) => job.canAddPassword).length;
  const canAddPasswordSelected = selectedJobs.length > 0 && selectedJobs.length === selectedJobIds.size
    && passwordAddSelectedCount > 0;
  const uploadConfigured = uploadTarget === "cpa" ? cpa.configured : Boolean(sub2apiSettings.baseUrl.trim() && sub2apiSettings.adminApiKey.trim());
  const uploadTargets = { sub2api: { available: Boolean(features.sub2apiUpload), configured: Boolean(sub2apiSettings.baseUrl.trim() && sub2apiSettings.adminApiKey.trim()) }, cpa: { available: features.cpaUpload === true, configured: cpa.configured } };

  const settingsSection = smsSettingsOpen ? "sms" : mailRequestSettingsOpen ? "mail" : sub2apiSettingsOpen ? "sub2api" : settingsTab;
  // Only writes block navigation. Leaving a read-only editor cancels its scoped read.
  const settingsSaving = mailRequestSettingsSaving || sub2apiSettingsSaving || insights.saving || trash.saving || quality.saving || cpa.saving || poolMonitor.saving || poolMonitor.checking;
  const settingsDirty = (smsSettingsOpen && JSON.stringify(withSmsProviderDefaults(smsProviderDefinitions, smsSettingsDraft)) !== JSON.stringify(withSmsProviderDefaults(smsProviderDefinitions, smsSettings)))
    || (mailRequestSettingsOpen && JSON.stringify(mailRequestSettingsDraft) !== JSON.stringify(mailRequestSettings))
    || (sub2apiSettingsOpen && JSON.stringify(sub2apiSettingsDraft) !== JSON.stringify(sub2apiSettings))
    || (activePage === "settings" && settingsTab === "network" && proxyDraft !== accountProxyUrl)
    || (activePage === "settings" && settingsTab === "insights" && insights.dirty)
    || (activePage === "settings" && settingsTab === "trash" && trash.dirty)
    || (activePage === "settings" && settingsTab === "quality" && quality.dirty)
    || (activePage === "settings" && settingsTab === "cpa" && cpa.dirty)
    || (activePage === "monitor" && poolMonitor.dirty);

  function leaveSettings(action) {
    if (settingsSaving || mailSettingsWrite.current || sub2apiSettingsWrite.current || cpa.isSaving() || poolMonitor.isSaving()) return false;
    if (settingsDirty) {
      setPendingNavigation(() => action);
      return false;
    }
    action();
    return true;
  }
  function resetSettingsEditors() {
    insights.resetDraft();
    quality.resetDraft();
    trash.resetDraft();
    cpa.resetDraft();
    poolMonitor.resetDraft();
    setSmsSettingsOpen(false);
    setMailRequestSettingsOpen(false);
    setSub2apiSettingsOpen(false);
    setSettingsTab("overview");
    setProxyDraft(accountProxyUrl);
  }
  function navigatePage(next, fromHistory = false) {
    if (next === activePage) return true;
    // Restore the current route while a draft decision is pending.
    if (fromHistory && (settingsDirty || settingsSaving)) history.replaceState(null, "", `#${activePage}`);
    return leaveSettings(() => {
      resetSettingsEditors();
      setActivePage(next);
      if (location.hash !== `#${next}`) location.hash = next;
      window.scrollTo({ top: 0 });
    });
  }
  function selectSettingsSection(next) {
    if (settingsSection === next) return;
    leaveSettings(() => {
      resetSettingsEditors();
      if (next === "sms") openSmsSettings();
      else if (next === "mail") openMailRequestSettings();
      else if (next === "sub2api") openSub2ApiSettings();
      else if (next === "cpa") { setSettingsTab(next); if (!cpa.loading && (!cpa.loaded || poolMonitor.status?.cpaRevision > cpa.settings?.revision)) void cpa.reload(); }
      else setSettingsTab(next);
    });
  }
  function closeSmsSettings() { leaveSettings(resetSettingsEditors); }
  function closeMailSettings() { leaveSettings(resetSettingsEditors); }
  function closeSub2ApiSettings() { leaveSettings(resetSettingsEditors); }
  function configureMonitorService() {
    leaveSettings(() => {
      resetSettingsEditors();
      setActivePage("settings");
      location.hash = "settings";
      if (monitorTarget === "sub2api") openSub2ApiSettings();
      else { setSettingsTab("cpa"); if (!cpa.loading) void cpa.reload(); }
      window.scrollTo({ top: 0 });
    });
  }
  useEffect(() => {
    const onHash = () => {
      const requested = location.hash.slice(1);
      navigatePage(["accounts", "trash", "settings", "monitor"].includes(requested) ? requested : "accounts", true);
    };
    const onUnload = (event) => { if (settingsDirty) { event.preventDefault(); event.returnValue = ""; } };
    window.addEventListener("hashchange", onHash);
    window.addEventListener("beforeunload", onUnload);
    return () => { window.removeEventListener("hashchange", onHash); window.removeEventListener("beforeunload", onUnload); };
  }, [activePage, settingsDirty, settingsSaving]);

  function openSmsSettings() {
    const draft = withSmsProviderDefaults(smsProviderDefinitions, smsSettings);
    setSmsSettingsDraft(draft);
    setSmsSettingsError("");
    setSmsSettingsOpen(true);
  }

  function openMailRequestSettings() {
    setMailRequestSettingsDraft({ ...mailRequestSettings });
    setMailRequestSettingsError("");
    setMailRequestSettingsOpen(true);
  }

  function saveAccountProxy(value) {
    if (!writeLocalTextSetting(ACCOUNT_PROXY_STORAGE_KEY, value)) return false;
    setAccountProxyUrl(value);
    setProxyDraft(value);
    return true;
  }

  async function saveMailRequestSettings(event) {
    event.preventDefault();
    if (mailSettingsWrite.current) return;
    let config;
    try {
      config = buildMailRequestConfig(mailRequestSettingsDraft);
    } catch (requestError) {
      setMailRequestSettingsError(requestError.message);
      return;
    }
    mailSettingsWrite.current = true;
    setMailRequestSettingsSaving(true);
    setMailRequestSettingsError("");
    try {
      await apiFetch(token, "/api/mail-request-config", {
        method: "POST",
        body: JSON.stringify({ config }),
      });
      const nextSettings = normalizeMailRequestSettings(mailRequestSettingsDraft);
      const persisted = writeLocalJson(MAIL_REQUEST_SETTINGS_KEY, nextSettings);
      setMailRequestSettings(nextSettings);
      if (!persisted) {
        setMailRequestSettingsError("邮件配置已在本地服务生效，但浏览器未能保存。刷新后可能恢复旧配置，请检查站点存储权限并重新保存。");
        return;
      }
      // A successful save confirms only its submitted draft. Keep edits made
      // while it was in flight visible and protected by the dirty-state guard.
      if (JSON.stringify(mailRequestDraft.current) === JSON.stringify(mailRequestSettingsDraft)) setMailRequestSettingsOpen(false);
    } catch (requestError) {
      setMailRequestSettingsError(requestError.message);
    } finally {
      mailSettingsWrite.current = false;
      setMailRequestSettingsSaving(false);
    }
  }

  function openSub2ApiSettings() {
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
    if (sub2apiSettingsWrite.current || sub2apiSettingsRead.current || sub2apiSettingsVerify.current) return;
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
      const { monitorEnabled, excludedPlanTypes, ...connectionConfig } = nextSettings;
      const monitor = await apiFetch(token, "/api/sub2api/monitor", {
        method: "POST",
        body: JSON.stringify({ config: connectionConfig }),
      });
      requireSub2ApiVerification(false);
      const savedSettings = withMonitorSettings(nextSettings, monitor);
      const persisted = writeLocalJson(SUB2API_UPLOAD_SETTINGS_KEY, savedSettings);
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
      if (draftUnchanged) setSub2apiSettingsOpen(false);
      setSub2apiSettingsError("");
    } catch (requestError) {
      const unknown = requestError.outcome === "unknown" || requestError.data?.uncertain === true || requestError.data?.outcome === "unknown" || !requestError.status || requestError.status >= 500;
      if (unknown) requireSub2ApiVerification(true);
      setSub2apiSettingsError(unknown ? "保存结果尚未确认，请先核对本地服务的监控配置，再明确保存；不会自动重试。" : requestError.message);
    } finally {
      sub2apiMonitorConfigVersion.current += 1;
      sub2apiSettingsWrite.current = false;
      setSub2apiSettingsSaving(false);
    }
  }

  async function verifySub2ApiSettings() {
    if (sub2apiSettingsWrite.current || sub2apiSettingsRead.current) return;
    sub2apiSettingsRead.current = true;
    sub2apiMonitorConfigVersion.current += 1;
    try {
      const monitor = await readMonitorSettings(signal => apiFetch(token, "/api/sub2api/monitor", { signal, cache: "no-store" }));
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
      if (requestError.name !== "AbortError") setSub2apiSettingsError(requestError.message);
    } finally {
      sub2apiMonitorConfigVersion.current += 1;
      sub2apiSettingsRead.current = false;
    }
  }

  function setSub2ApiGroupChecked(groupId, checked) {
    setSub2apiSettingsDraft((current) => {
      const selected = new Set(current.groupIds);
      if (checked) selected.add(String(groupId));
      else selected.delete(String(groupId));
      return { ...current, groupIds: [...selected] };
    });
  }

  async function uploadSelected(ids) {
    const requested = new Set(ids);
    function retainUnconfirmedCPASelection(receipt) {
      const succeeded = new Set(confirmedCPAUploadIds(receipt, ids));
      if (succeeded.size) setSelectedJobIds(current => new Set([...current].filter(id => !succeeded.has(id))));
    }
    if (jobSelectionIndex.some((job) => requested.has(job.id) && uploadBlockedJob(job))) {
      showNotice("请等待选中账号的当前流程结束或完成本地保存；2FA 结果未确认时需人工处理。", "warning");
      return;
    }
    if (!uploadAvailable) { showNotice(`当前服务暂不支持 ${targetName} 推送，请重新选择推送目标。`, "warning"); return; }
    if (uploadTarget === "cpa" && jobSelectionIndex.some(job => requested.has(job.id) && uploadTargetConflict(uploadTarget, job.authorizationMethod))) {
      showNotice("CPA 仅支持 ChatGPT / Codex 授权，请取消选择 BPS 账号，或切换至 Sub2API。", "warning");
      return;
    }
    if (!uploadConfigured) {
      if (navigatePage("settings") !== false) selectSettingsSection(uploadTarget);
      showNotice(uploadTarget === "cpa" ? "请先读取并配置 CPA 管理地址和管理密钥。" : "请先配置 Sub2API 后端地址、管理员 API Key 和目标号池", "warning");
      return;
    }
    if (batchActionRef.current) return;
    setBatchAction("upload");
    setError("");
    showNotice(uploadTarget === "cpa" ? "正在推送 CPA 授权文件，同名文件将更新，请稍候。" : "正在同步 Sub2API：检查已有账号并更新授权，请稍候。", "active");
    try {
      const data = await apiFetch(token, uploadTarget === "cpa" ? "/api/cpa/upload" : "/api/sub2api/upload", {
        method: "POST",
        body: JSON.stringify(uploadTarget === "cpa" ? { ids } : { ids, config: sub2apiSettings }),
      });
      const presentation = describeUpload(data, { target: uploadTarget });
      showNotice(`${targetName} · ${presentation.label}：${presentation.text}`, presentation.tone);
      if (uploadTarget === "cpa") retainUnconfirmedCPASelection(data);
      else setSelectedJobIds(new Set());
      setError("");
    } catch (requestError) {
      if (uploadTarget === "cpa") retainUnconfirmedCPASelection(requestError.data);
      const presentation = describeUpload(requestError.data || { uncertain: !requestError.status || requestError.outcome === "unknown" }, { error: requestError.message, target: uploadTarget });
      setError("");
      showNotice(`${targetName} · ${presentation.label}：${presentation.text}`, presentation.tone);
    } finally {
      setBatchAction("");
    }
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

  function saveSmsSettings(event) {
    event.preventDefault();
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
    if (!writeLocalJson(SMS_PROVIDER_SETTINGS_KEY, nextSettings)) {
      setSmsSettingsError("浏览器未能保存接码配置，请检查站点存储权限后重试。现有配置未变更。");
      return;
    }
    setSmsSettings(nextSettings);
    setSmsSettingsOpen(false);
    setSmsSettingsError("");
  }

  useEffect(() => {
    const valid = new Set(jobSelectionIndex.map((job) => job.id));
    setSelectedJobIds((current) => {
      const next = new Set([...current].filter((id) => valid.has(id)));
      if (next.size === current.size && [...next].every((id) => current.has(id))) return current;
      return next;
    });
  }, [jobSelectionIndex]);

  useEffect(() => {
    setExpandedJobId(null);
  }, [page]);

  async function createJob(event) {
    event.preventDefault();
    if (!email.trim() || busy || batchAction || reloginAction || totpAction) return;
    setReloginAction({ kind: "create", email: email.trim(), authorizationMethod: "chatgpt", initial: true });
  }

  async function createBatch(event) {
    event.preventDefault();
    if (!batchText.trim() || batchSubmission.current) return;
    batchSubmission.current = true;
    setBatchBusy(true);
    try {
      const data = await apiFetch(token, "/api/jobs/batch", {
        method: "POST",
        body: JSON.stringify({ text: batchText, timeZone: browserTimeZone(), ...(accountProxyUrl.trim() ? { proxyUrl: accountProxyUrl.trim() } : {}) }),
      });
      setEmailFilter([]);
      setFilterText("");
      setSelectedJobIds(new Set());
      setPage(1);
      if (page === 1) setJobs((current) => mergeJobs(data.jobs, current).slice(0, 20));
      const credentialWarnings = data.jobs.filter(job => typeof job.credentialWarning === "string" && job.credentialWarning).length;
      showNotice(`已导入 ${data.created} 个账号${data.updated ? `，更新 ${data.updated} 个账号资料` : ""}。账号已加入列表，请勾选后手动选择要执行的功能。${credentialWarnings ? ` ${credentialWarnings} 条存在凭据存储提醒，私有资料保存需核对。` : ""}`, credentialWarnings ? "warning" : "success");
      if (credentialWarnings) {
        setBatchError("账号记录已导入，但部分私有资料未确认可靠保存。已保留导入内容，请先查看账号的凭据存储提醒并核对保存结果，避免重复导入。");
        refreshAccounts();
        return;
      }
      setBatchText("");
      setBatchError("");
      setBatchOpen(false);
      setError("");
    } catch (requestError) {
      const receipt = requestError.data;
      if (Array.isArray(receipt?.jobs) && receipt.jobs.length) {
        setEmailFilter([]);
        setFilterText("");
        setSelectedJobIds(new Set());
        setPage(1);
        if (page === 1) setJobs((current) => mergeJobs(receipt.jobs, current).slice(0, 20));
        showNotice(requestError.message, "warning");
      }
      // Preserve the input for review. A failed HTTP response can still carry
      // confirmed saves; a lost response must never trigger an automatic replay.
      const detail = requestError.message.replace(/[。.!！?？；;\s]+$/u, "");
      setBatchError(`${detail}。已保留导入内容，请先核对账号列表与保存结果，避免重复导入；确认后只提交需要处理的资料。`);
      refreshAccounts();
    } finally {
      batchSubmission.current = false;
      setBatchBusy(false);
    }
  }

  function applyEmailFilter(event) {
    event.preventDefault();
    try {
      const emails = parseEmailFilter(filterText);
      setEmailFilter(emails);
      setSelectedJobIds(new Set());
      setExpandedJobId(null);
      setPage(1);
      setFilterError("");
      setFilterOpen(false);
    } catch (filterParseError) {
      setFilterError(filterParseError.message);
    }
  }

  function clearEmailFilter() {
    setEmailFilter([]);
    setFilterText("");
    setSelectedJobIds(new Set());
    setExpandedJobId(null);
    setPage(1);
    setFilterError("");
  }

  function toggleJobSelection(jobId) {
    setSelectedJobIds((current) => {
      const next = new Set(current);
      if (next.has(jobId)) next.delete(jobId);
      else next.add(jobId);
      return next;
    });
  }

  function toggleAllOnPage() {
    setSelectedJobIds((current) => {
      const next = new Set(current);
      pageJobIds.forEach((id) => {
        if (allPageSelected) next.delete(id);
        else next.add(id);
      });
      return next;
    });
  }

  async function downloadSelected() {
    if (!canDownloadSelected || batchActionRef.current) return;
    setBatchAction("download");
    try {
      const response = await fetch("/api/jobs/download-batch", {
        method: "POST",
        headers: { "content-type": "application/json", "x-console-token": token },
        body: JSON.stringify({ ids: [...selectedJobIds] }),
      });
      if (!response.ok) throw new Error((await response.json()).error || "批量下载失败");
      await saveDownloadResponse(response, `sub2api-import-oauth-${downloadableSelectedCount}-accounts-${localTimestamp()}.json`);
      setError("");
    } catch (requestError) {
      setError(requestError.message);
    } finally {
      setBatchAction("");
    }
  }

  async function reauthorizeSelected() {
    if (!canReauthorizeSelected || batchAction || reloginAction || totpAction) return;
    setReloginAction({ kind: "reauthorize", ids: [...selectedJobIds], batch: true, eligibleCount: selectedJobIds.size, initial: hasImportedSelected, authorizationMethod: selectedAuthorizationMethod });
  }

  function setupTotpSelected(resetTotp = false) {
    if (!(resetTotp ? canResetTotpSelected : canSetupTotpSelected) || batchAction || totpAction || reloginAction) return;
    const eligibleCount = resetTotp ? totpResetSelectedCount : totpSetupSelectedCount;
    setTotpAction({ ids: [...selectedJobIds], totpIntentId: resetTotp ? pendingTotpIntent([...selectedJobIds]) : undefined, resetTotp, batch: true, eligibleCount, skipped: selectedJobIds.size - eligibleCount, authorizationMethod: selectedAuthorizationMethod });
  }

  function setupTotpAccount(job, resetTotp, completeOnly = false) {
    const completeTotp = Boolean(completeOnly && job.canCompleteTotp);
    if (batchAction || totpAction || reloginAction || !(completeTotp ? job.canCompleteTotp : resetTotp ? job.canResetTotp : job.canSetupTotp)) return;
    setTotpAction({ ids: [job.id], totpIntentId: resetTotp && !completeTotp ? pendingTotpIntent([job.id]) : undefined, resetTotp, completeTotp, batch: false, email: job.email, eligibleCount: 1, skipped: 0, authorizationMethod: job.authorizationMethod });
  }

  function showBatchSubmission(data, operation) {
    if (!data || !Array.isArray(data.jobs) || !Number.isFinite(data.queued ?? data.started) || !Number.isFinite(data.skipped)) return false;
    const presentation = describeBatchSubmission(data, operation);
    const queuedIds = new Set(data.jobs.map((job) => job.id));
    setSelectedJobIds((current) => new Set([...current].filter((id) => !queuedIds.has(id))));
    setError("");
    showNotice(`${presentation.label}：${presentation.text}`, presentation.tone);
    return true;
  }

  async function submitTotpAction(options) {
    if (!totpAction || totpSubmission.current || batchActionRef.current) return;
    const { ids, totpIntentId, resetTotp, completeTotp, batch } = totpAction;
    const target = normalizeUploadTarget(options.uploadTarget ?? uploadTarget);
    const reloginAfterTotp = Boolean(!completeTotp && features.forceRelogin && options.reloginAfterTotp);
    if (reloginAfterTotp && options.uploadAfterTotp && uploadTargetConflict(target, options.authorizationMethod)) throw new Error(CPA_AUTHORIZATION_HELP);
    if (reloginAfterTotp && options.uploadAfterTotp && target === "cpa" && (!features.cpaUpload || !cpa.configured)) throw new Error("请先读取并保存 CPA 配置，再提交自动上传。");
    const uploadAfterTotp = Boolean(uploadTargets[target].available && reloginAfterTotp && options.uploadAfterTotp && uploadTargets[target].configured);
    totpSubmission.current = true;
    setBatchAction(completeTotp ? "complete-2fa" : resetTotp ? "reset-2fa" : "setup-2fa");
    try {
      const data = await apiFetch(token, completeTotp ? `/api/jobs/${ids[0]}/complete-2fa` : batch ? "/api/jobs/setup-2fa-batch" : `/api/jobs/${ids[0]}/setup-2fa`, {
        method: "POST",
        body: JSON.stringify(completeTotp ? {} : {
          ...(resetTotp ? { totpIntentId } : {}),
          ...(batch ? { ids } : {}),
          proxyUrl: accountProxyUrl.trim(),
          resetTotp,
          logoutAllDevicesAfterTotp: Boolean(resetTotp && features.logoutAllDevicesAfterTotp && options.logoutAllDevicesAfterTotp),
          reloginAfterTotp,
          ...(reloginAfterTotp ? { authorizationMethod: options.authorizationMethod } : {}),
          uploadAfterTotp,
          ...(uploadAfterTotp ? target === "cpa" ? { uploadTarget: "cpa" } : { sub2apiConfig: sub2apiSettings } : {}),
        }),
      });
      if (totpIntentId) acknowledgeTotpIntent(ids, totpIntentId);
      if (batch) showBatchSubmission(data, resetTotp ? "重置 2FA" : "设置 2FA");
      else showNotice(completeTotp ? "已提交本地保存；不连接远端、不重新登录、不修改 2FA。" : "已加入 1 个账号的 2FA 流程，当前状态见账号行，详细步骤见日志。", "info");
      setTotpAction(null);
      setError("");
    } catch (requestError) {
      if (batch && showBatchSubmission(requestError.data, resetTotp ? "重置 2FA" : "设置 2FA")) {
        setTotpAction(null);
        return;
      }
      throw requestError;
    } finally {
      totpSubmission.current = false;
      setBatchAction("");
    }
  }

  async function addPasswordSelected() {
    if (!canAddPasswordSelected || batchActionRef.current) return;
    const skipped = selectedJobIds.size - passwordAddSelectedCount;
    const message = `确定为选中的 ${passwordAddSelectedCount} 个无密码账号生成并添加随机强密码吗？${skipped ? `另有 ${skipped} 个账号已有密码或不符合条件，将自动跳过。` : ""}`;
    setBatchAction("add-password");
    try {
      if (!await confirm(message)) return;
      const data = await apiFetch(token, "/api/jobs/add-password-batch", {
        method: "POST",
        body: JSON.stringify({ ids: [...selectedJobIds], proxyUrl: accountProxyUrl.trim() }),
      });
      showBatchSubmission(data, "添加密码");
    } catch (requestError) {
      if (!showBatchSubmission(requestError.data, "添加密码")) setError(requestError.message);
    } finally {
      setBatchAction("");
    }
  }

  function forceReloginSelected() {
    if (!canForceReloginSelected || batchAction || reloginAction || totpAction) return;
    setReloginAction({ ids: [...selectedJobIds], batch: true, eligibleCount: forceReloginSelectedCount, skipped: selectedJobIds.size - forceReloginSelectedCount, authorizationMethod: selectedAuthorizationMethod });
  }

  function forceReloginAccount(job) {
    if (!job.canForceRelogin || batchAction || reloginAction || totpAction) return;
    setReloginAction({ ids: [job.id], batch: false, email: job.email, eligibleCount: 1, skipped: 0, authorizationMethod: job.authorizationMethod });
  }

  function authorizeAccount(job, kind) {
    if (batchAction || reloginAction || totpAction) return;
    setReloginAction({ kind, ids: [job.id], email: job.email, initial: job.status === "imported", lockedMethod: kind === "retry" && job.canResume, authorizationMethod: job.authorizationMethod });
  }

  async function submitReloginAction(options) {
    if (!reloginAction || reloginSubmission.current || batchActionRef.current) return;
    const { ids, batch, kind = "relogin" } = reloginAction;
    const localCompletion = kind === "retry" && reloginAction.lockedMethod;
    const authorizationMethod = normalizeAuthorizationMethod(localCompletion ? reloginAction.authorizationMethod : options.authorizationMethod);
    const target = normalizeUploadTarget(options.uploadTarget ?? uploadTarget);
    if (kind === "relogin" && options.uploadAfterRelogin && uploadTargetConflict(target, authorizationMethod)) throw new Error(CPA_AUTHORIZATION_HELP);
    if (kind === "relogin" && options.uploadAfterRelogin && target === "cpa" && (!features.cpaUpload || !cpa.configured)) throw new Error("请先读取并保存 CPA 配置，再提交自动上传。");
    const uploadAfterRelogin = Boolean(kind === "relogin" && features.reloginAutoUpload && uploadTargets[target].available && options.uploadAfterRelogin && uploadTargets[target].configured);
    reloginSubmission.current = true;
    setBatchAction(kind);
    try {
      const path = kind === "create" ? "/api/jobs" : batch ? `/api/jobs/${kind}-batch` : `/api/jobs/${ids[0]}/${kind}`;
      const data = await apiFetch(token, path, {
        method: "POST",
        body: JSON.stringify(localCompletion ? { authorizationMethod } : { ...(kind === "create" ? { email: reloginAction.email } : batch ? { ids } : {}), proxyUrl: accountProxyUrl.trim(), authorizationMethod, ...(kind === "relogin" ? { uploadAfterRelogin } : {}), ...(uploadAfterRelogin ? target === "cpa" ? { uploadTarget: "cpa" } : { sub2apiConfig: sub2apiSettings } : {}) }),
      });
      if (kind === "create") {
        setPage(1);
        if (page === 1) setJobs((current) => mergeJobs([data.job], current).slice(0, 20));
        setEmail("");
      }
      if (batch) showBatchSubmission(data, `${authorizationMethodLabel(authorizationMethod)} 授权${uploadAfterRelogin ? `（授权成功后自动上传 ${uploadTargetLabel(target)}）` : ""}`);
      else showNotice(localCompletion ? "已提交本地授权保存；不连接远端、不重新登录。" : `已提交 1 个账号的 ${authorizationMethodLabel(authorizationMethod)} 授权任务${uploadAfterRelogin ? `，成功后自动上传 ${uploadTargetLabel(target)}` : ""}。请在账号行查看进度。`, "info");
      setReloginAction(null);
      setError("");
    } catch (requestError) {
      if (batch && showBatchSubmission(requestError.data, `${authorizationMethodLabel(authorizationMethod)} 授权`)) {
        setReloginAction(null);
        return;
      }
      throw requestError;
    } finally {
      reloginSubmission.current = false;
      setBatchAction("");
    }
  }

  async function exportSelectedSource() {
    if (!selectedJobIds.size || batchActionRef.current) return;
    setBatchAction("source");
    try {
      const response = await fetch("/api/jobs/export-source", {
        method: "POST",
        headers: { "content-type": "application/json", "x-console-token": token },
        body: JSON.stringify({ ids: [...selectedJobIds] }),
      });
      if (!response.ok) throw new Error((await response.json()).error || "原始信息导出失败");
      await saveDownloadResponse(response, `chatgpt-account-source-${selectedJobIds.size}-accounts-${localTimestamp()}.txt`);
      setError("");
    } catch (requestError) {
      setError(requestError.message);
    } finally {
      setBatchAction("");
    }
  }

  async function deleteAccounts(ids, emailLabel = "") {
    if (!ids.length || trashMovePending.current || batchActionRef.current || !features.accountTrash || trash.actionBusy || trash.verificationRequired) return;
    const trigger = removalFocus.capture();
    let focusOperation;
    trashMovePending.current = true;
    setBatchAction("delete");
    try {
      if (!await confirm("确定将" + (emailLabel ? "账号 " + emailLabel : "选中的 " + ids.length + " 条记录对应的账号") + "移入回收站吗？同邮箱的全部历史记录、日志、密码、2FA 与授权文件将一起保留，可在回收站恢复。Sub2API 中的账号不受影响。", { title: "移入回收站", confirmLabel: "移入回收站" })) return;
      focusOperation = removalFocus.begin(trigger);
      const receipt = await trash.mutate("move", ids);
      removalFocus.complete(focusOperation, receipt?.count > 0 || ["unknown", "partial"].includes(receipt?.outcome));
      focusOperation = null;
    } finally { removalFocus.complete(focusOperation, false); trashMovePending.current = false; setBatchAction(""); }
  }

  function deleteSelected() {
    return deleteAccounts([...selectedJobIds]);
  }

  async function cancelAllRunningJobs() {
    const runningCount = (stats.active || 0) + (stats.queued || 0);
    if (!runningCount || batchActionRef.current) return;
    setBatchAction("cancel-all");
    try {
      if (!await confirm(`确定停止全部 ${runningCount} 条进行中和排队任务吗？`)) return;
      await apiFetch(token, "/api/jobs/cancel-all", { method: "POST" });
      setSelectedJobIds(new Set());
      setError("");
    } catch (requestError) {
      setError(requestError.message);
    } finally {
      setBatchAction("");
    }
  }

  return (
    <AppShell page={activePage} trashAvailable={features.accountTrash === true} onNavigate={navigatePage} theme={theme} onThemeChange={setTheme} connected={connected} total={pagination.totalAll ?? pagination.total}>
        {themeStorageError && (
          <div className="global-notice warning" role="status"><CircleAlert size={17} /><span>{themeStorageError}</span></div>
        )}
        {connectionError && (
          <div className="global-notice warning" role="status"><CircleAlert size={17} /><span>状态同步暂时中断：{connectionError}。页面保留最近一次数据。</span></div>
        )}
        {monitorConnectionError && !connectionError && !(activePage === "monitor" && monitorTarget === "cpa") && (
          <div className="global-notice warning" role="status"><CircleAlert size={17} /><span>Sub2API 监控状态暂未更新：{monitorConnectionError}。页面保留最近一次巡检结果。</span></div>
        )}
        {activePage === "accounts" && insights.summaryError && !connectionError && <div className="global-notice warning" role="status"><CircleAlert size={17} /><span>账号查询摘要暂未更新：{insights.summaryError}。页面保留最近一次结果。</span></div>}
        {error && (
          <div className="global-error" role="alert">
            <CircleAlert size={17} />
            <span>{error}</span>
            <button type="button" onClick={() => setError("")} title="关闭"><X size={16} /></button>
          </div>
        )}
        {uploadNotice && (
          <div className={`global-notice ${noticeTone}`} role="status">
            {noticeTone === "success" ? <Check size={17} /> : noticeTone === "warning" ? <CircleAlert size={17} /> : noticeTone === "active" ? <LoaderCircle className="spin" size={17} /> : <FileText size={17} />}
            <span>{uploadNotice}</span>
            <button type="button" onClick={clearNotice} title="关闭"><X size={16} /></button>
          </div>
        )}


      {trash.notice && ["accounts", "trash"].includes(activePage) && <div className={"global-notice " + trash.notice.tone} role="status"><span>{trash.notice.message}</span><button type="button" onClick={trash.clearNotice} title="关闭"><X size={16} /></button></div>}
      <section className="accounts-section" aria-label="账号管理" hidden={activePage !== "accounts"}>
        <AccountsOverview stats={stats} total={pagination.totalAll ?? pagination.total}
          insightsEnabled={features.accountInsights === true && insights.settingsLoaded && insights.settings.enabled}
          refreshSummary={insights.refreshSummary}
          refreshError={insights.statusError} onVerifyRefresh={() => insights.reloadStatus?.()} />
        <div className="section-card account-list-card">
        <div className="account-list-header">
          <div className="account-list-title">
            <h2 ref={accountsHeadingRef} tabIndex={-1}>{features.accountQuality === true ? "账号信息与操作" : features.accountInsights === true && insights.settingsLoaded && insights.settings.enabled ? "账号用量与操作" : "账号列表"}</h2>
            <p>{emailFilter.length
              ? `匹配 ${pagination.total} 条，共 ${pagination.totalAll ?? pagination.total} 条任务`
              : (pagination.total ? `共 ${pagination.total} 条任务` : "添加邮箱或批量导入账号")}</p>
          </div>
          <div className="account-list-tools">
        {features.accountQuality === true && <div className="quality-toolbar" role="group" aria-label="推理探针操作">
          <button type="button" className="selection-text-button" disabled={!token || !connected || !quality.canSubmit(null) || !(pagination.totalAll ?? pagination.total)} onClick={() => checkQuality()} title="检测全部账号，不受筛选或分页限制；发送合成提示并消耗模型额度">全部账号探针</button>
          <button type="button" className="selection-text-button" disabled={!token || !connected || !quality.canSubmit([...selectedJobIds])} onClick={() => checkQuality([...selectedJobIds])}>所选账号探针</button>
          <button type="button" className="selection-text-button" onClick={() => { if (navigatePage("settings") !== false) selectSettingsSection("quality"); }}>探针设置</button>
          <span className="quality-toolbar-status" role="status">{quality.actionBusy ? "正在提交检测操作…" : quality.verificationRequired ? "正在核对受理结果…" : quality.status?.active || quality.status?.queued ? `${quality.status.active} 个检测中 · ${quality.status.queued} 个排队` : quality.settingsLoaded ? quality.settings.enabled ? "推理探针 · 非标准 IQ · 消耗模型额度" : "检测未启用 · 历史结果仍可查看" : quality.settingsError ? "检测配置待核实" : "正在读取检测配置…"}</span>
          {(quality.error || quality.settingsVerificationRequired) && <span className="quality-toolbar-status">{quality.error || quality.settingsError}<button type="button" className="selection-text-button" disabled={Boolean(quality.actionBusy)} onClick={() => quality.settingsVerificationRequired ? void quality.reloadSettings() : void quality.reload()}>核对状态</button></span>}
        </div>}
        {features.bulkActions && jobs.length > 0 && (
            <div className="selection-summary">
              <span className="selection-count" role="status">已选 <strong>{selectedJobIds.size}</strong> 条</span>
              <button type="button" className="selection-text-button" onClick={toggleAllOnPage} disabled={allPageSelected || !pageJobIds.length}>
                本页全选
              </button>
              {selectedJobIds.size > 0 && (
                <button type="button" className="selection-text-button" onClick={() => setSelectedJobIds(new Set())}>
                  清除选择
                </button>
              )}
            </div>
          )}
            <div className="account-action-tools">
          <form className="add-form account-add-form" onSubmit={createJob}>
            <div className="email-field">
              <Mail size={17} aria-hidden="true" />
              <input
                type="email"
                value={email}
                onChange={(event) => setEmail(event.target.value)}
                placeholder="输入邮箱地址"
                autoComplete="email"
                aria-label="邮箱地址"
                required
              />
            </div>
            <button className="primary-button" type="submit" disabled={!token || busy}>
              {busy ? <LoaderCircle className="spin" size={17} /> : <Plus size={17} />}
              添加账号
            </button>
            <button className="secondary-button" type="button" onClick={() => { setBatchError(""); setBatchOpen(true); }} disabled={!token}>
              <ListPlus size={17} />
              批量导入
            </button>
            <button
              className={`secondary-button ${emailFilter.length ? "filter-active" : ""}`}
              type="button"
              onClick={() => { setFilterError(""); setFilterText(emailFilter.join("\n")); setFilterOpen(true); }}
              disabled={!token}
            >
              <Filter size={17} />
              {emailFilter.length ? `筛选 ${emailFilter.length}` : "筛选账号"}
            </button>
            {emailFilter.length > 0 && (
              <button className="selection-text-button clear-filter-button" type="button" onClick={clearEmailFilter}>
                清除筛选
              </button>
            )}
          </form>
        {features.bulkActions && jobs.length > 0 && (
            <div className="bulk-actions account-batch-actions" role="group" aria-label="账号批量操作">
              {features.forceRelogin && (
                <button type="button" className="relogin-button bulk-button" onClick={forceReloginSelected} disabled={!canForceReloginSelected || Boolean(batchAction)} title={`选中账号中 ${forceReloginSelectedCount} 个可重登授权`}>
                  {batchAction === "relogin" ? <LoaderCircle className="spin" size={15} /> : <LogIn size={15} />}
                  重登授权
                </button>
              )}
              {features.totpReset && (
                <button type="button" className="secondary-button bulk-button" onClick={() => setupTotpSelected(true)} disabled={!canResetTotpSelected || Boolean(batchAction)} title={`选中账号中 ${totpResetSelectedCount} 个可重置；其他账号跳过`}>
                  {batchAction === "reset-2fa" ? <LoaderCircle className="spin" size={15} /> : <RotateCcw size={15} />}
                  重置 2FA
                </button>
              )}
              {anyUploadAvailable && (
                <button type="button" className="secondary-button bulk-button" aria-label={`上传 ${targetName}`} onClick={() => uploadSelected([...selectedJobIds])} disabled={!canUploadSelected || Boolean(batchAction)} title={uploadBlockedSelected ? "请等待选中账号的当前流程结束或完成本地保存" : uploadTarget === "cpa" ? "推送选中账号到 CPA；同名授权文件将替换，使用当前优先级和权重" : "上传选中账号；已有账号更新授权并保留配置"}>
                  {batchAction === "upload" ? <LoaderCircle className="spin" size={15} /> : <Send size={15} />}
                  上传
                </button>
              )}
              {features.accountTrash === true && <button type="button" className="delete-button bulk-button" onClick={deleteSelected} disabled={!selectedJobIds.size || Boolean(batchAction) || Boolean(trash.actionBusy) || trash.verificationRequired} title="移入回收站，保留同邮箱账号资料和历史日志">
                {batchAction === "delete" ? <LoaderCircle className="spin" size={15} /> : <Trash2 size={15} />}
                删除
              </button>}
              <button type="button" className="secondary-button bulk-button" aria-label="刷新所选账号用量" aria-describedby="selected-insights-refresh-description" aria-busy={insights.selectedRefreshControl?.busy === true} disabled={!token || !connected || !selectedJobIds.size || !insights.settings.enabled || !insights.settingsLoaded || insights.settingsLoading || insights.verificationRequired || insights.saving || insights.actionBusy || (insights.selectedRefreshControl?.disabled ?? true)} onClick={() => refreshInsights([...selectedJobIds])} title={!insights.settings.enabled ? "请先启用账号查询" : insights.selectedRefreshControl?.description || "刷新所选账号的用量与状态；仅查询勾选账号，不触发授权或上传"}>{insights.selectedRefreshControl?.busy ? <LoaderCircle size={15} className="spin" aria-hidden="true" /> : <RefreshCw size={15} aria-hidden="true" />}{insights.selectedRefreshControl?.label || "刷新所选用量"}</button>
              <span id="selected-insights-refresh-description" className="sr-only" hidden={!selectedJobIds.size || !insights.settings.enabled}>{insights.selectedRefreshControl?.description}</span>
              <ActionMenu label="更多" ariaLabel="打开其他批量操作菜单">
              <p className="action-menu-note">已选 {selectedJobIds.size} 条 · 可下载 {downloadableSelectedCount} 条</p>
              {features.cancelAll && (
                <button
                  type="button"
                  className="stop-all-button"
                  onClick={cancelAllRunningJobs}
                  disabled={!(stats.active || stats.queued) || Boolean(batchAction)}
                >
                  {batchAction === "cancel-all" ? <LoaderCircle className="spin" size={16} /> : <Ban size={16} />}
                  停止全部
                </button>
              )}
              <button type="button" className="download-button" onClick={downloadSelected} disabled={!canDownloadSelected || Boolean(batchAction)}>
                {batchAction === "download" ? <LoaderCircle className="spin" size={16} /> : <Download size={16} />}
                批量下载
              </button>
              {features.sourceExport && (
                <button type="button" className="secondary-button bulk-button" onClick={exportSelectedSource} disabled={!selectedJobIds.size || Boolean(batchAction)}>
                  {batchAction === "source" ? <LoaderCircle className="spin" size={16} /> : <FileText size={16} />}
                  导出原始信息
                </button>
              )}
              <button type="button" className="regenerate-button bulk-button" onClick={reauthorizeSelected} disabled={!canReauthorizeSelected || Boolean(batchAction)}>
                {batchAction === "reauthorize" ? <LoaderCircle className="spin" size={16} /> : <RefreshCw size={16} />}
                {hasImportedSelected ? "批量开始授权" : "批量重新授权"}
              </button>
              {features.totpSetup && (
                <button type="button" className="secondary-button bulk-button" onClick={() => setupTotpSelected(false)} disabled={!canSetupTotpSelected || Boolean(batchAction)} title="仅为尚未启用 2FA 的账号设置密钥">
                  {batchAction === "setup-2fa" ? <LoaderCircle className="spin" size={16} /> : <ShieldCheck size={16} />}
                  批量设置 2FA
                </button>
              )}
              {features.totpReset && selectedJobIds.size > 0 && (
                <p className="action-menu-note">2FA：{totpSetupSelectedCount} 个可设置，{totpResetSelectedCount} 个可重置；其余账号将跳过。</p>
              )}
              {features.passwordAdd && (
                <button type="button" className="secondary-button bulk-button" onClick={addPasswordSelected} disabled={!canAddPasswordSelected || Boolean(batchAction)}>
                  {batchAction === "add-password" ? <LoaderCircle className="spin" size={16} /> : <KeyRound size={16} />}
                  批量添加密码
                </button>
              )}
              </ActionMenu>
            </div>
        )}
          </div>
          </div>
        </div>

        {jobs.length > 0 && <p className="table-scroll-hint" data-quality={features.accountQuality === true}>左右滑动查看完整账号信息</p>}
        <div className="table-frame accounts-table-frame" role="region" aria-label="账号列表，可左右滚动查看全部操作" tabIndex={0}>
          <table className="accounts-table" data-quality={features.accountQuality === true} aria-label="账号及授权任务">
            <thead>
              <tr>
                <th className="select-heading">
                  <input
                    type="checkbox"
                    checked={allPageSelected}
                    onChange={toggleAllOnPage}
                    disabled={!features.bulkActions || !pageJobIds.length}
                    aria-label="选择当前页全部任务"
                  />
                </th>
                <th className="account-heading">账号</th>
                <th className="usage-heading" title="各额度窗口独立显示已使用比例；额度重置时间与套餐到期时间相互独立">已用比例</th>
                {features.accountQuality === true && <th className="quality-heading" title="糖果题推理检测，不是标准 IQ">推理探针</th>}
                <th className="expiration-heading">到期时间</th>
                <th className="current-operation-heading">当前操作</th>
                <th className="activity-time-heading">操作时间</th>
                <th className="operation-type-heading">操作类型</th>
                <th className="actions-heading">操作</th>
              </tr>
            </thead>
            <tbody>
              {!jobs.length && <EmptyState filtered={emailFilter.length > 0} columnCount={features.accountQuality === true ? 9 : 8} />}
              {jobs.map((job) => (
                <React.Fragment key={job.id}>
                  <JobRow
                    job={job}
                    token={token}
                    expanded={expandedJobId === job.id}
                    onToggleLogs={() => setExpandedJobId((current) => current === job.id ? null : job.id)}
                    onError={setError}
                    selected={selectedJobIds.has(job.id)}
                    onToggleSelected={() => toggleJobSelection(job.id)}
                    selectionSupported={Boolean(features.bulkActions)}
                    smsProviderAvailable={smsProviderDefinitions.length > 0}
                    smsProvider={activeSmsProvider}
                    onUpload={() => uploadSelected([job.id])}
                    sub2apiUploadAvailable={anyUploadAvailable}
                    uploadTarget={uploadTarget}
                    totpSetupAvailable={Boolean(features.totpSetup)}
                    totpResetAvailable={Boolean(features.totpReset)}
                    onSetupTotp={(resetTotp, completeOnly) => setupTotpAccount(job, resetTotp, completeOnly)}
                    passwordAddAvailable={Boolean(features.passwordAdd)}
                    forceReloginAvailable={Boolean(features.forceRelogin)}
                    onRelogin={() => forceReloginAccount(job)}
                    onAuthorize={(kind) => authorizeAccount(job, kind)}
                    onDelete={() => deleteAccounts([job.id], job.email)}
                    onViewAccount={() => setAccountInfoJob(job)}
                    deleteSupported={features.accountTrash === true}
                    accountInfoAvailable={Boolean(features.accountInfo)}
                    accountActionsBusy={Boolean(batchAction) || Boolean(trash.actionBusy) || trash.verificationRequired}
                    accountProxyUrl={accountProxyUrl}
                    insightSnapshot={insightSnapshotForJob(job, insights.snapshots[job.id])}
                    onViewInsights={() => setInsightJob(job)}
                    onRefreshInsights={() => refreshInsights([job.id])}
                    insightEnabled={insights.settings.enabled && insights.settingsLoaded && !insights.settingsLoading && !insights.verificationRequired}
                    insightBusy={insights.actionBusy || insights.refreshPending}
                    subscriptionAvailable={features.accountSubscription === true}
                    subscriptionRefreshState={subscriptions.states[job.id]}
                    onRefreshSubscription={() => subscriptions.refresh(job)}
                    qualityAvailable={features.accountQuality === true}
                    qualitySnapshot={quality.snapshots[job.id]}
                    qualityStateUncertain={Boolean(quality.verificationRequired || quality.uncertain || quality.error || quality.status?.active > 0)}
                    onViewQuality={() => setQualityJob(job)}
                    onCheckQuality={() => checkQuality([job.id])}
                    qualityCanCheck={quality.canSubmit([job.id])}
                  />
                  {expandedJobId === job.id && (
                    <tr className="log-row">
                      <td colSpan={features.accountQuality === true ? 9 : 8}><JobLogs token={token} jobId={job.id} job={job} /></td>
                    </tr>
                  )}
                </React.Fragment>
              ))}
            </tbody>
          </table>
        </div>
        {features.pagination && pagination.totalPages > 1 && (
          <nav className="pagination" aria-label="任务分页">
            <button type="button" className="icon-button" onClick={() => setPage((current) => Math.max(1, current - 1))} disabled={page <= 1} title="上一页">
              <ChevronLeft size={17} />
            </button>
            <span>第 <strong>{pagination.page}</strong> / {pagination.totalPages} 页</span>
            <button type="button" className="icon-button" onClick={() => setPage((current) => Math.min(pagination.totalPages, current + 1))} disabled={page >= pagination.totalPages} title="下一页">
              <ChevronRight size={17} />
            </button>
          </nav>
        )}
        </div>
      </section>
      {activePage === "trash" && <AccountTrashPage token={token} trash={trash} available={features.accountTrash === true} />}
      {pendingNavigation && (
        <DialogFrame onClose={() => setPendingNavigation(null)}>
          <section className="batch-dialog" role="dialog" aria-modal="true" aria-labelledby="unsaved-title">
            <div className="dialog-header"><h2 id="unsaved-title">放弃未保存的更改？</h2></div>
            <p className="dialog-hint">当前配置尚未保存。离开后，这些更改会丢失。</p>
            <div className="dialog-footer">
              <button type="button" className="cancel-button" onClick={() => setPendingNavigation(null)}>继续编辑</button>
              <button type="button" className="primary-button" onClick={() => {
                const navigate = pendingNavigation;
                setPendingNavigation(null);
                navigate();
              }}>放弃更改并离开</button>
            </div>
          </section>
        </DialogFrame>
      )}
      {totpAction && <TotpActionDialog uploadTarget={uploadTarget} onUploadTargetChange={selectUploadTarget} uploadTargets={uploadTargets} uploadTargetSaved={uploadTargetSaved} action={totpAction} logoutAvailable={Boolean(features.logoutAllDevicesAfterTotp)} reloginAvailable={Boolean(features.forceRelogin)} uploadAvailable={uploadAvailable} uploadConfigured={uploadConfigured} onConfigureUpload={(target = uploadTarget) => { setTotpAction(null); if (navigatePage("settings") !== false) selectSettingsSection(target); }} onSubmit={submitTotpAction} onClose={() => setTotpAction(null)} />}
      {reloginAction && <ReloginActionDialog uploadTarget={uploadTarget} onUploadTargetChange={selectUploadTarget} uploadTargets={uploadTargets} uploadTargetSaved={uploadTargetSaved} action={reloginAction} uploadAvailable={Boolean(features.reloginAutoUpload && uploadAvailable)} uploadConfigured={uploadConfigured} onConfigureUpload={(target = uploadTarget) => { setReloginAction(null); if (navigatePage("settings") !== false) selectSettingsSection(target); }} onSubmit={submitReloginAction} onClose={() => setReloginAction(null)} />}
      {accountInfoJob && <AccountInfoDialog key={JSON.stringify([token, accountInfoJob.id])} job={accountInfoJob} token={token} insightSnapshot={insights.snapshots[accountInfoJob.id]} onExpirationSaved={(updatedJob) => {
        refreshAccounts.invalidate?.();
        const adopt = current => current?.id === updatedJob.id && subscriptionResponseIsCurrent(updatedJob, accountInfoJob, current) ? { ...current, ...updatedJob } : current;
        setJobs(current => current.map(adopt));
        setJobSelectionIndex(current => current.map(adopt));
        setAccountInfoJob(adopt);
        refreshAccounts();
      }} onClose={() => setAccountInfoJob(null)} />}
      {insightJob && <AccountInsightsDialog key={insightJob.id} job={jobs.find(job => job.id === insightJob.id) || insightJob} token={token} initialSnapshot={insights.snapshots[insightJob.id]} qualitySnapshot={features.accountQuality === true ? quality.snapshots[insightJob.id] : undefined} insights={insights} subscriptionAvailable={features.accountSubscription === true} subscriptionRefreshState={subscriptions.states[insightJob.id]} onRefreshSubscription={() => subscriptions.refresh(insightJob)} onClose={() => setInsightJob(null)} />}
      {qualityJob && features.accountQuality === true && <AccountQualityDialog key={qualityJob.id} job={jobs.find(job => job.id === qualityJob.id) || qualityJob} quality={quality} onClose={() => setQualityJob(null)} onCheck={options => checkQuality([qualityJob.id], options)} />}
      {activePage === "settings" && <ConfigurationPage cpaAvailable={features.cpaUpload === true} cpa={cpa} qualityAvailable={features.accountQuality === true} quality={quality} trashAvailable={features.accountTrash === true} trashSettings={trash.settings} section={settingsSection} onSectionChange={selectSettingsSection} dirty={settingsDirty} saving={settingsSaving} sms={activeSmsProvider} mail={mailRequestSettings} sub2api={sub2apiSettings} proxy={accountProxyUrl} proxyDraft={proxyDraft} onProxyDraftChange={setProxyDraft} onProxySave={saveAccountProxy} theme={theme} onThemeChange={setTheme} onClear={clearBrowserSettings} clearDisabled={busy || batchBusy || Boolean(batchAction) || settingsSaving} clearError={browserSettingsError} insightSettings={insights.settings} insightSettingsLoaded={insights.settingsLoaded} insightSettingsError={insights.settingsError} unconfirmed={settingsSection === "insights" && insights.uncertain || settingsSection === "trash" && (trash.uncertain || trash.persistenceUnconfirmed) || settingsSection === "quality" && quality.uncertain || settingsSection === "cpa" && cpa.uncertain}>
        {settingsSection === "cpa" && features.cpaUpload === true && <CPASettings cpa={cpa} />}
        {settingsSection === "insights" && <AccountInsightsSettings insights={insights} />}
        {settingsSection === "quality" && features.accountQuality === true && <AccountQualitySettings quality={quality} jobs={jobs} />}
        {settingsSection === "trash" && features.accountTrash === true && <AccountTrashSettings trash={trash} />}
        {smsSettingsOpen && <SmsSettingsDialog inline {...{ setSmsSettingsOpen: closeSmsSettings, saveSmsSettings, smsProviderDefinitions, draftSmsProvider, setSmsSettingsDraft, setSmsSettingsError, smsNumberOptions, smsOptionsLoading, updateSmsProviderConfig, loadSmsNumberOptions, smsSettingsError }} />}
        {mailRequestSettingsOpen && <MailSettingsDialog inline {...{ setMailRequestSettingsOpen: closeMailSettings, mailRequestSettingsSaving, saveMailRequestSettings, mailRequestSettingsDraft, setMailRequestSettingsDraft, mailRequestSettingsError, token }} />}
        {sub2apiSettingsOpen && <Sub2ApiSettingsDialog inline onOpenQuality={() => selectSettingsSection("quality")} {...{ setSub2apiSettingsOpen: closeSub2ApiSettings, saveSub2ApiSettings, sub2apiSettingsDraft, setSub2apiSettingsDraft, features, sub2apiGroups, setSub2ApiGroupChecked, sub2apiProxies, sub2apiSettingsError, loadSub2ApiOptions, sub2apiGroupsLoading, token, sub2apiSettingsSaving, sub2apiSettingsVerificationRequired, sub2apiSettingsVerifying, verifySub2ApiSettings }} />}
      </ConfigurationPage>}
      {activePage === "monitor" && <MonitorPage target={monitorTarget} onTargetChange={selectMonitorTarget}
        policy={poolMonitor} status={poolMonitor.status} onConfigure={configureMonitorService}
        onCheck={poolMonitor.check} onRefresh={poolMonitor.refresh} checking={poolMonitor.checking}
        loading={poolMonitor.loading || monitorAvailable && !poolMonitor.loaded && !poolMonitor.error}
        available={monitorAvailable} targetAvailability={{ sub2api: features.sub2apiMonitor === true, cpa: features.cpaUpload === true }}
        checkDisabled={poolMonitor.dirty || poolMonitor.uncertain || poolMonitor.conflict || poolMonitor.saving || !poolMonitor.loaded}
        queryOverview={<AccountInsightsMonitor embedded insights={insights} onConfigure={() => { if (navigatePage("settings") !== false) selectSettingsSection("insights"); }} />} />}
      {batchOpen && (
        <DialogFrame onClose={() => setBatchOpen(false)} busy={batchBusy}>
          <form className="batch-dialog" onSubmit={createBatch} role="dialog" aria-modal="true" aria-labelledby="batch-title">
            <div className="dialog-header">
              <div>
                <h2 id="batch-title">批量导入账号</h2>
                <span>{countBatchLines(batchText)} 条，仅导入列表；稍后自行选择要执行的功能</span>
              </div>
              <button type="button" className="icon-button" onClick={() => setBatchOpen(false)} disabled={batchBusy} title="关闭">
                <X size={18} />
              </button>
            </div>
            <label className="batch-label" htmlFor="batch-input">
              {mailRequestSettings.method === "POST"
                ? "每行：邮箱----编码请求体，或 邮箱----密码----编码请求体；邮箱及可识别字段顺序不限"
                : "每行一个账号：邮箱----密码----2FA----到期时间"}
            </label>
            <p className="batch-expiration-help" id="batch-expiration-help">
              <span><code>16.30</code> 或 <code>16:30</code> 表示导入当天 16:30；<code>7-18 08:00</code> 或 <code>7-18 8:00</code> 表示导入当年 7 月 18 日 08:00。已过则显示已到期，不顺延到明天或明年。也支持 <code>2026-12-31 16:30</code> 等完整日期时间。</span>
              <span>按当前浏览器时区{browserTimeZone() ? ` ${browserTimeZone()}` : "（暂未识别）"}解析。手动时间优先；未填时使用真实套餐到期时间，未知则如实显示。</span>
              <span>仍兼容原有邮箱收码 API / 编码请求体格式，可在末尾的 2FA 后追加到期时间；重复导入未填到期时间时保留原手动值，清除请在账号信息中操作。</span>
            </p>
            <textarea
              id="batch-input"
              value={batchText}
              onChange={(event) => setBatchText(event.target.value)}
              placeholder={mailRequestSettings.method === "POST"
                ? "a@example.com----eyJtYWlsYm94X2lkIjoiaWQtYSJ9\nb@example.com----账号密码----mailbox_id%3Did-b"
                : "name@example.com----账号密码----BASE32二步验证密钥----16.30\nname2@example.com----账号密码----BASE32二步验证密钥----2026-12-31 16:30\nname3@example.com----https://mail.example/messages/name3----账号密码----BASE32二步验证密钥----2026-12-31T16:30"}
              aria-describedby="batch-expiration-help"
              spellCheck="false"
              autoFocus
            />
            {batchError && <div className="dialog-error" role="alert"><CircleAlert size={15} />{batchError}</div>}
            <div className="dialog-footer">
              <button type="button" className="cancel-button" onClick={() => setBatchOpen(false)} disabled={batchBusy}>取消</button>
              <button type="submit" className="primary-button" disabled={!batchText.trim() || batchBusy || countBatchLines(batchText) > 500}>
                {batchBusy ? <LoaderCircle className="spin" size={17} /> : <ListPlus size={17} />}
                导入 {countBatchLines(batchText)} 个账号
              </button>
            </div>
          </form>
        </DialogFrame>
      )}
      {filterOpen && (
        <DialogFrame onClose={() => setFilterOpen(false)}>
          <form className="batch-dialog filter-dialog" onSubmit={applyEmailFilter} role="dialog" aria-modal="true" aria-labelledby="filter-title">
            <div className="dialog-header">
              <div>
                <h2 id="filter-title">筛选账号</h2>
                <span>{countBatchLines(filterText)} 个邮箱</span>
              </div>
              <button type="button" className="icon-button" onClick={() => setFilterOpen(false)} title="关闭">
                <X size={18} />
              </button>
            </div>
            <label className="batch-label" htmlFor="filter-input">每行输入一个完整邮箱地址</label>
            <textarea
              id="filter-input"
              value={filterText}
              onChange={(event) => setFilterText(event.target.value)}
              placeholder={"name1@icloud.com\nname2@icloud.com"}
              spellCheck="false"
              autoFocus
            />
            {filterError && <div className="dialog-error" role="alert"><CircleAlert size={15} />{filterError}</div>}
            <div className="dialog-footer">
              <button type="button" className="cancel-button" onClick={() => setFilterOpen(false)}>取消</button>
              <button type="submit" className="primary-button" disabled={!filterText.trim() || countBatchLines(filterText) > 500}>
                <Filter size={17} />应用筛选
              </button>
            </div>
          </form>
        </DialogFrame>
      )}
    </AppShell>
  );
}

const appRoot = globalThis.__chatgptOnboardingRoot || createRoot(document.getElementById("root"));
globalThis.__chatgptOnboardingRoot = appRoot;
appRoot.render(<ConfirmationProvider><App /></ConfirmationProvider>);
