import React, { useEffect, useRef, useState } from "react";
import { UploadTargetPicker } from "./upload-target.jsx";
import { uploadTargetLabel, uploadTargetConflict, CPA_AUTHORIZATION_HELP } from "../lib/uploadTarget.js";
import { PlanBadge } from "./PlanBadge.jsx";
import { AccountExpiration } from "./account-expiration.jsx";
import { AccountInsightSummary } from "./account-insights.jsx";
import { AccountQualityStatus } from "./account-quality.jsx";
import { AccountOperationSummary, operationExecutionHint } from "./account-operation.jsx";
import { getInsightExpirationJob } from "../lib/accountInsights.js";
import { subscriptionDisplayPlan } from "../lib/accountSubscription.js";
import "./account-actions.css";
import {
  ActionMenu,
  DialogFrame,
  useConfirmation,
} from "./ui.jsx";
import {
  Ban,
  Check,
  CheckCircle2,
  ChevronRight,
  CircleAlert,
  Copy,
  Download,
  FileText,
  KeyRound,
  ListPlus,
  LoaderCircle,
  LogIn,
  Mail,
  MailCheck,
  RefreshCw,
  RotateCcw,
  Send,
  ShieldCheck,
  Settings2,
  Smartphone,
  Trash2,
  UsersRound,
  UserRound,
  PhoneIncoming,
  X,
} from "lucide-react";
import {
  apiFetch,
  saveDownloadResponse,
  localTimestamp,
  formatDateTime,
  normalizeTotpWorkflowPreferences,
  readTotpWorkflowPreferences,
  saveTotpWorkflowPreferences,
} from "../lib/console.js";
import { describeJob, uploadBlockedJob } from "../lib/jobPresentation.js";
import { AuthorizationMethodPicker, useAuthorizationMethodPreference } from "./authorization-method.jsx";
import { useTotpSetupSecret } from "../hooks/totpSetup.js";

export { AccountInfoDialog } from "./account-info-dialog.jsx";
export { ReloginActionDialog } from "./relogin-action-dialog.jsx";

export function EmptyState({ filtered = false, columnCount = 8 }) {
  return (
    <tr>
      <td colSpan={columnCount}>
        <div className="empty-state">
          <div><Mail size={24} /></div>
          <h3>{filtered ? "没有匹配账号" : "暂无授权任务"}</h3>
          <p>{filtered ? "当前筛选邮箱不在任务列表中。" : "可单个添加并开始登录，或批量导入后自行选择功能。"}</p>
        </div>
      </td>
    </tr>
  );
}

