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
  POLL_INTERVAL_MS,
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
import { describeMonitorResult } from "./lib/jobPresentation.js";
import {
  EmptyState,
  TotpActionDialog,
  ReloginActionDialog,
  AccountInfoDialog,
  JobRow,
} from "./components/accounts.jsx";
import { JobLogs } from "./components/job-logs.jsx";
import { authorizationMethodLabel, normalizeAuthorizationMethod } from "./components/authorization-method.jsx";
import { uploadBlockedJob, describeUpload } from "./lib/jobPresentation.js";
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
  PageHeader,
  DialogFrame,
  ActionMenu,
  ConfirmationProvider,
  useConfirmation,
} from "./components/ui.jsx";


function App() {
  const confirm = useConfirmation();
  const [activePage, setActivePage] = useState(() => ["accounts", "settings", "monitor"].includes(location.hash.slice(1)) ? location.hash.slice(1) : "accounts");
  const [settingsTab, setSettingsTab] = useState("overview");
  const [pendingNavigation, setPendingNavigation] = useState(null);
  const [theme, setTheme] = useTheme();
  const [connected, setConnected] = useState(false);
  const [token, setToken] = useState("");
  const [features, setFeatures] = useState({});
  const [jobs, setJobs] = useState([]);
  const [email, setEmail] = useState("");
  const [busy, setBusy] = useState(false);
  const [batchOpen, setBatchOpen] = useState(false);
  const [batchText, setBatchText] = useState("");
  const [batchBusy, setBatchBusy] = useState(false);
  const [batchError, setBatchError] = useState("");
  const [filterOpen, setFilterOpen] = useState(false);
  const [filterText, setFilterText] = useState("");
  const [filterError, setFilterError] = useState("");
  const [emailFilter, setEmailFilter] = useState([]);
  const [error, setActionError] = useState("");
  function setError(message) {
    setActionError(message);
    if (message) setUploadNotice("");
  }
  const [connectionError, setConnectionError] = useState("");
  const [expandedJobId, setExpandedJobId] = useState(null);
  const [selectedJobIds, setSelectedJobIds] = useState(() => new Set());
  const [jobSelectionIndex, setJobSelectionIndex] = useState([]);
  const [batchAction, setBatchAction] = useState("");
  const [totpAction, setTotpAction] = useState(null);
  const totpSubmission = useRef(false);
  const [reloginAction, setReloginAction] = useState(null);
  const reloginSubmission = useRef(false);
  const [accountInfoJob, setAccountInfoJob] = useState(null);
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
  const [mailRequestSettingsDraft, setMailRequestSettingsDraft] = useState(readMailRequestSettings);
  const [mailRequestSettingsOpen, setMailRequestSettingsOpen] = useState(false);
  const [mailRequestSettingsError, setMailRequestSettingsError] = useState("");
  const [mailRequestSettingsSaving, setMailRequestSettingsSaving] = useState(false);
  const [sub2apiSettings, setSub2apiSettings] = useState(readSub2ApiSettings);
  const [sub2apiSettingsDraft, setSub2apiSettingsDraft] = useState(readSub2ApiSettings);
  const previousSub2apiSettings = useRef(sub2apiSettings);
  const [sub2apiGroups, setSub2apiGroups] = useState([]);
  const [sub2apiProxies, setSub2apiProxies] = useState([]);
  const [sub2apiSettingsOpen, setSub2apiSettingsOpen] = useState(false);
  const [sub2apiSettingsError, setSub2apiSettingsError] = useState("");
  const [sub2apiGroupsLoading, setSub2apiGroupsLoading] = useState(false);
  const [sub2apiSettingsSaving, setSub2apiSettingsSaving] = useState(false);
  const [sub2apiMonitorChecking, setSub2apiMonitorChecking] = useState(false);
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
  const [uploadNotice, setUploadNotice] = useState("");
  const [noticeTone, setNoticeTone] = useState("info");
  function showNotice(message, tone = "info") {
    setUploadNotice(message);
    setNoticeTone(tone);
  }
  const [accountProxyUrl, setAccountProxyUrl] = useState(() => readLocalTextSetting(ACCOUNT_PROXY_STORAGE_KEY));
  const [proxyDraft, setProxyDraft] = useState(() => readLocalTextSetting(ACCOUNT_PROXY_STORAGE_KEY));
  const [browserSettingsError, setBrowserSettingsError] = useState("");

  useEffect(() => {
    const previous = previousSub2apiSettings.current;
    previousSub2apiSettings.current = sub2apiSettings;
    setSub2apiSettingsDraft((current) => JSON.stringify(current) === JSON.stringify(previous)
      ? sub2apiSettings
      : current);
  }, [sub2apiSettings]);

  async function clearBrowserSettings() {
    if (!await confirm("请先备份需要保留的配置。确定清除当前浏览器保存的接码平台、邮件 API、Sub2API、代理配置和账号操作偏好吗？服务端任务及号池监控配置需通过本地服务清理操作删除。")) return;
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
    setSub2apiSettingsDraft(sub2api);
    setSub2apiSettingsOpen(false);
    setSub2apiSettingsError("");
    setSub2apiGroups([]);
    setSub2apiProxies([]);
    setAccountProxyUrl("");
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
    let stopped = false;
    fetch("/api/bootstrap")
      .then(readResponse)
      .then((data) => {
        if (!stopped) {
          setToken(data.token);
          setFeatures(data.features || {});
        }
      })
      .catch((requestError) => { if (!stopped) setConnectionError(requestError.message); });
    return () => {
      stopped = true;
    };
  }, []);

  useEffect(() => {
    if (!token) return;
    void apiFetch(token, "/api/mail-request-config", {
      method: "POST",
      body: JSON.stringify({ config: buildMailRequestConfig(mailRequestSettings) }),
    }).catch((requestError) => setError(requestError.message));
  }, [token]);

  useEffect(() => {
    if (!token) return undefined;
    let stopped = false;
    let timer;
    const poll = async () => {
      try {
        const data = emailFilter.length
          ? await apiFetch(token, "/api/jobs/query", {
              method: "POST",
              body: JSON.stringify({ page, emails: emailFilter }),
            })
          : await apiFetch(token, `/api/jobs?page=${page}`);
        if (!stopped) {
          setJobs(data.jobs);
          setJobSelectionIndex(data.selection || data.jobs);
          setPagination(data.pagination || { page, pageSize: 20, total: data.jobs.length, totalPages: 1 });
          setStats(data.stats || { active: 0, queued: 0, completed: 0 });
          setConnected(true);
          if (data.pagination?.page && data.pagination.page !== page) setPage(data.pagination.page);
          setConnectionError("");
        }
      } catch (requestError) {
        if (!stopped) { setConnectionError(requestError.message); setConnected(false); }
      } finally {
        if (!stopped) timer = window.setTimeout(poll, POLL_INTERVAL_MS);
      }
    };
    poll();
    return () => {
      stopped = true;
      window.clearTimeout(timer);
    };
  }, [token, page, emailFilter]);

  useEffect(() => {
    if (!token || !features.sub2apiMonitor) return undefined;
    let stopped = false;
    const load = async () => {
      const configVersion = sub2apiMonitorConfigVersion.current;
      try {
        const data = await apiFetch(token, "/api/sub2api/monitor");
        if (!stopped && configVersion === sub2apiMonitorConfigVersion.current) {
          setSub2apiMonitorStatus(data);
          setSub2apiSettings((current) => withMonitorSettings(current, data));
        }
      } catch {}
    };
    void load();
    const timer = window.setInterval(load, 5_000);
    return () => {
      stopped = true;
      window.clearInterval(timer);
    };
  }, [token, features.sub2apiMonitor]);

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
    && downloadableSelectedCount > 0;
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

  const settingsSection = smsSettingsOpen ? "sms" : mailRequestSettingsOpen ? "mail" : sub2apiSettingsOpen ? "sub2api" : settingsTab;
  const settingsSaving = mailRequestSettingsSaving || sub2apiSettingsSaving || smsOptionsLoading || sub2apiGroupsLoading;
  const settingsDirty = (smsSettingsOpen && JSON.stringify(withSmsProviderDefaults(smsProviderDefinitions, smsSettingsDraft)) !== JSON.stringify(withSmsProviderDefaults(smsProviderDefinitions, smsSettings)))
    || (mailRequestSettingsOpen && JSON.stringify(mailRequestSettingsDraft) !== JSON.stringify(mailRequestSettings))
    || (sub2apiSettingsOpen && JSON.stringify(sub2apiSettingsDraft) !== JSON.stringify(sub2apiSettings))
    || (activePage === "settings" && settingsTab === "network" && proxyDraft !== accountProxyUrl);

  function leaveSettings(action) {
    if (settingsSaving) return false;
    if (settingsDirty) {
      setPendingNavigation(() => action);
      return false;
    }
    action();
    return true;
  }
  function resetSettingsEditors() {
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
      else setSettingsTab(next);
    });
  }
  function closeSmsSettings() { leaveSettings(resetSettingsEditors); }
  function closeMailSettings() { leaveSettings(resetSettingsEditors); }
  function closeSub2ApiSettings() { leaveSettings(resetSettingsEditors); }
  useEffect(() => {
    const onHash = () => {
      const requested = location.hash.slice(1);
      navigatePage(["accounts", "settings", "monitor"].includes(requested) ? requested : "accounts", true);
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

  async function saveMailRequestSettings(event) {
    event.preventDefault();
    let config;
    try {
      config = buildMailRequestConfig(mailRequestSettingsDraft);
    } catch (requestError) {
      setMailRequestSettingsError(requestError.message);
      return;
    }
    setMailRequestSettingsSaving(true);
    setMailRequestSettingsError("");
    try {
      await apiFetch(token, "/api/mail-request-config", {
        method: "POST",
        body: JSON.stringify({ config }),
      });
      const nextSettings = normalizeMailRequestSettings(mailRequestSettingsDraft);
      writeLocalJson(MAIL_REQUEST_SETTINGS_KEY, nextSettings);
      setMailRequestSettings(nextSettings);
      setMailRequestSettingsOpen(false);
    } catch (requestError) {
      setMailRequestSettingsError(requestError.message);
    } finally {
      setMailRequestSettingsSaving(false);
    }
  }

  function openSub2ApiSettings() {
    setSub2apiSettingsDraft(withMonitorSettings(sub2apiSettings, sub2apiMonitorStatus));
    setSub2apiSettingsError("");
    setSub2apiSettingsOpen(true);
  }

  async function loadSub2ApiOptions(settings = sub2apiSettingsDraft) {
    setSub2apiGroupsLoading(true);
    setSub2apiSettingsError("");
    try {
      const data = await apiFetch(token, "/api/sub2api/options", {
        method: "POST",
        body: JSON.stringify({ config: settings }),
      });
      setSub2apiGroups(Array.isArray(data.groups) ? data.groups : []);
      setSub2apiProxies(Array.isArray(data.proxies) ? data.proxies : []);
    } catch (requestError) {
      setSub2apiGroups([]);
      setSub2apiProxies([]);
      setSub2apiSettingsError(requestError.message);
    } finally {
      setSub2apiGroupsLoading(false);
    }
  }

  async function saveSub2ApiSettings(event) {
    event.preventDefault();
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
    setSub2apiSettingsSaving(true);
    sub2apiMonitorConfigVersion.current += 1;
    try {
      const monitor = await apiFetch(token, "/api/sub2api/monitor", {
        method: "POST",
        body: JSON.stringify({ enabled: nextSettings.monitorEnabled, config: nextSettings }),
      });
      sub2apiMonitorConfigVersion.current += 1;
      const savedSettings = withMonitorSettings(nextSettings, monitor);
      writeLocalJson(SUB2API_UPLOAD_SETTINGS_KEY, savedSettings);
      setSub2apiSettings(savedSettings);
      setSub2apiSettingsDraft(savedSettings);
      setSub2apiMonitorStatus(monitor);
      setSub2apiSettingsOpen(false);
      setSub2apiSettingsError("");
    } catch (requestError) {
      setSub2apiSettingsError(requestError.message);
    } finally {
      setSub2apiSettingsSaving(false);
    }
  }

  async function checkSub2ApiMonitorNow() {
    if (!sub2apiMonitorStatus.enabled || sub2apiMonitorChecking) return;
    setSub2apiMonitorChecking(true);
    setUploadNotice("");
    setError("");
    setSub2apiSettingsError("");
    try {
      const data = await apiFetch(token, "/api/sub2api/monitor/check", { method: "POST" });
      setSub2apiMonitorStatus(data);
      const outcome = describeMonitorResult(data.result, data.lastError);
      showNotice(`${outcome.label}：${outcome.text}`, outcome.tone);
      setError("");
    } catch (requestError) {
      if (sub2apiSettingsOpen) setSub2apiSettingsError(requestError.message);
      else setError(requestError.message);
    } finally {
      setSub2apiMonitorChecking(false);
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
    if (jobSelectionIndex.some((job) => requested.has(job.id) && uploadBlockedJob(job))) {
      showNotice("请等待选中账号的当前流程结束或恢复完成，再上传最新授权。", "warning");
      return;
    }
    if (!sub2apiSettings.baseUrl || !sub2apiSettings.adminApiKey) {
      if (navigatePage("settings") !== false) selectSettingsSection("sub2api");
      showNotice("请先配置 Sub2API 后端地址、管理员 API Key 和目标号池", "warning");
      return;
    }
    if (batchAction) return;
    setBatchAction("upload");
    setError("");
    showNotice("正在同步 Sub2API：检查已有账号并更新授权，请稍候。", "active");
    try {
      const data = await apiFetch(token, "/api/sub2api/upload", {
        method: "POST",
        body: JSON.stringify({ ids, config: sub2apiSettings }),
      });
      const presentation = describeUpload(data);
      showNotice(`${presentation.label}：${presentation.text}`, presentation.tone);
      setSelectedJobIds(new Set());
      setError("");
    } catch (requestError) {
      setError(requestError.message);
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
    setSmsOptionsLoading(true);
    setSmsSettingsError("");
    try {
      const data = await apiFetch(token, resolved.definition.optionsEndpoint, {
        method: "POST",
        body: JSON.stringify({ config: resolved.config }),
      });
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
      setSmsNumberOptions([]);
      setSmsSettingsError(requestError.message);
    } finally {
      setSmsOptionsLoading(false);
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
    writeLocalJson(SMS_PROVIDER_SETTINGS_KEY, nextSettings);
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
    if (!batchText.trim() || batchBusy) return;
    setBatchBusy(true);
    try {
      const data = await apiFetch(token, "/api/jobs/batch", {
        method: "POST",
        body: JSON.stringify({ text: batchText, ...(accountProxyUrl.trim() ? { proxyUrl: accountProxyUrl.trim() } : {}) }),
      });
      setEmailFilter([]);
      setFilterText("");
      setSelectedJobIds(new Set());
      setPage(1);
      if (page === 1) setJobs((current) => mergeJobs(data.jobs, current).slice(0, 20));
      showNotice(`已导入 ${data.created} 个账号${data.updated ? `，更新 ${data.updated} 个账号资料` : ""}。账号已加入列表，请勾选后手动选择要执行的功能。`, "success");
      setBatchText("");
      setBatchError("");
      setBatchOpen(false);
      setError("");
    } catch (requestError) {
      setBatchError(requestError.message);
    } finally {
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
    if (!canDownloadSelected || batchAction) return;
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
    setTotpAction({ ids: [...selectedJobIds], resetTotp, batch: true, eligibleCount, skipped: selectedJobIds.size - eligibleCount, authorizationMethod: selectedAuthorizationMethod });
  }

  function setupTotpAccount(job, resetTotp) {
    if (batchAction || totpAction || reloginAction || !(resetTotp ? job.canResetTotp : job.canSetupTotp)) return;
    setTotpAction({ ids: [job.id], resetTotp, batch: false, email: job.email, eligibleCount: 1, skipped: 0, authorizationMethod: job.authorizationMethod });
  }

  async function submitTotpAction(options) {
    if (!totpAction || totpSubmission.current) return;
    totpSubmission.current = true;
    const { ids, resetTotp, batch } = totpAction;
    const reloginAfterTotp = Boolean(features.forceRelogin && options.reloginAfterTotp);
    const uploadAfterTotp = Boolean(features.sub2apiUpload && reloginAfterTotp && options.uploadAfterTotp && sub2apiSettings.baseUrl.trim() && sub2apiSettings.adminApiKey.trim());
    setBatchAction(resetTotp ? "reset-2fa" : "setup-2fa");
    try {
      const data = await apiFetch(token, batch ? "/api/jobs/setup-2fa-batch" : `/api/jobs/${ids[0]}/setup-2fa`, {
        method: "POST",
        body: JSON.stringify({
          ...(batch ? { ids } : {}),
          proxyUrl: accountProxyUrl.trim(),
          resetTotp,
          logoutAllDevicesAfterTotp: Boolean(resetTotp && features.logoutAllDevicesAfterTotp && options.logoutAllDevicesAfterTotp),
          reloginAfterTotp,
          ...(reloginAfterTotp ? { authorizationMethod: options.authorizationMethod } : {}),
          uploadAfterTotp,
          ...(uploadAfterTotp ? { sub2apiConfig: sub2apiSettings } : {}),
        }),
      });
      showNotice(`已加入 ${batch ? data.started : 1} 个账号的 2FA 流程${data.skipped ? `，跳过 ${data.skipped} 个` : ""}，当前状态见账号行，详细步骤见日志。`, data.skipped ? "warning" : "info");
      setTotpAction(null);
      if (batch) setSelectedJobIds(new Set());
      setError("");
    } finally {
      totpSubmission.current = false;
      setBatchAction("");
    }
  }

  async function addPasswordSelected() {
    if (!canAddPasswordSelected || batchAction) return;
    const skipped = selectedJobIds.size - passwordAddSelectedCount;
    const message = `确定为选中的 ${passwordAddSelectedCount} 个无密码账号生成并添加随机强密码吗？${skipped ? `另有 ${skipped} 个账号已有密码或不符合条件，将自动跳过。` : ""}`;
    if (!await confirm(message)) return;
    setBatchAction("add-password");
    try {
      const data = await apiFetch(token, "/api/jobs/add-password-batch", {
        method: "POST",
        body: JSON.stringify({ ids: [...selectedJobIds], proxyUrl: accountProxyUrl.trim() }),
      });
      showNotice(`已加入 ${data.started} 个账号的添加密码任务${data.skipped ? `，跳过 ${data.skipped} 个` : ""}`, data.skipped ? "warning" : "info");
      setSelectedJobIds(new Set());
      setError("");
    } catch (requestError) {
      setError(requestError.message);
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
    if (!reloginAction || reloginSubmission.current) return;
    reloginSubmission.current = true;
    const { ids, batch, kind = "relogin" } = reloginAction;
    const authorizationMethod = normalizeAuthorizationMethod(options.authorizationMethod);
    const uploadAfterRelogin = Boolean(kind === "relogin" && features.reloginAutoUpload && features.sub2apiUpload && options.uploadAfterRelogin && sub2apiSettings.baseUrl.trim() && sub2apiSettings.adminApiKey.trim());
    setBatchAction(kind);
    try {
      const path = kind === "create" ? "/api/jobs" : batch ? `/api/jobs/${kind}-batch` : `/api/jobs/${ids[0]}/${kind}`;
      const data = await apiFetch(token, path, {
        method: "POST",
        body: JSON.stringify({ ...(kind === "create" ? { email: reloginAction.email } : batch ? { ids } : {}), proxyUrl: accountProxyUrl.trim(), authorizationMethod, ...(kind === "relogin" ? { uploadAfterRelogin } : {}), ...(uploadAfterRelogin ? { sub2apiConfig: sub2apiSettings } : {}) }),
      });
      if (kind === "create") {
        setPage(1);
        if (page === 1) setJobs((current) => mergeJobs([data.job], current).slice(0, 20));
        setEmail("");
      }
      showNotice(`已提交 ${batch ? data.started : 1} 个账号的 ${authorizationMethodLabel(authorizationMethod)} 授权任务${uploadAfterRelogin ? "，成功后自动上传 Sub2API" : ""}${data.skipped ? `，跳过 ${data.skipped} 个` : ""}。请在账号行查看进度。`, data.skipped ? "warning" : "info");
      setReloginAction(null);
      if (batch) setSelectedJobIds(new Set());
      setError("");
    } finally {
      reloginSubmission.current = false;
      setBatchAction("");
    }
  }

  async function exportSelectedSource() {
    if (!selectedJobIds.size || batchAction) return;
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
    if (!ids.length || batchAction) return;
    const requested = new Set(ids);
    const normalizeEmail = (value) => String(value || "").trim().toLowerCase();
    const emails = new Set(jobSelectionIndex.filter((job) => requested.has(job.id)).map((job) => normalizeEmail(job.email)));
    if (emailLabel) emails.add(normalizeEmail(emailLabel));
    if (!await confirm(`确定删除${emailLabel ? `账号 ${emailLabel}` : `选中的 ${emails.size || ids.length} 个账号`}的本地数据吗？同邮箱的历史记录、保存的密码与 2FA、授权文件都会一并删除。Sub2API 中的账号需要另行管理。`, { title: "删除本地账号数据", confirmLabel: "确认删除", danger: true })) return;
    setBatchAction("delete");
    try {
      const data = await apiFetch(token, "/api/jobs/delete-batch", {
        method: "POST",
        body: JSON.stringify({ ids }),
      });
      const isDeleted = (job) => requested.has(job.id) || emails.has(normalizeEmail(job.email));
      const deleted = new Set([...jobs, ...jobSelectionIndex].filter(isDeleted).map((job) => job.id));
      setJobs((current) => current.filter((job) => !isDeleted(job)));
      setJobSelectionIndex((current) => current.filter((job) => !isDeleted(job)));
      setSelectedJobIds((current) => new Set([...current].filter((id) => !deleted.has(id))));
      setExpandedJobId((current) => deleted.has(current) ? null : current);
      setAccountInfoJob((current) => current && isDeleted(current) ? null : current);
      showNotice(`已删除 ${data.deleted ?? emails.size} 个账号的本地数据及同邮箱历史记录。`, "success");
      setError("");
    } catch (requestError) {
      setError(requestError.message);
    } finally {
      setBatchAction("");
    }
  }

  function deleteSelected() {
    return deleteAccounts([...selectedJobIds]);
  }

  async function cancelAllRunningJobs() {
    const runningCount = (stats.active || 0) + (stats.queued || 0);
    if (!runningCount || batchAction) return;
    if (!await confirm(`确定停止全部 ${runningCount} 条进行中和排队任务吗？`)) return;
    setBatchAction("cancel-all");
    try {
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
    <AppShell page={activePage} onNavigate={navigatePage} theme={theme} onThemeChange={setTheme} connected={connected} total={pagination.totalAll ?? pagination.total}>
        {connectionError && (
          <div className="global-notice warning" role="status"><CircleAlert size={17} /><span>状态同步暂时中断：{connectionError}。正在重连，页面保留最近一次数据。</span></div>
        )}
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
            <button type="button" onClick={() => setUploadNotice("")} title="关闭"><X size={16} /></button>
          </div>
        )}


      <section className="accounts-section" hidden={activePage !== "accounts"}>
        <PageHeader title="账号管理" description="管理账号授权与安全验证，随时掌握任务进度。" />
        <AccountsOverview stats={stats} total={pagination.totalAll ?? pagination.total} />
        <div className="section-card account-list-card">
        <div className="account-list-header">
          <div className="account-list-title">
            <h2>账号列表</h2>
            <p>{emailFilter.length
              ? `匹配 ${pagination.total} 条，共 ${pagination.totalAll ?? pagination.total} 条任务`
              : (pagination.total ? `共 ${pagination.total} 条任务` : "添加邮箱或批量导入账号")}</p>
          </div>
          <div className="account-list-tools">
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
              {features.sub2apiUpload && (
                <button type="button" className="secondary-button bulk-button" aria-label="上传 Sub2API" onClick={() => uploadSelected([...selectedJobIds])} disabled={!canUploadSelected || Boolean(batchAction)} title={uploadBlockedSelected ? "请等待选中账号的当前流程结束或恢复完成" : "上传选中账号；已有账号更新授权并保留配置"}>
                  {batchAction === "upload" ? <LoaderCircle className="spin" size={15} /> : <Send size={15} />}
                  上传
                </button>
              )}
              <button type="button" className="delete-button bulk-button" onClick={deleteSelected} disabled={!selectedJobIds.size || Boolean(batchAction)} title="删除选中账号的本地资料和授权文件">
                {batchAction === "delete" ? <LoaderCircle className="spin" size={15} /> : <Trash2 size={15} />}
                删除
              </button>
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

        {jobs.length > 0 && <p className="table-scroll-hint">左右滑动查看完整账号信息</p>}
        <div className="table-frame accounts-table-frame">
          <table className="accounts-table">
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
                <th className="current-operation-heading">当前操作</th>
                <th className="operation-type-heading">操作类型</th>
                <th className="activity-time-heading">时间记录</th>
                <th className="status-heading">状态</th>
                <th className="actions-heading">操作</th>
              </tr>
            </thead>
            <tbody>
              {!jobs.length && <EmptyState filtered={emailFilter.length > 0} />}
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
                    sub2apiUploadAvailable={Boolean(features.sub2apiUpload)}
                    totpSetupAvailable={Boolean(features.totpSetup)}
                    totpResetAvailable={Boolean(features.totpReset)}
                    onSetupTotp={(resetTotp) => setupTotpAccount(job, resetTotp)}
                    passwordAddAvailable={Boolean(features.passwordAdd)}
                    forceReloginAvailable={Boolean(features.forceRelogin)}
                    onRelogin={() => forceReloginAccount(job)}
                    onAuthorize={(kind) => authorizeAccount(job, kind)}
                    onDelete={() => deleteAccounts([job.id], job.email)}
                    onViewAccount={() => setAccountInfoJob(job)}
                    deleteSupported={Boolean(features.bulkActions)}
                    accountInfoAvailable={Boolean(features.accountInfo)}
                    accountActionsBusy={Boolean(batchAction)}
                    accountProxyUrl={accountProxyUrl}
                  />
                  {expandedJobId === job.id && (
                    <tr className="log-row">
                      <td colSpan="7"><JobLogs token={token} jobId={job.id} job={job} /></td>
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
      {totpAction && <TotpActionDialog action={totpAction} logoutAvailable={Boolean(features.logoutAllDevicesAfterTotp)} reloginAvailable={Boolean(features.forceRelogin)} uploadAvailable={Boolean(features.sub2apiUpload)} uploadConfigured={Boolean(sub2apiSettings.baseUrl.trim() && sub2apiSettings.adminApiKey.trim())} onConfigureUpload={() => { setTotpAction(null); if (navigatePage("settings") !== false) selectSettingsSection("sub2api"); }} onSubmit={submitTotpAction} onClose={() => setTotpAction(null)} />}
      {reloginAction && <ReloginActionDialog action={reloginAction} uploadAvailable={Boolean(features.reloginAutoUpload && features.sub2apiUpload)} uploadConfigured={Boolean(sub2apiSettings.baseUrl.trim() && sub2apiSettings.adminApiKey.trim())} onConfigureUpload={() => { setReloginAction(null); if (navigatePage("settings") !== false) selectSettingsSection("sub2api"); }} onSubmit={submitReloginAction} onClose={() => setReloginAction(null)} />}
      {accountInfoJob && <AccountInfoDialog job={accountInfoJob} token={token} onClose={() => setAccountInfoJob(null)} />}
      {activePage === "settings" && <ConfigurationPage section={settingsSection} onSectionChange={selectSettingsSection} dirty={settingsDirty} saving={settingsSaving} sms={activeSmsProvider} mail={mailRequestSettings} sub2api={sub2apiSettings} proxy={accountProxyUrl} proxyDraft={proxyDraft} onProxyDraftChange={setProxyDraft} onProxySave={(value) => {setAccountProxyUrl(value);setProxyDraft(value);writeLocalTextSetting(ACCOUNT_PROXY_STORAGE_KEY, value);}} theme={theme} onThemeChange={setTheme} onClear={clearBrowserSettings} clearDisabled={busy || batchBusy || Boolean(batchAction) || settingsSaving} clearError={browserSettingsError}>
        {smsSettingsOpen && <SmsSettingsDialog inline {...{ setSmsSettingsOpen: closeSmsSettings, saveSmsSettings, smsProviderDefinitions, draftSmsProvider, setSmsSettingsDraft, setSmsSettingsError, smsNumberOptions, smsOptionsLoading, updateSmsProviderConfig, loadSmsNumberOptions, smsSettingsError }} />}
        {mailRequestSettingsOpen && <MailSettingsDialog inline {...{ setMailRequestSettingsOpen: closeMailSettings, mailRequestSettingsSaving, saveMailRequestSettings, mailRequestSettingsDraft, setMailRequestSettingsDraft, mailRequestSettingsError, token }} />}
        {sub2apiSettingsOpen && <Sub2ApiSettingsDialog inline {...{ setSub2apiSettingsOpen: closeSub2ApiSettings, saveSub2ApiSettings, sub2apiSettingsDraft, setSub2apiSettingsDraft, features, sub2apiGroups, setSub2ApiGroupChecked, sub2apiProxies, sub2apiMonitorStatus, sub2apiSettingsError, loadSub2ApiOptions, sub2apiGroupsLoading, token, checkSub2ApiMonitorNow, sub2apiMonitorChecking, sub2apiSettingsSaving }} />}
      </ConfigurationPage>}
      {activePage === "monitor" && <MonitorPage status={sub2apiMonitorStatus} onConfigure={() => { if (navigatePage("settings") !== false) selectSettingsSection("sub2api"); }} onCheck={checkSub2ApiMonitorNow} checking={sub2apiMonitorChecking} available={Boolean(features.sub2apiMonitor)} />}
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
                : "每行一个账号：自动识别邮箱、密码、邮件 API 和 2FA，字段顺序不限"}
            </label>
            <textarea
              id="batch-input"
              value={batchText}
              onChange={(event) => setBatchText(event.target.value)}
              placeholder={mailRequestSettings.method === "POST"
                ? "a@example.com----eyJtYWlsYm94X2lkIjoiaWQtYSJ9\nb@example.com----账号密码----mailbox_id%3Did-b"
                : "name@icloud.com----https://mail.example/messages/name\nhttps://mail.example/messages/name2|账号密码|name2@example.co.uk|BASE32二步验证密钥\nBASE32二步验证密钥::name3@example.dev::账号密码"}
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
