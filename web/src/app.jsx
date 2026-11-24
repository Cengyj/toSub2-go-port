import React, { useEffect, useRef, useState } from "react";
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
  BROWSER_SETTINGS_KEYS,
  BROWSER_SECRET_SETTINGS_KEYS,
  countBatchLines,
  parseEmailFilter,
} from "./lib/console.js";
import { browserTimeZone } from "./lib/accountExpiration.js";
import { uploadTargetLabel } from "./lib/uploadTarget.js";
import { CPASettings } from "./components/cpa-settings.jsx";
import { ManualUploadDialog } from "./components/upload-target.jsx";
import { useCPASettings } from "./hooks/cpaSettings.js";
import { usePoolMonitor } from "./hooks/poolMonitor.js";
import { useAccountPolling } from "./hooks/serverState.js";
import { useAccountInsights } from "./hooks/accountInsights.js";
import { useAccountSubscription } from "./hooks/accountSubscription.js";
import { subscriptionResponseIsCurrent } from "./lib/accountSubscription.js";
import { useAccountQuality } from "./hooks/accountQuality.js";
import { describeQualityReceipt } from "./lib/accountQuality.js";
import { AccountQualitySettings, AccountQualityDialog } from "./components/account-quality.jsx";
import { useNotice } from "./hooks/notice.js";
import { useAccountTrash } from "./hooks/accountTrash.js";
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
  useConfirmation,
} from "./components/ui.jsx";

import { useSmsSettings } from "./hooks/smsSettings.js";
import { useMailSettings } from "./hooks/mailSettings.js";
import { useSub2ApiSettings } from "./hooks/sub2apiSettings.js";
import { useAccountCollection } from "./hooks/accountCollection.js";
import { useAccountCommands } from "./hooks/accountCommands.js";
import { useConsoleConnection } from "./hooks/consoleConnection.js";
import { useUploadPreference } from "./hooks/uploadPreference.js";
import { useProxySettings } from "./hooks/proxySettings.js";
import { useMaintenance } from "./hooks/maintenance.js";
import { MaintenanceSettings } from "./components/maintenance.jsx";