export function TotpActionDialog({ action, logoutAvailable, reloginAvailable, uploadAvailable, uploadConfigured, uploadTarget = "sub2api", onUploadTargetChange, uploadTargets, uploadTargetSaved = true, onConfigureUpload, onSubmit, onClose }) {
  const [authorizationMethod, setAuthorizationMethod, authorizationMethodSaved] = useAuthorizationMethodPreference(action);
  const [preferences, setPreferences] = useState(readTotpWorkflowPreferences);
  const [preferencesSaved, setPreferencesSaved] = useState(true);
  const saved = preferencesSaved && authorizationMethodSaved && uploadTargetSaved;
  const targetName = uploadTargetLabel(uploadTarget);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const submitting = useRef(false);
  const localCompletion = Boolean(action.completeTotp);
  const verb = action.resetTotp ? "重置" : "设置";
  const title = localCompletion ? "完成本地保存" : `${verb} 2FA`;
  const logout = Boolean(!localCompletion && action.resetTotp && logoutAvailable && preferences.logoutAllDevicesAfterTotp);
  const relogin = Boolean(!localCompletion && reloginAvailable && preferences.reloginAfterTotp);
  const upload = Boolean(uploadAvailable && uploadConfigured && relogin && preferences.uploadAfterTotp);
  const incompatible = upload && uploadTargetConflict(uploadTarget, authorizationMethod);
  const steps = [
    { key: "totp", icon: ShieldCheck, title, detail: localCompletion ? "完成本地密钥保存及收尾" : "激活并保存密钥" },
    ...(logout ? [{ key: "logout", icon: UsersRound, title: "退出设备", detail: "发送退出请求" }] : []),
    ...(relogin ? [{ key: "relogin", icon: LogIn, title: "重登授权", detail: "生成新授权" }] : []),
    ...(upload ? [{ key: "upload", icon: Send, title: "推送授权", detail: `上传 ${targetName}` }] : []),
  ];

  function updatePreference(key, checked) {
    const next = normalizeTotpWorkflowPreferences({
      ...preferences,
      [key]: checked,
      ...(key === "reloginAfterTotp" && !checked ? { uploadAfterTotp: false } : {}),
    });
    setPreferences(next);
    setPreferencesSaved(saveTotpWorkflowPreferences(next));
  }

  function close() {
    if (submitting.current) return;
    onClose();
  }

  async function submit(event) {
    event.preventDefault();
    if (submitting.current || incompatible) return;
    submitting.current = true;
    setBusy(true);
    setError("");
    try {
      await onSubmit(localCompletion ? {} : { logoutAllDevicesAfterTotp: logout, reloginAfterTotp: relogin, uploadAfterTotp: upload, ...(upload && uploadTarget === "cpa" ? { uploadTarget } : {}), ...(relogin ? { authorizationMethod } : {}) });
    } catch (requestError) {
      setError(requestError.message);
    } finally {
      submitting.current = false;
      setBusy(false);
    }
  }

  return (
    <DialogFrame onClose={close} busy={busy}>
      <section className="batch-dialog totp-action-dialog" role="dialog" aria-modal="true" aria-labelledby="totp-action-title">
        <div className="dialog-header">
          <div className="totp-dialog-heading">
            <span className="totp-dialog-icon" aria-hidden="true"><ShieldCheck size={21} /></span>
            <div><h2 id="totp-action-title">{title}</h2><p>{action.batch ? `已选 ${action.eligibleCount} 个账号` : action.email}</p></div>
          </div>
          <button type="button" className="icon-button" onClick={close} disabled={busy} aria-label="关闭窗口"><X size={18} /></button>
        </div>
        <form onSubmit={submit}>
          <div className={`totp-dialog-notice ${action.resetTotp ? "is-reset" : ""}`}>
            {action.resetTotp ? <CircleAlert size={16} aria-hidden="true" /> : <ShieldCheck size={16} aria-hidden="true" />}
            <p>{localCompletion ? "新 2FA 已确认成功，将使用本次激活证据完成密钥的安全保存及本地记录收尾。不连接远端、不重新登录、不请求验证码，也不退出设备或上传；材料不完整或不匹配时停止。" : action.resetTotp ? "重置成功后可在「账号信息」中查看新密钥。新密钥可用，不能证明所有旧密钥已失效。" : "启用并保存 2FA 密钥后，按下方选项继续执行。"}</p>
          </div>
          {action.skipped > 0 && <p className="dialog-hint">另有 {action.skipped} 个账号不符合条件，将自动跳过。</p>}
          {!localCompletion && <div className="totp-workflow-options">
            {action.resetTotp && (
              <TotpWorkflowOption icon={UsersRound} title="退出所有设备" description={logoutAvailable ? "新密钥保存后发送退出请求。服务端接受请求，不等于所有设备已立即退出。" : "当前服务暂不支持退出所有设备。"} checked={logout} disabled={busy || !logoutAvailable} onChange={(checked) => updatePreference("logoutAllDevicesAfterTotp", checked)} />
            )}
            <TotpWorkflowOption icon={LogIn} title="重登授权" description={reloginAvailable ? (logout ? "退出请求被接受后，重新登录并生成授权。" : "2FA 完成后，重新登录并生成授权。") : "当前服务暂不支持重登授权。"} checked={relogin} disabled={busy || !reloginAvailable} onChange={(checked) => updatePreference("reloginAfterTotp", checked)} />
            {relogin && <AuthorizationMethodPicker value={authorizationMethod} onChange={setAuthorizationMethod} disabled={busy} />}
            <div>
              {onUploadTargetChange && <UploadTargetPicker value={uploadTarget} onChange={onUploadTargetChange} disabled={busy} sub2apiAvailable={uploadTargets?.sub2api?.available} cpaAvailable={uploadTargets?.cpa?.available} />}
              <TotpWorkflowOption icon={Send} title={`授权后自动上传 ${targetName}`} description={!uploadAvailable ? `当前服务暂不支持 ${targetName} 上传。` : !uploadConfigured ? uploadTarget === "cpa" ? "请先读取并保存 CPA 管理地址和管理密钥。" : "请先配置 Sub2API 地址和管理员 API Key。" : !relogin ? "开启重登授权后，可自动同步新授权。" : uploadTarget === "cpa" ? "将新授权推送到 CPA，更新同名授权文件。" : "已有账号更新授权并保留原有号池配置。"} checked={upload} disabled={busy || !uploadAvailable || !uploadConfigured || !relogin} onChange={(checked) => updatePreference("uploadAfterTotp", checked)} />
              {uploadAvailable && !uploadConfigured && <button type="button" className="totp-configure-link" disabled={busy} onClick={() => onConfigureUpload(uploadTarget)}><Settings2 size={13} />配置 {targetName}<ChevronRight size={13} /></button>}
            </div>
          </div>}
          {incompatible && <p className="dialog-error" role="alert">{CPA_AUTHORIZATION_HELP}</p>}
          <div className="totp-flow-preview" aria-label="执行流程">
            <div className="totp-flow-preview-heading"><span>执行顺序</span><small>{steps.length} 个步骤</small></div>
            <ol className={`totp-flow-preview-steps ${steps.length === 1 ? "is-single" : ""}`} style={{ "--totp-step-count": steps.length }}>
              {steps.map(({ key, icon: Icon, title, detail }, index) => <li key={key}><span className="totp-flow-preview-icon" aria-hidden="true"><Icon size={16} /></span><span className="totp-flow-preview-copy"><strong>{title}</strong><small>{detail}</small></span><span className="sr-only">第 {index + 1} 步</span></li>)}
            </ol>
          </div>
          {!localCompletion && <p className={`totp-preferences-hint ${saved ? "" : "unsaved"}`} role="status">{saved ? <Check size={13} aria-hidden="true" /> : <CircleAlert size={13} aria-hidden="true" />}{saved ? "选项自动保存在当前浏览器，仅确认后执行。" : "浏览器阻止保存，部分选项仅在本次窗口有效。"}</p>}
          {error && <p className="dialog-error" role="alert"><CircleAlert size={16} />{error}</p>}
          <div className="dialog-actions">
            <button type="button" className="secondary-button" onClick={close} disabled={busy}>取消</button>
            <button type="submit" className="primary-button" disabled={busy || incompatible}>
              {busy ? <LoaderCircle className="spin" size={16} /> : <ShieldCheck size={16} />}{busy ? "正在提交…" : localCompletion ? "完成本地保存" : "开始执行"}
            </button>
          </div>
        </form>
      </section>
    </DialogFrame>
  );
}

