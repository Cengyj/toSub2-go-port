import React, { useEffect, useRef, useState } from "react";
import { PlanBadge } from "./PlanBadge.jsx";
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
import { describeJob, describeStatus, uploadBlockedJob } from "../lib/jobPresentation.js";
import { AuthorizationMethodBadge, AuthorizationMethodPicker, useAuthorizationMethodPreference } from "./authorization-method.jsx";

export { AccountInfoDialog } from "./account-info-dialog.jsx";
export { ReloginActionDialog } from "./relogin-action-dialog.jsx";

export function EmptyState({ filtered = false }) {
  return (
    <tr>
      <td colSpan="7">
        <div className="empty-state">
          <div><Mail size={24} /></div>
          <h3>{filtered ? "没有匹配账号" : "暂无授权任务"}</h3>
          <p>{filtered ? "当前筛选邮箱不在任务列表中。" : "可单个添加并开始登录，或批量导入后自行选择功能。"}</p>
        </div>
      </td>
    </tr>
  );
}

export function TotpActionDialog({ action, logoutAvailable, reloginAvailable, uploadAvailable, uploadConfigured, onConfigureUpload, onSubmit, onClose }) {
  const [authorizationMethod, setAuthorizationMethod, authorizationMethodSaved] = useAuthorizationMethodPreference(action);
  const [preferences, setPreferences] = useState(readTotpWorkflowPreferences);
  const [preferencesSaved, setPreferencesSaved] = useState(true);
  const saved = preferencesSaved && authorizationMethodSaved;
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const submitting = useRef(false);
  const verb = action.resetTotp ? "重置" : "设置";
  const logout = Boolean(action.resetTotp && logoutAvailable && preferences.logoutAllDevicesAfterTotp);
  const relogin = Boolean(reloginAvailable && preferences.reloginAfterTotp);
  const upload = Boolean(uploadAvailable && uploadConfigured && relogin && preferences.uploadAfterTotp);
  const steps = [
    { key: "totp", icon: ShieldCheck, title: `${verb} 2FA`, detail: "激活并保存密钥" },
    ...(logout ? [{ key: "logout", icon: UsersRound, title: "退出设备", detail: "退出所有登录" }] : []),
    ...(relogin ? [{ key: "relogin", icon: LogIn, title: "重登授权", detail: "生成新授权" }] : []),
    ...(upload ? [{ key: "upload", icon: Send, title: "同步号池", detail: "上传 Sub2API" }] : []),
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
    if (submitting.current) return;
    submitting.current = true;
    setBusy(true);
    setError("");
    try {
      await onSubmit({ logoutAllDevicesAfterTotp: logout, reloginAfterTotp: relogin, uploadAfterTotp: upload, ...(relogin ? { authorizationMethod } : {}) });
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
            <div><h2 id="totp-action-title">{verb} 2FA</h2><p>{action.batch ? `已选 ${action.eligibleCount} 个账号` : action.email}</p></div>
          </div>
          <button type="button" className="icon-button" onClick={close} disabled={busy} aria-label="关闭窗口"><X size={18} /></button>
        </div>
        <form onSubmit={submit}>
          <div className={`totp-dialog-notice ${action.resetTotp ? "is-reset" : ""}`}>
            {action.resetTotp ? <CircleAlert size={16} aria-hidden="true" /> : <ShieldCheck size={16} aria-hidden="true" />}
            <p>{action.resetTotp ? "将更换并保存新的 2FA 密钥，旧验证器随即失效。若中途失败，请先核实账号的 2FA 状态。" : "启用并保存 2FA 密钥后，按下方选项继续执行。"}</p>
          </div>
          {action.skipped > 0 && <p className="dialog-hint">另有 {action.skipped} 个账号不符合条件，将自动跳过。</p>}
          <div className="totp-workflow-options">
            {action.resetTotp && (
              <TotpWorkflowOption icon={UsersRound} title="退出所有设备" description={logoutAvailable ? "新密钥安全保存后，退出所有登录会话。" : "当前服务暂不支持退出所有设备。"} checked={logout} disabled={busy || !logoutAvailable} onChange={(checked) => updatePreference("logoutAllDevicesAfterTotp", checked)} />
            )}
            <TotpWorkflowOption icon={LogIn} title="重登授权" description={reloginAvailable ? (logout ? "退出所有设备成功后，重新登录并生成授权。" : "2FA 完成后，重新登录并生成授权。") : "当前服务暂不支持重登授权。"} checked={relogin} disabled={busy || !reloginAvailable} onChange={(checked) => updatePreference("reloginAfterTotp", checked)} />
            {relogin && <AuthorizationMethodPicker value={authorizationMethod} onChange={setAuthorizationMethod} disabled={busy} />}
            <div>
              <TotpWorkflowOption icon={Send} title="授权后自动上传 Sub2API" description={!uploadAvailable ? "当前服务暂不支持 Sub2API 上传。" : !uploadConfigured ? "请先配置 Sub2API 地址和管理员 API Key。" : !relogin ? "开启重登授权后，可自动同步新授权。" : "已有账号更新授权并保留原有号池配置。"} checked={upload} disabled={busy || !uploadAvailable || !uploadConfigured || !relogin} onChange={(checked) => updatePreference("uploadAfterTotp", checked)} />
              {uploadAvailable && !uploadConfigured && <button type="button" className="totp-configure-link" disabled={busy} onClick={onConfigureUpload}><Settings2 size={13} />配置 Sub2API<ChevronRight size={13} /></button>}
            </div>
          </div>
          <div className="totp-flow-preview" aria-label="执行流程">
            <div className="totp-flow-preview-heading"><span>执行顺序</span><small>{steps.length} 个步骤</small></div>
            <ol className={`totp-flow-preview-steps ${steps.length === 1 ? "is-single" : ""}`} style={{ "--totp-step-count": steps.length }}>
              {steps.map(({ key, icon: Icon, title, detail }, index) => <li key={key}><span className="totp-flow-preview-icon" aria-hidden="true"><Icon size={16} /></span><span className="totp-flow-preview-copy"><strong>{title}</strong><small>{detail}</small></span><span className="sr-only">第 {index + 1} 步</span></li>)}
            </ol>
          </div>
          <p className={`totp-preferences-hint ${saved ? "" : "unsaved"}`} role="status">{saved ? <Check size={13} aria-hidden="true" /> : <CircleAlert size={13} aria-hidden="true" />}{saved ? "选项自动保存在当前浏览器，仅确认后执行。" : "浏览器阻止保存，部分选项仅在本次窗口有效。"}</p>
          {error && <p className="dialog-error" role="alert"><CircleAlert size={16} />{error}</p>}
          <div className="dialog-actions">
            <button type="button" className="secondary-button" onClick={close} disabled={busy}>取消</button>
            <button type="submit" className="primary-button" disabled={busy}>
              {busy ? <LoaderCircle className="spin" size={16} /> : <ShieldCheck size={16} />}{busy ? "正在提交…" : "开始执行"}
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

export function JobRow({ job, token, expanded, onToggleLogs, onError, selected, onToggleSelected, selectionSupported, smsProviderAvailable, smsProvider, onUpload, onRelogin, onAuthorize, onDelete, onViewAccount, deleteSupported = true, accountInfoAvailable = true, sub2apiUploadAvailable, totpSetupAvailable, totpResetAvailable, onSetupTotp, passwordAddAvailable, forceReloginAvailable, accountActionsBusy, accountProxyUrl }) {
  const confirm = useConfirmation();
  const [value, setValue] = useState("");
  const [submitting, setSubmitting] = useState(false);
  const [totpSetupSecret, setTotpSetupSecret] = useState("");
  const presentation = describeJob(job);

  useEffect(() => setValue(""), [job.status, job.lastOperationAt, presentation.terminal]);
  useEffect(() => {
    let cancelled = false;
    let retryTimer;
    let controller;
    setTotpSetupSecret("");
    if (presentation.terminal || job.status !== "totp_setup_otp") return undefined;
    async function readSetupSecret() {
      controller = new AbortController();
      const requestTimer = setTimeout(() => controller.abort(), 10000);
      try {
        const response = await fetch(`/api/jobs/${job.id}/totp-setup`, {
          headers: { "x-console-token": token },
          signal: controller.signal,
        });
        const data = response.ok ? await response.json() : null;
        if (!cancelled && typeof data?.secret === "string" && data.secret) {
          setTotpSetupSecret(data.secret);
          return;
        }
      } catch {
        // Job polling previously recovered transient setup reads. Keep the
        // private endpoint equally resilient without publishing the seed.
      } finally {
        clearTimeout(requestTimer);
      }
      if (!cancelled) retryTimer = setTimeout(readSetupSecret, 1000);
    }
    readSetupSecret();
    return () => {
      cancelled = true;
      clearTimeout(retryTimer);
      controller?.abort();
    };
  }, [job.id, job.status, job.lastOperationAt, presentation.terminal, token]);

  async function sendInput(action, submittedValue = value) {
    setSubmitting(true);
    try {
      await apiFetch(token, `/api/jobs/${job.id}/input`, {
        method: "POST",
        body: JSON.stringify({ action, value: submittedValue }),
      });
      setValue("");
      onError("");
    } catch (requestError) {
      onError(requestError.message);
    } finally {
      setSubmitting(false);
    }
  }

  async function cancel() {
    setSubmitting(true);
    try {
      await apiFetch(token, `/api/jobs/${job.id}/cancel`, { method: "POST" });
    } catch (requestError) {
      onError(requestError.message);
    } finally {
      setSubmitting(false);
    }
  }

  async function retry() {
    if (onAuthorize) { onAuthorize("retry"); return; }
    setSubmitting(true);
    try {
      await apiFetch(token, `/api/jobs/${job.id}/retry`, {
        method: "POST",
        body: JSON.stringify({ proxyUrl: accountProxyUrl.trim() }),
      });
      onError("");
    } catch (requestError) {
      onError(requestError.message);
    } finally {
      setSubmitting(false);
    }
  }

  async function regenerate() {
    if (onAuthorize) { onAuthorize("regenerate"); return; }
    setSubmitting(true);
    try {
      await apiFetch(token, `/api/jobs/${job.id}/regenerate`, {
        method: "POST",
        body: JSON.stringify({ proxyUrl: accountProxyUrl.trim() }),
      });
      onError("");
    } catch (requestError) {
      onError(requestError.message);
    } finally {
      setSubmitting(false);
    }
  }

  async function addPassword() {
    if (!await confirm("确定为该账号生成并添加随机强密码吗？成功后会自动更新账号原始信息。")) return;
    setSubmitting(true);
    try {
      await apiFetch(token, `/api/jobs/${job.id}/add-password`, {
        method: "POST",
        body: JSON.stringify({ proxyUrl: accountProxyUrl.trim() }),
      });
      onError("");
    } catch (requestError) {
      onError(requestError.message);
    } finally {
      setSubmitting(false);
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
    if (!smsProvider.ready) return;
    setSubmitting(true);
    try {
      await apiFetch(token, `/api/jobs/${job.id}/sms-number`, {
        method: "POST",
        body: JSON.stringify({ providerId: smsProvider.id, config: smsProvider.config }),
      });
      onError("");
    } catch (requestError) {
      onError(requestError.message);
    } finally {
      setSubmitting(false);
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

  const inputConfig = !presentation.terminal && getInputConfig(job.status, job.currentPhone);
  const terminal = presentation.terminal;
  const hasEnabledTotp = Boolean(job.hasTotpKey || job.totpKnownEnabled);
  const showTotpSetupAction = totpSetupAvailable && (!hasEnabledTotp || job.totpRecoveryPending);
  const actionBusy = submitting || accountActionsBusy;
  const canDelete = terminal && !actionBusy;
  const hasMoreActions = accountInfoAvailable
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
            <PlanBadge value={job.planType} short />
          </div>
        </div>
      </td>
      <td className="step-cell">
        <div className="prompt-line">{presentation.summary}</div>
        {presentation.notices.map((notice) => <JobNotice key={notice.key} notice={notice} />)}
        {presentation.phone.visible && <div className="phone-target"><Smartphone size={13} />当前手机号：<strong>{presentation.phone.number}</strong></div>}
        {presentation.historyNotices.length > 0 && <details className="job-history-notices"><summary>历史提醒 <span>{presentation.historyNotices.length}</span></summary>{presentation.historyNotices.map((notice) => <JobNotice key={notice.key} notice={notice} />)}</details>}
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
      <td className="operation-type-cell"><span>{presentation.operation.label}</span><AuthorizationMethodBadge value={job.authorizationMethod} /></td>
      <td className="activity-time-cell">
        <div className="time-entry"><span>添加</span><time dateTime={job.createdAt}>{formatDateTime(job.createdAt)}</time></div>
        <div className="time-entry"><span>操作</span><time dateTime={job.lastOperationAt || job.createdAt}>{formatDateTime(job.lastOperationAt || job.createdAt)}</time></div>
      </td>
      <td className="status-cell"><StatusBadge presentation={presentation.status} /></td>
      <td className="actions-cell">
        <div className="row-action-stack">
        <div className="row-primary-actions" aria-label={`快捷操作 ${job.email}`}>
          {forceReloginAvailable && (
            <button type="button" className="row-action-button relogin" onClick={onRelogin} disabled={!job.canForceRelogin || actionBusy} title={job.canForceRelogin ? "完整重新登录并生成授权，可选择成功后自动上传 Sub2API" : "当前账号暂不能重新登录并授权"} aria-label={`重新登录并授权 ${job.email}`}>
              <LogIn size={14} aria-hidden="true" /><span>重登授权</span>
            </button>
          )}
          {totpResetAvailable && (
            <button type="button" className="row-action-button row-totp-button" onClick={() => onSetupTotp(true)} disabled={!job.canResetTotp || actionBusy} title={job.canResetTotp ? "更换 2FA 密钥，可选择退出所有设备、重登授权和同步 Sub2API" : job.totpResetUnavailableReason || "当前账号暂不能重置 2FA"} aria-label={`重置 2FA ${job.email}`}>
              <RotateCcw size={14} aria-hidden="true" /><span>重置 2FA</span>
            </button>
          )}
          {deleteSupported && (
            <button type="button" className="row-action-button row-delete-button" onClick={onDelete} disabled={!canDelete} title={terminal ? "删除本行账号及本地数据" : "请先取消当前任务，再删除本行数据"} aria-label={`删除当前行数据 ${job.email}`}>
              <Trash2 size={14} aria-hidden="true" /><span>删除</span>
            </button>
          )}
        </div>
        <div className="row-utility-actions">
          {sub2apiUploadAvailable && (
            <button type="button" className="row-action-button row-upload-button" onClick={onUpload} disabled={uploadBlockedJob(job) || !job.canDownload || actionBusy} title={uploadBlockedJob(job) ? "请等待当前流程结束或恢复完成，再上传最新授权" : job.canDownload ? "上传最新授权到 Sub2API 号池" : "授权文件可用后，可上传 Sub2API"} aria-label={`上传 Sub2API ${job.email}`}>
              <Send size={14} aria-hidden="true" /><span>上传</span>
            </button>
          )}
          <button type="button" className={`row-action-button row-logs-button ${expanded ? "is-active" : ""}`} onClick={onToggleLogs} title={expanded ? "收起日志" : "查看日志"} aria-label={`${expanded ? "收起日志" : "查看日志"} ${job.email}`} aria-expanded={expanded}>
            <FileText size={14} aria-hidden="true" /><span>日志</span>
          </button>
          {hasMoreActions && <ActionMenu label="更多" ariaLabel={`账号操作 ${job.email}`}>
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
              {job.securityCheckRequired ? "手动重试" : job.canResume ? "继续流程" : "重新授权"}
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

export function StatusBadge({ status, presentation = describeStatus(status) }) {
  const Icon = presentationIcons[presentation.icon] || CircleAlert;
  return <span className={`status-badge tone-${presentation.tone}`}><Icon size={14} className={presentation.spinning ? "spin" : ""} aria-hidden="true" />{presentation.label}</span>;
}

export function LoginMethodBadge({ job }) {
  const totpRecoveryPending = Boolean(job.totpRecoveryPending || job.totpCredentialInvalidated);
  const mailFailed = !describeJob(job).terminal && job.status === "email_otp" && ["error", "timeout"].includes(job.mailStatus);
  const totpMethod = totpRecoveryPending ? "2FA 待恢复" : job.hasTotpKey || job.totpKnownEnabled ? "2FA" : "";
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