export function App() {
  const confirm = useConfirmation();
  const { connected, setConnected, token, features, connectionError, setConnectionError } = useConsoleConnection();
  const accounts = useAccountCollection();
  const { jobs, setJobs, expandedJobId, setExpandedJobId, selectedJobIds, setSelectedJobIds, setJobSelectionIndex, page, setPage, pagination, setPagination, stats, setStats,
    pageJobIds, downloadableSelectedCount, allPageSelected, canDownloadSelected, canReauthorizeSelected, hasImportedSelected, forceReloginSelectedCount, canForceReloginSelected, uploadBlockedSelected, canUploadSelected, totpSetupSelectedCount, canSetupTotpSelected, totpResetSelectedCount, canResetTotpSelected, canAddPasswordSelected, toggleJobSelection, toggleAllOnPage } = accounts;
  const { uploadTarget, setUploadTarget, uploadTargetSaved, setUploadTargetSaved, selectUploadTarget } = useUploadPreference();
  const [activePage, setActivePage] = useState(() => ["accounts", "trash", "settings", "monitor"].includes(location.hash.slice(1)) ? location.hash.slice(1) : "accounts");
  const [settingsTab, setSettingsTab] = useState("overview");
  const [pendingNavigation, setPendingNavigation] = useState(null);
  const [theme, setTheme, themeStorageError] = useTheme();
  function selectMonitorTarget(value) {
    if (!["sub2api", "cpa"].includes(value) || poolMonitor.isSaving()) return;
    poolMonitor.setDraft(current => ({ ...current, target: value }));
  }
  const cpa = useCPASettings({ token, enabled: features.cpaUpload === true });
  const anyMonitorAvailable = Boolean(features.sub2apiMonitor || features.cpaUpload);
  const poolMonitor = usePoolMonitor({ token, enabled: anyMonitorAvailable, active: activePage === "monitor" });
  const monitorTarget = poolMonitor.draft?.target || poolMonitor.status?.target || "sub2api";
  const monitorAvailable = monitorTarget === "cpa" ? features.cpaUpload === true : features.sub2apiMonitor === true;
  const uploadAvailable = uploadTarget === "cpa" ? features.cpaUpload === true : features.sub2apiUpload === true;
  const anyUploadAvailable = Boolean(features.sub2apiUpload || features.cpaUpload);
  const chooseManualUploadTarget = Boolean(features.sub2apiUpload && features.cpaUpload);
  const manualUploadTarget = chooseManualUploadTarget ? uploadTarget : features.cpaUpload ? "cpa" : "sub2api";
  const accountsHeadingRef = useRef(null);
  const [filterOpen, setFilterOpen] = useState(false);
  const [filterText, setFilterText] = useState("");
  const [filterError, setFilterError] = useState("");
  const [emailFilter, setEmailFilter] = useState([]);
  const [error, setActionError] = useState("");
  function setError(message) {
    setActionError(message);
    if (message) clearNotice();
  }
  const [accountInfoJob, setAccountInfoJob] = useState(null);
  const [insightJob, setInsightJob] = useState(null);
  const [qualityJob, setQualityJob] = useState(null);
  const { message: uploadNotice, tone: noticeTone, showNotice, clearNotice } = useNotice();
  const sms = useSmsSettings({ token, features, activePage, settingsTab });
  const protectedStorage = features.protectedIntegrationSettings === true;
  const mail = useMailSettings({ token, onError: setError, protectedStorage });
  const sub2api = useSub2ApiSettings({ token, enabled: features.sub2apiMonitor, activePage, settingsTab, protectedStorage });
  const proxy = useProxySettings({ token, protectedStorage });
  const { value: accountProxyUrl, draft: proxyDraft, setDraft: setProxyDraft, save: saveAccountProxy } = proxy;
  const [browserSettingsError, setBrowserSettingsError] = useState("");
  async function clearBrowserSettings() {
    if (!await confirm(protectedStorage ? "确定清除当前浏览器保存的主题和账号操作偏好吗？服务端安全配置、账号及后台功能保持不变；尚未完成安全迁移的浏览器旧秘密也会保留。" : "请先备份需要保留的配置。确定清除当前浏览器保存的接码平台、邮件 API、Sub2API、代理、主题和账号操作偏好吗？服务端任务及号池监控配置需通过本地服务清理操作删除。")) return;
    try {
      for (const key of BROWSER_SETTINGS_KEYS) if (!protectedStorage || !BROWSER_SECRET_SETTINGS_KEYS.includes(key)) window.localStorage.removeItem(key);
    } catch {
      setBrowserSettingsError("网页配置未能全部清除，请检查浏览器的站点存储权限后重试。");
      return;
    }
    if (!protectedStorage) { sms.clear(); mail.clear(); sub2api.clear(); proxy.clear(); }
    setUploadTarget("sub2api");
    setUploadTargetSaved(true);
    setTheme("light", false);
    commands.clearDrafts();
    setAccountInfoJob(null);
    setExpandedJobId(null);
    setBrowserSettingsError("");
    showNotice(protectedStorage ? "已清除当前浏览器的主题和操作偏好；服务端安全配置、账号与后台功能保持不变。" : "已清除当前浏览器保存的网页配置及账号操作偏好。服务端数据请通过本地服务清理操作删除。", "success");
  }

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

  const uploadConfigured = uploadTarget === "cpa" ? cpa.configured : Boolean(sub2api.settings.baseUrl.trim() && sub2api.settings.adminApiKey.trim());
  const uploadTargets = { sub2api: { available: Boolean(features.sub2apiUpload), configured: Boolean(sub2api.settings.baseUrl.trim() && sub2api.settings.adminApiKey.trim()) }, cpa: { available: features.cpaUpload === true, configured: cpa.configured } };
  uploadTargets.sub2api.ready = uploadTargets.sub2api.configured && sub2api.ready && !sub2api.saving;
  uploadTargets.sub2api.status = sub2api.saving ? "正在保存配置" : !sub2api.ready ? "配置待核实" : uploadTargets.sub2api.configured ? "已配置" : "未配置";
  uploadTargets.cpa.ready = cpa.configured && !cpa.loading && !cpa.saving && !cpa.error;
  uploadTargets.cpa.status = cpa.loading ? "正在读取配置" : cpa.saving ? "正在保存配置" : cpa.uncertain || cpa.error ? "配置待核实" : !cpa.loaded ? "尚未读取配置" : cpa.configured ? "已配置" : "未配置";

  const commands = useAccountCommands({ token, features, accounts: { ...accounts, refresh: refreshAccounts },
    settings: { proxy, mail, sub2api, cpa, uploadTargets, uploadTarget, selectUploadTarget },
    feedback: { setError, showNotice }, confirm, trash, headingRef: accountsHeadingRef,
    navigation: { imported() { setEmailFilter([]); setFilterText(""); }, configure(target) { if (navigatePage("settings") !== false) selectSettingsSection(target); } },
  });
  const { email, setEmail, batchOpen, setBatchOpen, batchText, setBatchText, batchBusy, batchError, setBatchError, batchAction,
    totpAction, setTotpAction, reloginAction, setReloginAction, manualUploadAction, setManualUploadAction, setManualUploadTarget,
    createJob, createBatch, manualUploadProblem, requestManualUpload, submitManualUpload, downloadSelected, reauthorizeSelected,
    setupTotpSelected, setupTotpAccount, submitTotpAction, addPasswordSelected, forceReloginSelected, forceReloginAccount,
    authorizeAccount, submitReloginAction, exportSelectedSource, deleteAccounts, deleteSelected, cancelAllRunningJobs } = commands;

  const settingsSection = sms.isOpen ? "sms" : mail.isOpen ? "mail" : sub2api.isOpen ? "sub2api" : settingsTab;
  const maintenance = useMaintenance({ token, enabled: features.maintenance === true, active: activePage === "settings" && settingsSection === "overview" });
  // Only writes block navigation. Leaving a read-only editor cancels its scoped read.
  const settingsSaving = sms.saving || proxy.saving || mail.saving || sub2api.saving || maintenance.saving || insights.saving || trash.saving || quality.saving || cpa.saving || poolMonitor.saving || poolMonitor.checking;
  const settingsDirty = sms.isOpen && sms.dirty || mail.isOpen && mail.dirty || sub2api.isOpen && sub2api.dirty
    || (activePage === "settings" && settingsTab === "network" && proxyDraft !== accountProxyUrl)
    || (activePage === "settings" && settingsTab === "insights" && insights.dirty)
    || (activePage === "settings" && settingsTab === "trash" && trash.dirty)
    || (activePage === "settings" && settingsTab === "quality" && quality.dirty)
    || (activePage === "settings" && settingsTab === "cpa" && cpa.dirty)
    || (activePage === "monitor" && poolMonitor.dirty);

  function leaveSettings(action) {
    if (settingsSaving || sms.isSaving() || proxy.isSaving() || mail.isSaving() || sub2api.isSaving() || maintenance.isSaving() || cpa.isSaving() || poolMonitor.isSaving()) return false;
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
    sms.close();
    mail.close();
    sub2api.close();
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
      if (next === "sms") sms.open();
      else if (next === "mail") mail.open();
      else if (next === "sub2api") sub2api.open();
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
      if (monitorTarget === "sub2api") sub2api.open();
      else { setSettingsTab("cpa"); if (!cpa.loading) void cpa.reload(); }
      window.scrollTo({ top: 0 });
    });
  }
  useEffect(() => {
    const onHash = () => {
      const requested = location.hash.slice(1);
      navigatePage(["accounts", "trash", "settings", "monitor"].includes(requested) ? requested : "accounts", true);
    };
    const onUnload = (event) => { if (settingsDirty || settingsSaving) { event.preventDefault(); event.returnValue = ""; } };
    window.addEventListener("hashchange", onHash);
    window.addEventListener("beforeunload", onUnload);
    return () => { window.removeEventListener("hashchange", onHash); window.removeEventListener("beforeunload", onUnload); };
  }, [activePage, settingsDirty, settingsSaving]);

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

  return (
    <AppShell page={activePage} trashAvailable={features.accountTrash === true} onNavigate={navigatePage} theme={theme} onThemeChange={setTheme} connected={connected} total={pagination.totalAll ?? pagination.total}>
        {themeStorageError && (
          <div className="global-notice warning" role="status"><CircleAlert size={17} /><span>{themeStorageError}</span></div>
        )}
        {connectionError && (
          <div className="global-notice warning" role="status"><CircleAlert size={17} /><span>状态同步暂时中断：{connectionError}。页面保留最近一次数据。</span></div>
        )}
        {sub2api.monitorConnectionError && !connectionError && !(activePage === "monitor" && monitorTarget === "cpa") && (
          <div className="global-notice warning" role="status"><CircleAlert size={17} /><span>Sub2API 监控状态暂未更新：{sub2api.monitorConnectionError}。页面保留最近一次巡检结果。</span></div>
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
            <button className="primary-button" type="submit" disabled={!token}>
              <Plus size={17} />
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
                <button type="button" className="secondary-button bulk-button" aria-label={chooseManualUploadTarget ? "选择上传目标" : `上传 ${uploadTargetLabel(manualUploadTarget)}`} onClick={() => requestManualUpload([...selectedJobIds])} disabled={!canUploadSelected || Boolean(batchAction)} title={uploadBlockedSelected ? "请等待选中账号的当前流程结束或完成本地保存" : chooseManualUploadTarget ? "选择 Sub2API 或 CPA 后上传选中账号" : manualUploadTarget === "cpa" ? "推送选中账号到 CPA；同名授权文件将替换，使用当前优先级和权重" : "上传选中账号；已有账号更新授权并保留配置"}>
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
                    smsProviderAvailable={sms.definitions.length > 0}
                    smsProvider={sms.provider}
                    onUpload={() => requestManualUpload([job.id])}
                    sub2apiUploadAvailable={anyUploadAvailable}
                    uploadTarget={manualUploadTarget}
                    chooseUploadTarget={chooseManualUploadTarget}
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
                    accountProxyReady={proxy.ready}
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
      {manualUploadAction && <ManualUploadDialog action={manualUploadAction} targets={uploadTargets} problem={manualUploadProblem(manualUploadAction.ids, manualUploadAction.target)} onTargetChange={setManualUploadTarget} onSubmit={submitManualUpload} onClose={() => setManualUploadAction(null)} onConfigure={target => { setManualUploadAction(null); if (navigatePage("settings") !== false) selectSettingsSection(target); }} onReloadCPA={cpa.reload} cpaLoading={cpa.loading || cpa.saving} />}
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
      {activePage === "settings" && <ConfigurationPage cpaAvailable={features.cpaUpload === true} cpa={cpa} qualityAvailable={features.accountQuality === true} quality={quality} trashAvailable={features.accountTrash === true} trashSettings={trash.settings} section={settingsSection} onSectionChange={selectSettingsSection} dirty={settingsDirty} saving={settingsSaving} sms={sms.provider} mail={mail.settings} sub2api={sub2api.settings} proxy={accountProxyUrl} proxyDraft={proxyDraft} onProxyDraftChange={setProxyDraft} onProxySave={saveAccountProxy} proxyProtection={proxy.protection} theme={theme} onThemeChange={setTheme} onClear={clearBrowserSettings} clearDisabled={batchBusy || Boolean(batchAction) || settingsSaving} clearError={browserSettingsError} insightSettings={insights.settings} insightSettingsLoaded={insights.settingsLoaded} insightSettingsError={insights.settingsError} unconfirmed={settingsSection === "insights" && insights.uncertain || settingsSection === "trash" && (trash.uncertain || trash.persistenceUnconfirmed) || settingsSection === "quality" && quality.uncertain || settingsSection === "cpa" && cpa.uncertain}>
        {settingsSection === "overview" && <MaintenanceSettings maintenance={maintenance} />}
        {settingsSection === "cpa" && features.cpaUpload === true && <CPASettings cpa={cpa} />}
        {settingsSection === "insights" && <AccountInsightsSettings insights={insights} />}
        {settingsSection === "quality" && features.accountQuality === true && <AccountQualitySettings quality={quality} jobs={jobs} />}
        {settingsSection === "trash" && features.accountTrash === true && <AccountTrashSettings trash={trash} />}
        {sms.isOpen && <SmsSettingsDialog inline {...sms.editor} setSmsSettingsOpen={closeSmsSettings} />}
        {mail.isOpen && <MailSettingsDialog inline {...mail.editor} setMailRequestSettingsOpen={closeMailSettings} />}
        {sub2api.isOpen && <Sub2ApiSettingsDialog inline {...sub2api.editor} features={features} setSub2apiSettingsOpen={closeSub2ApiSettings} onOpenQuality={() => selectSettingsSection("quality")} />}
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
              {mail.settings.method === "POST"
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
              placeholder={mail.settings.method === "POST"
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