function TotpWorkflowOption({ icon: Icon, title, description, checked, disabled, onChange }) {
  return <label className={`totp-workflow-option ${checked ? "is-selected" : ""} ${disabled ? "is-disabled" : ""}`}>
    <span className="totp-option-icon" aria-hidden="true"><Icon size={18} /></span>
    <span className="totp-option-copy"><strong>{title}</strong><span>{description}</span></span>
    <span className="totp-option-switch"><input type="checkbox" role="switch" aria-label={title} checked={checked} disabled={disabled} onChange={(event) => onChange(event.target.checked)} /><span aria-hidden="true" /></span>
  </label>;
}

const presentationIcons = { Ban, CheckCircle2, CircleAlert, KeyRound, ListPlus, LoaderCircle, LogIn, Mail, PhoneIncoming, RefreshCw, RotateCcw, ShieldCheck, Smartphone };

function JobNotice({ notice }) {
  const Icon = presentationIcons[notice.icon] || CircleAlert;
  const longText = notice.text.length > 160;
  const details = notice.details || (longText ? notice.text : "");
  const summary = !notice.details && longText ? `${notice.title}：${notice.text.slice(0, 100)}…` : notice.text;
  if (details) return <details className={`job-notice job-notice-details tone-${notice.tone}`}>
    <summary aria-label={`${notice.title}：${summary}，展开查看完整内容`}><Icon size={13} aria-hidden="true" /><span className="job-notice-summary-copy">{summary.split(" · ").map((part, index) => <span key={index}>{index > 0 ? "· " : ""}{part}</span>)}</span><ChevronRight className="job-notice-chevron" size={12} aria-hidden="true" /></summary>
    <div className="job-notice-detail-copy">{details}</div>
  </details>;
  return <div className={`job-notice tone-${notice.tone}`}><Icon size={13} aria-hidden="true" /><span><strong>{notice.title}</strong>{notice.text}</span></div>;
}

export function JobRow({ job, token, expanded, onToggleLogs, onError, selected, onToggleSelected, selectionSupported, smsProviderAvailable, smsProvider, onUpload, onRelogin, onAuthorize, onDelete, onViewAccount, deleteSupported = true, accountInfoAvailable = true, sub2apiUploadAvailable, uploadTarget = "sub2api", totpSetupAvailable, totpResetAvailable, onSetupTotp, passwordAddAvailable, forceReloginAvailable, accountActionsBusy, accountProxyUrl, insightSnapshot, onViewInsights, onRefreshInsights, insightEnabled = false, insightBusy = false, subscriptionAvailable = false, subscriptionRefreshState = {}, onRefreshSubscription, qualityAvailable = false, qualitySnapshot, qualityStateUncertain = false, onViewQuality, onCheckQuality, qualityCanCheck = false }) {
  const confirm = useConfirmation();
  const [input, setInput] = useState(null);
  const [submission, setSubmission] = useState(null);
  const authority = useRef(null);
  if (!authority.current || authority.current.token !== token || authority.current.jobId !== job.id) {
    authority.current = { token, jobId: job.id, pending: false, active: true };
  }
  const scope = authority.current;
  const value = input?.scope === scope && input.status === job.status && input.operationAt === job.lastOperationAt ? input.value : "";
  const setValue = value => setInput({ scope, status: job.status, operationAt: job.lastOperationAt, value });
  const submitting = submission === scope;
  const isCurrent = () => authority.current === scope && scope.active;
  useEffect(() => {
    scope.active = true;
    return () => { scope.active = false; };
  }, [scope]);
  function beginSubmission() {
    if (!isCurrent() || scope.pending || accountActionsBusy) return false;
    scope.pending = true;
    setSubmission(scope);
    return true;
  }
  function finishSubmission() {
    scope.pending = false;
    if (isCurrent()) setSubmission(null);
  }
  const presentation = describeJob(job);
  const { secret: totpSetupSecret, error: totpSetupError, retry: retryTotpSetupRead } = useTotpSetupSecret({
    token, jobId: job.id, status: job.status, lastOperationAt: job.lastOperationAt, terminal: presentation.terminal,
  });

  async function sendInput(action, submittedValue = value) {
    if (!beginSubmission()) return;
    try {
      await apiFetch(token, `/api/jobs/${job.id}/input`, {
        method: "POST",
        body: JSON.stringify({ action, value: submittedValue }),
      });
      if (isCurrent()) { setValue(""); onError(""); }
    } catch (requestError) {
      if (isCurrent()) onError(requestError.message);
    } finally {
      finishSubmission();
    }
  }

  async function cancel() {
    if (!beginSubmission()) return;
    try {
      await apiFetch(token, `/api/jobs/${job.id}/cancel`, { method: "POST" });
    } catch (requestError) {
      if (isCurrent()) onError(requestError.message);
    } finally {
      finishSubmission();
    }
  }

  async function retry() {
    if (onAuthorize) { if (isCurrent() && !scope.pending && !accountActionsBusy) onAuthorize("retry"); return; }
    if (!beginSubmission()) return;
    try {
      await apiFetch(token, `/api/jobs/${job.id}/retry`, {
        method: "POST",
        body: JSON.stringify({ proxyUrl: accountProxyUrl.trim() }),
      });
      if (isCurrent()) onError("");
    } catch (requestError) {
      if (isCurrent()) onError(requestError.message);
    } finally {
      finishSubmission();
    }
  }

  async function regenerate() {
    if (onAuthorize) { if (isCurrent() && !scope.pending && !accountActionsBusy) onAuthorize("regenerate"); return; }
    if (!beginSubmission()) return;
    try {
      await apiFetch(token, `/api/jobs/${job.id}/regenerate`, {
        method: "POST",
        body: JSON.stringify({ proxyUrl: accountProxyUrl.trim() }),
      });
      if (isCurrent()) onError("");
    } catch (requestError) {
      if (isCurrent()) onError(requestError.message);
    } finally {
      finishSubmission();
    }
  }

  async function addPassword() {
    if (!beginSubmission()) return;
    try {
      if (!await confirm("确定为该账号生成并添加随机强密码吗？成功后会自动更新账号原始信息。") || !isCurrent()) return;
      await apiFetch(token, `/api/jobs/${job.id}/add-password`, {
        method: "POST",
        body: JSON.stringify({ proxyUrl: accountProxyUrl.trim() }),
      });
      if (isCurrent()) onError("");
    } catch (requestError) {
      if (isCurrent()) onError(requestError.message);
    } finally {
      finishSubmission();
    }
  }

  async function copyTotpSecret() {
    try {
      await navigator.clipboard.writeText(totpSetupSecret || "");
      onError("");
    } catch {
      onError("无法自动复制，请手动选择 2FA 密钥");
    }
  }

  async function requestSmsNumber() {
    if (!smsProvider.ready || !beginSubmission()) return;
    try {
      await apiFetch(token, `/api/jobs/${job.id}/sms-number`, {
        method: "POST",
        body: JSON.stringify({ providerId: smsProvider.id, config: smsProvider.config }),
      });
      if (isCurrent()) onError("");
    } catch (requestError) {
      if (isCurrent()) onError(requestError.message);
    } finally {
      finishSubmission();
    }
  }

  async function download() {
    try {
      const response = await fetch(`/api/jobs/${job.id}/download`, {
        headers: { "x-console-token": token },
      });
      if (!response.ok) throw new Error((await response.json()).error || "下载失败");
      await saveDownloadResponse(response, `${job.email}-sub2api-import-oauth-${localTimestamp()}.json`);
    } catch (requestError) {
      onError(requestError.message);
    }
  }

  const inputConfig = !presentation.terminal && !job.totpCompletionPending && (!(job.totpRecoveryPending || job.totpCredentialInvalidated) || ["setup_2fa", "reset_2fa"].includes(presentation.operation.type)) && getInputConfig(job.status, job.currentPhone);
  const terminal = presentation.terminal;
  const hasEnabledTotp = Boolean(job.hasTotpKey || job.totpKnownEnabled);
  const blockedTotp = Boolean(job.totpRecoveryPending || job.totpCredentialInvalidated || job.totpCompletionPending || job.totpExportBlocked);
  const localCompletion = Boolean(job.canCompleteTotp || job.totpCompletionPending);
  const totpActionLabel = localCompletion ? "完成本地保存" : blockedTotp ? "需人工处理" : "重置 2FA";
  const totpActionAllowed = localCompletion ? job.canCompleteTotp : !blockedTotp && job.canResetTotp;
  const totpActionHint = localCompletion ? job.canCompleteTotp ? job.totpCompletionHint : job.totpCompletionUnavailableReason : blockedTotp ? "2FA 修改结果未确认，需人工处理；不能再次修改或恢复远端会话" : job.canResetTotp ? "更换 2FA 密钥，可选择退出所有设备、重登授权和推送新授权" : job.totpResetUnavailableReason || "当前账号暂不能重置 2FA";
  const showTotpSetupAction = totpSetupAvailable && !hasEnabledTotp && !blockedTotp;
  const actionBusy = submitting || accountActionsBusy;
  const subscriptionBlocked = job.canRefreshSubscription !== true || subscriptionRefreshState.busy || actionBusy;
  const canDelete = terminal && !actionBusy;
  const hasMoreActions = onViewInsights || qualityAvailable || accountInfoAvailable
    || job.canDownload
    || job.canRegenerate
    || (passwordAddAvailable && job.canAddPassword)
    || job.canRetry
    || showTotpSetupAction
    || !terminal;

  return (
    <tr className={`job-row status-${job.status}`}>
      <td className="select-cell">
        <input
          type="checkbox"
          checked={selected}
          onChange={onToggleSelected}
          disabled={!selectionSupported}
          aria-label={`选择 ${job.email}`}
        />
      </td>
      <td>
        <div className="account-cell">
          <strong title={job.email}>{job.email}</strong>
          <div className="account-meta">
            <LoginMethodBadge job={job} />
            <PlanBadge value={subscriptionDisplayPlan(job, subscriptionRefreshState)} short
              onRefresh={subscriptionAvailable ? onRefreshSubscription : undefined}
              refreshDisabled={subscriptionBlocked} refreshing={subscriptionRefreshState.busy} accountLabel={job.email}
              refreshHint={job.canRefreshSubscription !== true ? job.subscriptionRefreshUnavailableReason || "当前账号暂不可刷新套餐" : actionBusy ? "账号操作进行中，暂不可刷新套餐" : ""} />
          </div>
        </div>
      </td>
      <td className="usage-cell">{onViewInsights && <AccountInsightSummary snapshot={insightSnapshot} qualitySnapshot={qualityAvailable ? qualitySnapshot : undefined} requestObservations={job.accountRequestObservations} onOpen={onViewInsights} accountLabel={job.email} />}</td>
      {qualityAvailable && <td className="quality-cell"><AccountQualityStatus snapshot={qualitySnapshot} authorizationMethod={job.authorizationMethod} onOpen={onViewQuality} accountLabel={job.email} stateUncertain={qualityStateUncertain} /></td>}
      <td className="expiration-cell"><AccountExpiration job={getInsightExpirationJob(job, insightSnapshot)} onEdit={accountInfoAvailable ? onViewAccount : undefined} /></td>
      <td className="step-cell">
        <div className="prompt-line" title={presentation.summary}>{operationExecutionHint(presentation)}</div>
        {presentation.notices.map((notice) => <JobNotice key={notice.key} notice={notice} />)}
        {presentation.phone.visible && <div className="phone-target"><Smartphone size={13} />当前手机号：<strong>{presentation.phone.number}</strong></div>}
        {presentation.historyNotices.length > 0 && <details className="job-history-notices"><summary>历史提醒 <span>{presentation.historyNotices.length}</span></summary>{presentation.historyNotices.map((notice) => <JobNotice key={notice.key} notice={notice} />)}</details>}
        {!terminal && job.status === "totp_setup_otp" && totpSetupError && <div className="totp-read-error" role="status">
          <span>2FA 密钥暂未读取：{totpSetupError}</span>
          <button type="button" className="text-action" onClick={retryTotpSetupRead}><RefreshCw size={12} aria-hidden="true" />重新读取密钥</button>
        </div>}
        {!terminal && job.status === "totp_setup_otp" && totpSetupSecret && (
          <div className="totp-setup-secret">
            <span>2FA 密钥</span>
            <code>{totpSetupSecret}</code>
            <button type="button" className="icon-button" onClick={copyTotpSecret} title="复制 2FA 密钥">
              <Copy size={15} />
            </button>
          </div>
        )}
        {inputConfig && (
          <div className="inline-entry">
            <div className="compact-input">
              {inputConfig.icon}
              <input
                value={value}
                onChange={(event) => setValue(event.target.value)}
                onKeyDown={(event) => {
                  if (event.key === "Enter" && value && !submitting) sendInput(inputConfig.action);
                }}
                inputMode={inputConfig.inputMode}
                type={inputConfig.type || "text"}
                placeholder={inputConfig.placeholder}
                aria-label={inputConfig.placeholder}
                autoComplete={inputConfig.autoComplete || "one-time-code"}
              />
            </div>
            <button
              type="button"
              className="icon-button submit"
              onClick={() => sendInput(inputConfig.action)}
              disabled={!value || submitting}
              title={inputConfig.submitLabel}
            >
              {submitting ? <LoaderCircle className="spin" size={17} /> : <Send size={17} />}
            </button>
            {job.status === "email_otp" && (
              <button type="button" className="text-action" onClick={() => sendInput("resend_email", "")} disabled={submitting}>
                <RefreshCw size={14} />重发
              </button>
            )}
            {job.status === "phone_otp" && (
              <>
                <button type="button" className="text-action" onClick={() => sendInput("resend_phone", "")} disabled={submitting}>
                  <RefreshCw size={14} />重发
                </button>
                <button type="button" className="text-action" onClick={() => sendInput("change_phone", "")} disabled={submitting}>
                  <RotateCcw size={14} />换号
                </button>
              </>
            )}
            {job.status === "phone" && smsProviderAvailable && (
              <>
                <span className="input-separator" aria-hidden="true" />
                <button
                  type="button"
                  className="platform-number-button"
                  onClick={requestSmsNumber}
                  disabled={!smsProvider.ready || submitting || job.smsStatus === "requesting"}
                  title={smsProvider.ready ? `使用 ${smsProvider.name} 取号` : "请先完成接码平台配置"}
                >
                  {submitting || job.smsStatus === "requesting" ? <LoaderCircle className="spin" size={15} /> : <PhoneIncoming size={15} />}
                  {smsProvider.name || "平台"}取号
                </button>
              </>
            )}
          </div>
        )}
      </td>
      <td className="activity-time-cell">
        <div className="time-entry"><span>添加</span><time dateTime={job.createdAt}>{formatDateTime(job.createdAt)}</time></div>
        <div className="time-entry"><span>操作</span><time dateTime={job.lastOperationAt || job.createdAt}>{formatDateTime(job.lastOperationAt || job.createdAt)}</time></div>
      </td>
      <td className="operation-type-cell"><AccountOperationSummary job={job} presentation={presentation} expanded={expanded} onToggleLogs={onToggleLogs} /></td>
      <td className="actions-cell">
        <div className="row-action-stack">
        <div className="row-primary-actions" aria-label={`快捷操作 ${job.email}`}>
          {forceReloginAvailable && (
            <button type="button" className="row-action-button relogin" onClick={onRelogin} disabled={!job.canForceRelogin || actionBusy} title={job.canForceRelogin ? "完整重新登录并生成授权，可选择成功后自动推送" : "当前账号暂不能重新登录并授权"} aria-label={`重新登录并授权 ${job.email}`}>
              <LogIn size={14} aria-hidden="true" /><span>重登授权</span>
            </button>
          )}
          {totpResetAvailable && (
            <button type="button" className="row-action-button row-totp-button" onClick={() => onSetupTotp(true, Boolean(job.canCompleteTotp))} disabled={!totpActionAllowed || actionBusy} title={totpActionHint} aria-label={`${totpActionLabel} ${job.email}`}>
              <RotateCcw size={14} aria-hidden="true" /><span>{totpActionLabel}</span>
            </button>
          )}
          {deleteSupported && (
            <button type="button" className="row-action-button row-delete-button" onClick={onDelete} disabled={!canDelete} title={terminal ? "移入回收站，保留同邮箱全部历史记录" : "请先取消当前任务，再移入回收站"} aria-label={`移入回收站 ${job.email}`}>
              <Trash2 size={14} aria-hidden="true" /><span>删除</span>
            </button>
          )}
        </div>
        <div className="row-utility-actions">
          {sub2apiUploadAvailable && (
            <button type="button" className="row-action-button row-upload-button" onClick={onUpload} disabled={uploadBlockedJob(job) || !job.canDownload || actionBusy} title={uploadBlockedJob(job) ? "请等待当前流程结束或本地保存完成，再上传最新授权" : job.canDownload ? `上传最新授权到 ${uploadTargetLabel(uploadTarget)}` : `授权文件可用后，可上传 ${uploadTargetLabel(uploadTarget)}`} aria-label={`上传 ${uploadTargetLabel(uploadTarget)} ${job.email}`}>
              <Send size={14} aria-hidden="true" /><span>上传</span>
            </button>
          )}
          <button type="button" className={`row-action-button row-logs-button ${expanded ? "is-active" : ""}`} onClick={onToggleLogs} title={expanded ? "收起日志" : "查看日志"} aria-label={`${expanded ? "收起日志" : "查看日志"} ${job.email}`} aria-expanded={expanded}>
            <FileText size={14} aria-hidden="true" /><span>日志</span>
          </button>
          {hasMoreActions && <ActionMenu label="更多" ariaLabel={`账号操作 ${job.email}`}>
          {onViewInsights && <button type="button" className="secondary-button" onClick={onViewInsights}><FileText size={16} />用量与状态</button>}
          {qualityAvailable && <button type="button" className="secondary-button" onClick={onViewQuality}><FileText size={16} />推理探针详情</button>}
          {qualityAvailable && <button type="button" className="secondary-button" onClick={onCheckQuality} disabled={!qualityCanCheck}><RefreshCw size={16} />发起推理探针</button>}
          {onRefreshInsights && <button type="button" className="secondary-button" onClick={onRefreshInsights} disabled={!insightEnabled || insightBusy || insightSnapshot?.canQuery === false} title={!insightEnabled ? "请先启用账号查询" : insightSnapshot?.unavailableReason || "只读查询，不刷新授权或重新登录"}><RefreshCw size={16} />更新用量与状态</button>}
          {accountInfoAvailable && (
            <button type="button" className="secondary-button" onClick={onViewAccount} title="查看当前保存的邮箱、密码与 2FA 等账号资料">
              <UserRound size={16} />查看账号信息
            </button>
          )}
          {job.canDownload && (
            <button type="button" className="secondary-button" onClick={download} title="下载授权文件" aria-label={`下载授权文件 ${job.email}`}>
              <Download size={16} aria-hidden="true" />下载授权文件
            </button>
          )}
          {job.status === "imported" && job.canRetry && (
            <button type="button" className="secondary-button" onClick={retry} disabled={submitting || accountActionsBusy} title="开始登录并生成授权文件">
              {submitting ? <LoaderCircle className="spin" size={16} /> : <LogIn size={16} />}开始授权
            </button>
          )}
          {job.canRegenerate && (
            <button type="button" className="regenerate-button" onClick={regenerate} disabled={actionBusy} title="重新授权：优先使用刷新令牌，失效后自动重新登录">
              {submitting ? <LoaderCircle className="spin" size={16} /> : <RefreshCw size={16} />}
              重新授权
            </button>
          )}
          {showTotpSetupAction && (
            <button type="button" className="secondary-button" onClick={() => onSetupTotp(false)} disabled={!job.canSetupTotp || submitting || accountActionsBusy} title={job.canSetupTotp ? "为账号设置 2FA" : job.totpSetupUnavailableReason || "当前账号暂不能设置 2FA"}>
              <ShieldCheck size={16} />设置 2FA
            </button>
          )}
          {passwordAddAvailable && job.canAddPassword && (
            <button type="button" className="icon-button" onClick={addPassword} disabled={actionBusy} title="添加密码">
              {submitting ? <LoaderCircle className="spin" size={16} /> : <KeyRound size={16} />}
              添加密码
            </button>
          )}
          {job.canRetry && job.status !== "imported" && (!job.canRegenerate || job.canResume || job.securityCheckRequired) && (
            <button type="button" className="retry-button" onClick={retry} disabled={actionBusy}>
              {submitting ? <LoaderCircle className="spin" size={16} /> : <RefreshCw size={16} />}
              {job.securityCheckRequired ? "手动重试" : job.canResume ? "完成本地保存" : "重新授权"}
            </button>
          )}
          {!terminal && (
            <button type="button" className="icon-button danger" onClick={cancel} disabled={actionBusy} title="取消任务">
              <Ban size={17} />取消任务
            </button>
          )}
          </ActionMenu>}
        </div>
        </div>
      </td>
    </tr>
  );
}

export function LoginMethodBadge({ job }) {
  const totpRecoveryPending = Boolean(job.totpRecoveryPending || job.totpCredentialInvalidated);
  const mailFailed = !describeJob(job).terminal && job.status === "email_otp" && ["error", "timeout"].includes(job.mailStatus);
  const totpMethod = job.totpCompletionPending ? "2FA 已确认 · 待保存" : totpRecoveryPending ? "2FA 需人工处理" : job.hasTotpKey || job.totpKnownEnabled ? "2FA" : "";
  if (job.loginMode === "password") {
    const methods = ["密码", job.autoEmailOtp ? "自动收码" : "", totpMethod].filter(Boolean);
    return (
      <span className={`mail-mode password-mode ${totpRecoveryPending ? "error" : ""}`}>
        {totpMethod ? <ShieldCheck size={12} /> : <KeyRound size={12} />}
        {methods.join(" + ")}
      </span>
    );
  }
  if (job.autoEmailOtp) {
    return (
      <span className={`mail-mode ${totpRecoveryPending || mailFailed ? "error" : ""}`}>
        {totpMethod ? <ShieldCheck size={12} /> : <MailCheck size={12} />}
        {["自动收码", totpMethod].filter(Boolean).join(" + ")}
      </span>
    );
  }
  if (job.loginMode === "manual") {
    return (
      <span className={`mail-mode unknown-mode ${totpRecoveryPending ? "error" : ""}`}>
        <CircleAlert size={12} />{["登录资料未记录", totpMethod].filter(Boolean).join(" + ")}
      </span>
    );
  }
  if (!totpMethod) return null;
  return (
    <span className={`mail-mode ${totpRecoveryPending ? "error" : ""}`}>
      <ShieldCheck size={12} />邮箱码 + {totpMethod}
    </span>
  );
}

function getInputConfig(status, currentPhone) {
  if (status === "password") {
    return { action: "password", placeholder: "输入账号密码", submitLabel: "提交密码", inputMode: "text", type: "password", autoComplete: "current-password", icon: <KeyRound size={15} /> };
  }
  if (status === "mfa_otp") {
    return { action: "mfa_otp", placeholder: "6 位 2FA 验证码", submitLabel: "提交 2FA 验证码", inputMode: "numeric", icon: <ShieldCheck size={15} /> };
  }
  if (status === "totp_setup_otp") {
    return { action: "totp_setup_otp", placeholder: "新 2FA 的 6 位验证码", submitLabel: "激活新的 2FA", inputMode: "numeric", icon: <ShieldCheck size={15} /> };
  }
  if (status === "email_otp") {
    return { action: "email_otp", placeholder: "6 位邮箱验证码", submitLabel: "提交邮箱验证码", inputMode: "numeric", icon: <Mail size={15} /> };
  }
  if (status === "phone") {
    return { action: "phone", placeholder: "+60123456789", submitLabel: "发送手机验证码", inputMode: "tel", icon: <Smartphone size={15} /> };
  }
  if (status === "phone_otp") {
    return { action: "phone_otp", placeholder: currentPhone ? `${currentPhone} 的验证码` : "手机验证码", submitLabel: "提交手机验证码", inputMode: "numeric", icon: <Smartphone size={15} /> };
  }
  return null;
}
