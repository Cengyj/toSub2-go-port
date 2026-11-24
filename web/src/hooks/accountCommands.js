import { useEffect, useRef, useState } from "react";
import { apiFetch, countBatchLines, localTimestamp, mergeJobs, saveDownloadResponse } from "../lib/console.js";
import { browserTimeZone } from "../lib/accountExpiration.js";
import { authorizationMethodLabel, normalizeAuthorizationMethod } from "../lib/authorizationMethod.js";
import { pendingTotpIntent, acknowledgeTotpIntent } from "../lib/totpIntent.js";
import { CPA_AUTHORIZATION_HELP, normalizeUploadTarget, uploadTargetConflict, uploadTargetLabel } from "../lib/uploadTarget.js";
import { describeBatchSubmission, describeUpload, uploadBlockedJob } from "../lib/jobPresentation.js";
import { decodeBatchSubmission, decodeCancellationReceipt, decodeImportReceipt, decodeSingleSubmission, decodeUploadReceipt } from "../lib/accountCommandOutcome.js";
import { useRemovedRowFocus } from "./removalFocus.js";

const emptyView = () => ({ email: "", batchOpen: false, batchScope: null, batchText: "", batchError: "", batchBusy: false, batchAction: "", totpAction: null, reloginAction: null, manualUploadAction: null });
const accountIdentity = job => JSON.stringify([job.id, job.email, job.createdAt, job.lastOperationAt, job.attempt, job.authorizationMethod, job.status, job.canResume]);
const changedAccount = () => new Error("账号状态或操作资格已变化，请关闭弹窗并重新选择账号后确认。");

// Own the lifetime of explicit account commands. The server remains the
// authority for capabilities and admission; this hook binds a user's confirmed
// scope to one request and consumes only validated receipts. It never retries.
export function useAccountCommands({ token, features, accounts, settings, feedback, navigation, trash, confirm, headingRef }) {
  const [, render] = useState(0);
  const owner = useRef(null), latest = useRef(null);
  if (!owner.current || owner.current.token !== token) {
    const unconfirmed = Boolean(owner.current?.pending?.dispatched || owner.current?.unconfirmed);
    if (owner.current) owner.current.active = false;
    owner.current = { token, active: true, pending: null, unconfirmed, view: emptyView() };
  }
  const session = owner.current, view = session.view;
  latest.current = { accounts, settings, feedback, navigation, trash, features, confirm };
  useEffect(() => {
    session.active = true;
    if (session.unconfirmed) {
      session.unconfirmed = false;
      latest.current.feedback.setError("");
      latest.current.feedback.showNotice("控制台会话已变化，先前操作结果尚未在当前页面确认。请先核对账号与目标服务的状态，避免重复提交。", "warning");
    }
    return () => { session.active = false; };
  }, [session]);
  const removalFocus = useRemovedRowFocus(accounts.jobs, headingRef, Boolean(view.batchAction) || Boolean(trash.actionBusy) || trash.verificationRequired);
  const current = () => owner.current === session && session.active;
  function publish(patch) {
    if (!current()) return;
    session.view = { ...session.view, ...patch };
    render(value => value + 1);
  }
  function canOpen() {
    const state = session.view;
    return current() && Boolean(token) && !session.pending && !state.totpAction && !state.reloginAction && !state.manualUploadAction && !state.batchOpen;
  }
  function begin(kind) {
    if (!current() || !token || session.pending) return null;
    const attempt = { kind, selection: latest.current.accounts.selectedJobIds, rows: latest.current.accounts.jobs };
    session.pending = attempt;
    publish({ batchAction: kind, batchBusy: kind === "import" });
    return attempt;
  }
  const owns = attempt => Boolean(attempt) && current() && session.pending === attempt;
  function finish(attempt) {
    if (!owns(attempt)) return;
    session.pending = null;
    publish({ batchAction: "", batchBusy: false });
  }
  function sending() { session.pending.dispatched = true; latest.current.accounts.refresh.invalidate?.(); }
  function adoptRows(attempt, received) {
    const baseline = new Map(attempt.rows.map(job => [job.id, job]));
    latest.current.accounts.setJobs(rows => {
      const currentRows = new Map(rows.map(job => [job.id, job]));
      // A poll or another completed operation can already have newer state.
      // A delayed creation/import receipt must not replace or resurrect it.
      const accepted = received.filter(job => currentRows.get(job.id) === baseline.get(job.id));
      return mergeJobs(accepted, rows).slice(0, 20);
    });
  }
  function removeConfirmed(attempt, ids) {
    const confirmed = new Set(ids);
    if (!confirmed.size || !owns(attempt)) return;
    latest.current.accounts.setSelectedJobIds(selection => {
      // A changed selection expresses a new user intention, including removing
      // and reselecting the same ID. An older receipt must not clear it.
      if (selection !== attempt.selection) return selection;
      return new Set([...selection].filter(id => !confirmed.has(id)));
    });
  }
  function lookup(ids) {
    const { jobs, jobSelectionIndex } = latest.current.accounts;
    return ids.map(id => jobSelectionIndex.find(job => job.id === id) || jobs.find(job => job.id === id));
  }
  function capture(ids, eligible) {
    const jobs = lookup(ids);
    if (!ids.length || jobs.some(job => !job)) throw changedAccount();
    return jobs.map(job => ({ identity: accountIdentity(job), eligible: Boolean(eligible(job)) }));
  }
  function verify(action, eligible) {
    const now = capture(action.ids, eligible);
    if (now.some((job, index) => job.identity !== action.snapshot[index].identity || job.eligible !== action.snapshot[index].eligible)) throw changedAccount();
    if (!now.some(job => job.eligible)) throw changedAccount();
  }
  function closeDialog(key, expected = view[key]) {
    if (current() && !session.pending && session.view[key] === expected) publish({ [key]: null });
  }
  function consumeBatch(data, action, label, attempt) {
    const receipt = decodeBatchSubmission(data, action.ids);
    removeConfirmed(attempt, receipt.confirmedIds);
    const message = describeBatchSubmission(receipt, label);
    latest.current.feedback.setError("");
    latest.current.feedback.showNotice(`${message.label}：${message.text}`, message.tone);
    return receipt;
  }
  function validateUploadWorkflow(target, method) {
    const { uploadTargets, cpa } = latest.current.settings;
    if (uploadTargetConflict(target, method)) throw new Error(CPA_AUTHORIZATION_HELP);
    if (target === "cpa" && (!latest.current.features.cpaUpload || !cpa.configured)) throw new Error("请先读取并保存 CPA 配置，再提交自动上传。");
    if (!uploadTargets[target]?.ready) throw new Error("上传配置尚未核实，请先在配置页确认保存结果。");
  }

  function manualUploadProblem(ids, target) {
    const selected = lookup(ids), { uploadTargets, cpa, sub2api } = latest.current.settings;
    if (!ids.length || selected.some(job => !job)) return "账号列表已变化，请关闭弹窗并重新选择账号。";
    if (selected.some(uploadBlockedJob)) return "请等待选中账号的当前流程结束或完成本地保存；2FA 结果未确认时需人工处理。";
    if (!selected.some(job => job.canDownload)) return "选中账号暂无可上传的授权，请先完成授权后重新选择。";
    if (!uploadTargets[target]?.available) return "当前服务不支持该上传目标，请重新选择。";
    if (target === "cpa" && selected.some(job => uploadTargetConflict(target, job.authorizationMethod))) return "CPA 仅支持 ChatGPT / Codex 授权，请取消选择 BPS 账号，或切换至 Sub2API。";
    if (target === "cpa" && selected.some(job => !job.canDownload)) return "CPA 需要每个选中账号都有已保存的授权，请取消选择尚无授权的账号。";
    if (!uploadTargets[target].ready) {
      if (target === "cpa") return cpa.loading || cpa.saving ? "CPA 配置正在读取或保存，请稍候。" : !cpa.loaded || cpa.error || cpa.uncertain ? "CPA 配置尚未核实，请读取当前配置后再上传。" : "请先配置 CPA 管理地址和管理密钥。";
      return sub2api.saving ? "Sub2API 配置正在保存，请稍候。" : !sub2api.ready ? "Sub2API 配置待核实，请在配置页确认安全迁移及保存结果后再上传。" : "请先配置 Sub2API 后端地址、管理员 API Key 和目标号池。";
    }
    return "";
  }
  function requestManualUpload(ids) {
    if (!canOpen()) return;
    const selected = [...new Set(ids)];
    if (!selected.length) return;
    const { uploadTarget } = latest.current.settings, flags = latest.current.features;
    const target = flags.sub2apiUpload && flags.cpaUpload ? uploadTarget : flags.cpaUpload ? "cpa" : "sub2api";
    if (flags.sub2apiUpload && flags.cpaUpload) publish({ manualUploadAction: { ids: selected, target } });
    else void uploadSelected(selected, target);
  }
  function setManualUploadTarget(target) {
    if (current() && !session.pending && view.manualUploadAction && session.view.manualUploadAction === view.manualUploadAction && latest.current.settings.uploadTargets[target]?.available) publish({ manualUploadAction: { ...view.manualUploadAction, target } });
  }
  function submitManualUpload() {
    if (current() && view.manualUploadAction && session.view.manualUploadAction === view.manualUploadAction) void uploadSelected(view.manualUploadAction.ids, view.manualUploadAction.target);
  }
  async function uploadSelected(ids, target) {
    if (!current() || session.pending) return;
    const problem = manualUploadProblem(ids, target), { feedback, settings, navigation } = latest.current;
    if (problem) {
      feedback.showNotice(problem, "warning");
      if (!session.view.manualUploadAction && settings.uploadTargets[target]?.available && !settings.uploadTargets[target].ready) navigation.configure(target);
      return;
    }
    const attempt = begin("upload");
    if (!attempt) return;
    publish({ manualUploadAction: null });
    const saved = settings.selectUploadTarget(target);
    feedback.setError("");
    feedback.showNotice(`${saved ? "" : "浏览器未能保存目标偏好，本次上传不受影响。"}${target === "cpa" ? "正在推送 CPA 授权文件，同名文件将更新，请稍候。" : "正在同步 Sub2API：检查已有账号并更新授权，请稍候。"}`, "active");
    function consume(value, requestError) {
      const decoded = decodeUploadReceipt(value, ids, target);
      removeConfirmed(attempt, decoded.confirmedIds);
      const presentation = describeUpload(decoded.receipt, { error: requestError?.message || "", target });
      latest.current.feedback.setError("");
      latest.current.feedback.showNotice(`${uploadTargetLabel(target)} · ${presentation.label}：${presentation.text}`, presentation.tone);
    }
    try {
      sending();
      const data = await apiFetch(token, `/api/${target}/upload`, { method: "POST", body: JSON.stringify(target === "cpa" ? { ids } : { ids, config: settings.sub2api.settings }) });
      if (owns(attempt)) consume(data);
    } catch (requestError) {
      if (!owns(attempt)) return;
      try {
        if (!requestError.data?.result) throw requestError;
        consume(requestError.data, requestError);
      } catch (receiptError) {
        const presentation = describeUpload({ uncertain: !requestError.status || receiptError.outcome === "unknown" || requestError.data?.uncertain === true || requestError.data?.outcome === "unknown" }, { error: receiptError.message, target });
        latest.current.feedback.setError("");
        latest.current.feedback.showNotice(`${uploadTargetLabel(target)} · ${presentation.label}：${presentation.text}`, presentation.tone);
      }
    } finally { finish(attempt); }
  }

  function createJob(event) {
    event.preventDefault();
    if (canOpen() && session.view.email.trim()) publish({ reloginAction: { kind: "create", email: session.view.email.trim(), authorizationMethod: "chatgpt", initial: true } });
  }
  async function createBatch(event) {
    event.preventDefault();
    if (!current() || !view.batchScope || session.view.batchScope !== view.batchScope || !session.view.batchText.trim()) return;
    const { proxy, mail } = latest.current.settings;
    if (!proxy.ready || !mail.ready) { publish({ batchError: "请先在配置页确认代理及邮件配置的安全迁移；已保存的邮件配置还需明确应用到当前进程。" }); return; }
    const attempt = begin("import");
    if (!attempt) return;
    const text = session.view.batchText;
    function adopt(receipt) {
      if (receipt.jobs.length) {
        const currentAccounts = latest.current.accounts;
        latest.current.navigation.imported();
        currentAccounts.setPage(1);
        if (currentAccounts.page === 1) adoptRows(attempt, receipt.jobs);
      }
    }
    try {
      sending();
      const data = await apiFetch(token, "/api/jobs/batch", { method: "POST", body: JSON.stringify({ text, timeZone: browserTimeZone(), ...(proxy.value.trim() ? { proxyUrl: proxy.value.trim() } : {}) }) });
      if (!owns(attempt)) return;
      const receipt = decodeImportReceipt(data, countBatchLines(text));
      adopt(receipt);
      if (receipt.outcome !== "completed") {
        const error = new Error(receipt.error || "部分导入尚未完成");
        error.data = receipt;
        throw error;
      }
      const warnings = receipt.jobs.filter(job => typeof job.credentialWarning === "string" && job.credentialWarning).length;
      latest.current.feedback.showNotice(`已导入 ${receipt.created} 个账号${receipt.updated ? `，更新 ${receipt.updated} 个账号资料` : ""}。账号已加入列表，请勾选后手动选择要执行的功能。${warnings ? ` ${warnings} 条存在凭据存储提醒，私有资料保存需核对。` : ""}`, warnings ? "warning" : "success");
      if (warnings) publish({ batchError: "账号记录已导入，但部分私有资料未确认可靠保存。已保留导入内容，请先查看账号的凭据存储提醒并核对保存结果，避免重复导入。" });
      else if (session.view.batchText === text) publish({ batchText: "", batchError: "", batchOpen: false, batchScope: null });
      else publish({ batchError: "本次导入已保存；当前新编辑的内容已保留，尚未提交。" });
      latest.current.feedback.setError("");
    } catch (requestError) {
      if (!owns(attempt)) return;
      let message = requestError.message;
      if (requestError.data?.jobs) {
        try {
          const receipt = decodeImportReceipt(requestError.data, countBatchLines(text));
          adopt(receipt);
          latest.current.feedback.showNotice(message, "warning");
        } catch (receiptError) { message = receiptError.message; }
      }
      publish({ batchError: `${message.replace(/[。.!！?？；;\s]+$/u, "")}。已保留导入内容，请先核对账号列表与保存结果，避免重复导入；确认后只提交需要处理的资料。` });
    } finally {
      if (owns(attempt)) latest.current.accounts.refresh();
      finish(attempt);
    }
  }

  const canAuthorize = (job, kind) => kind === "reauthorize" ? job.canRegenerate || job.canRetry : kind === "retry" ? job.canRetry : kind === "regenerate" ? job.canRegenerate : job.canForceRelogin;
  function openAuthorization(ids, kind, batch = false) {
    if (!canOpen()) return;
    try {
      const snapshot = capture(ids, job => canAuthorize(job, kind));
      const eligibleCount = snapshot.filter(job => job.eligible).length;
      if (!eligibleCount || kind === "reauthorize" && eligibleCount !== ids.length) return;
      const selected = lookup(ids), first = selected[0];
      publish({ reloginAction: { kind, ids, batch, snapshot, email: batch ? undefined : first.email, eligibleCount, skipped: ids.length - eligibleCount,
        initial: selected.some(job => job.status === "imported"), lockedMethod: kind === "retry" && first.canResume,
        authorizationMethod: selected.every(job => normalizeAuthorizationMethod(job.authorizationMethod) === normalizeAuthorizationMethod(first.authorizationMethod)) ? normalizeAuthorizationMethod(first.authorizationMethod) : "chatgpt" } });
    } catch (error) { latest.current.feedback.setError(error.message); }
  }
  async function submitReloginAction(options) {
    const action = view.reloginAction;
    if (!current() || !action || session.view.reloginAction !== action || session.pending) return;
    const { ids, batch, kind = "relogin" } = action;
    if (kind !== "create") verify(action, job => canAuthorize(job, kind));
    const { proxy, uploadTarget, uploadTargets, sub2api } = latest.current.settings;
    const localCompletion = kind === "retry" && action.lockedMethod;
    if (!localCompletion && !proxy.ready) throw new Error("代理配置尚未核实，请先在配置页确认安全迁移与保存结果。");
    const method = normalizeAuthorizationMethod(localCompletion ? action.authorizationMethod : options.authorizationMethod), target = normalizeUploadTarget(options.uploadTarget ?? uploadTarget);
    if (kind === "relogin" && options.uploadAfterRelogin) validateUploadWorkflow(target, method);
    const uploadAfterRelogin = Boolean(kind === "relogin" && latest.current.features.reloginAutoUpload && uploadTargets[target].available && options.uploadAfterRelogin && uploadTargets[target].configured);
    const attempt = begin(kind);
    if (!attempt) return;
    const label = `${authorizationMethodLabel(method)} 授权${uploadAfterRelogin ? `（授权成功后自动上传 ${uploadTargetLabel(target)}）` : ""}`;
    try {
      sending();
      const path = kind === "create" ? "/api/jobs" : batch ? `/api/jobs/${kind}-batch` : `/api/jobs/${ids[0]}/${kind}`;
      const data = await apiFetch(token, path, { method: "POST", body: JSON.stringify(localCompletion ? { authorizationMethod: method } : { ...(kind === "create" ? { email: action.email } : batch ? { ids } : {}), proxyUrl: proxy.value.trim(), authorizationMethod: method, ...(kind === "relogin" ? { uploadAfterRelogin } : {}), ...(uploadAfterRelogin ? target === "cpa" ? { uploadTarget: "cpa" } : { sub2apiConfig: sub2api.settings } : {}) }) });
      if (!owns(attempt)) return;
      if (batch) consumeBatch(data, action, label, attempt);
      else {
        const job = decodeSingleSubmission(data, kind === "create" ? { email: action.email } : { id: ids[0] });
        if (kind === "create") {
          const currentAccounts = latest.current.accounts;
          currentAccounts.setPage(1);
          if (currentAccounts.page === 1) adoptRows(attempt, [job]);
          if (session.view.email.trim() === action.email) publish({ email: "" });
        }
        latest.current.feedback.showNotice(localCompletion ? "已提交本地授权保存；不连接远端、不重新登录。" : `已提交 1 个账号的 ${authorizationMethodLabel(method)} 授权任务${uploadAfterRelogin ? `，成功后自动上传 ${uploadTargetLabel(target)}` : ""}。请在账号行查看进度。`, "info");
      }
      publish({ reloginAction: null });
      latest.current.feedback.setError("");
    } catch (error) {
      if (!owns(attempt)) return;
      if (!batch || !error.data?.jobs) throw error;
      consumeBatch(error.data, action, label, attempt);
      publish({ reloginAction: null });
    } finally { finish(attempt); }
  }

  const canTotp = (job, reset, complete) => complete ? job.canCompleteTotp : reset ? job.canResetTotp : job.canSetupTotp;
  function openTotp(ids, resetTotp, batch, completeOnly = false) {
    if (!canOpen()) return;
    try {
      const selected = lookup(ids), completeTotp = Boolean(!batch && completeOnly && selected[0]?.canCompleteTotp);
      const snapshot = capture(ids, job => canTotp(job, resetTotp, completeTotp)), eligibleCount = snapshot.filter(job => job.eligible).length;
      if (!eligibleCount) return;
      publish({ totpAction: { ids, snapshot, totpIntentId: resetTotp && !completeTotp ? pendingTotpIntent(ids) : undefined, resetTotp, completeTotp, batch, email: batch ? undefined : selected[0].email, eligibleCount, skipped: ids.length - eligibleCount, authorizationMethod: batch ? latest.current.accounts.selectedAuthorizationMethod : selected[0].authorizationMethod } });
    } catch (error) { latest.current.feedback.setError(error.message); }
  }
  async function submitTotpAction(options) {
    const action = view.totpAction;
    if (!current() || !action || session.view.totpAction !== action || session.pending) return;
    const { ids, resetTotp, completeTotp, batch, totpIntentId } = action;
    verify(action, job => canTotp(job, resetTotp, completeTotp));
    const { proxy, uploadTarget, uploadTargets, sub2api } = latest.current.settings, flags = latest.current.features;
    if (!completeTotp && !proxy.ready) throw new Error("代理配置尚未核实，请先在配置页确认安全迁移与保存结果。");
    const target = normalizeUploadTarget(options.uploadTarget ?? uploadTarget);
    const reloginAfterTotp = Boolean(!completeTotp && flags.forceRelogin && options.reloginAfterTotp);
    if (reloginAfterTotp && options.uploadAfterTotp) validateUploadWorkflow(target, options.authorizationMethod);
    const uploadAfterTotp = Boolean(uploadTargets[target].available && reloginAfterTotp && options.uploadAfterTotp && uploadTargets[target].configured);
    const attempt = begin(completeTotp ? "complete-2fa" : resetTotp ? "reset-2fa" : "setup-2fa");
    if (!attempt) return;
    function consume(data) {
      const receipt = batch ? consumeBatch(data, action, resetTotp ? "重置 2FA" : "设置 2FA", attempt) : (decodeSingleSubmission(data, { id: ids[0] }), null);
      if (totpIntentId && (!batch || receipt.queued === ids.length && !receipt.unknown)) acknowledgeTotpIntent(ids, totpIntentId);
      if (!batch) latest.current.feedback.showNotice(completeTotp ? "已提交本地保存；不连接远端、不重新登录、不修改 2FA。" : "已加入 1 个账号的 2FA 流程，当前状态见账号行，详细步骤见日志。", "info");
      publish({ totpAction: null });
      latest.current.feedback.setError("");
    }
    try {
      sending();
      const data = await apiFetch(token, completeTotp ? `/api/jobs/${ids[0]}/complete-2fa` : batch ? "/api/jobs/setup-2fa-batch" : `/api/jobs/${ids[0]}/setup-2fa`, { method: "POST", body: JSON.stringify(completeTotp ? {} : {
        ...(resetTotp ? { totpIntentId } : {}), ...(batch ? { ids } : {}), proxyUrl: proxy.value.trim(), resetTotp,
        logoutAllDevicesAfterTotp: Boolean(resetTotp && flags.logoutAllDevicesAfterTotp && options.logoutAllDevicesAfterTotp), reloginAfterTotp,
        ...(reloginAfterTotp ? { authorizationMethod: options.authorizationMethod } : {}), uploadAfterTotp,
        ...(uploadAfterTotp ? target === "cpa" ? { uploadTarget: "cpa" } : { sub2apiConfig: sub2api.settings } : {}),
      }) });
      if (owns(attempt)) consume(data);
    } catch (error) {
      if (!owns(attempt)) return;
      if (!batch || !error.data?.jobs) throw error;
      consume(error.data);
    } finally { finish(attempt); }
  }

  async function addPasswordSelected() {
    if (!canOpen() || !latest.current.accounts.canAddPasswordSelected) return;
    const { accounts, settings, feedback, confirm } = latest.current;
    if (!settings.proxy.ready) { feedback.setError("代理配置尚未核实，请先在配置页确认安全迁移与保存结果。"); return; }
    const ids = [...accounts.selectedJobIds], snapshot = capture(ids, job => job.canAddPassword);
    const eligibleCount = snapshot.filter(job => job.eligible).length, skipped = ids.length - eligibleCount;
    const attempt = begin("add-password");
    if (!attempt) return;
    try {
      if (!await confirm(`确定为选中的 ${eligibleCount} 个无密码账号生成并添加随机强密码吗？${skipped ? `另有 ${skipped} 个账号已有密码或不符合条件，将自动跳过。` : ""}`) || !owns(attempt)) return;
      verify({ ids, snapshot }, job => job.canAddPassword);
      if (!latest.current.settings.proxy.ready) throw new Error("代理配置尚未核实，请先确认保存结果。");
      sending();
      const data = await apiFetch(token, "/api/jobs/add-password-batch", { method: "POST", body: JSON.stringify({ ids, proxyUrl: latest.current.settings.proxy.value.trim() }) });
      if (owns(attempt)) consumeBatch(data, { ids }, "添加密码", attempt);
    } catch (error) {
      if (!owns(attempt)) return;
      try {
        if (!error.data?.jobs) throw error;
        consumeBatch(error.data, { ids }, "添加密码", attempt);
      } catch (receiptError) { latest.current.feedback.setError(receiptError.message); }
    } finally { finish(attempt); }
  }

  async function downloadSelection(source = false) {
    if (!canOpen()) return;
    const { accounts } = latest.current, ids = [...accounts.selectedJobIds];
    if (!ids.length || !source && !accounts.canDownloadSelected) return;
    const attempt = begin(source ? "source" : "download");
    if (!attempt) return;
    try {
      const response = await fetch(source ? "/api/jobs/export-source" : "/api/jobs/download-batch", { method: "POST", headers: { "content-type": "application/json", "x-console-token": token }, body: JSON.stringify({ ids }) });
      if (!owns(attempt)) return;
      if (!response.ok) throw new Error((await response.json()).error || (source ? "原始信息导出失败" : "批量下载失败"));
      await saveDownloadResponse(response, source ? `chatgpt-account-source-${ids.length}-accounts-${localTimestamp()}.txt` : `sub2api-import-oauth-${accounts.downloadableSelectedCount}-accounts-${localTimestamp()}.json`, () => owns(attempt));
      if (owns(attempt)) latest.current.feedback.setError("");
    } catch (error) { if (owns(attempt)) latest.current.feedback.setError(error.message); }
    finally { finish(attempt); }
  }
  async function deleteAccounts(ids, emailLabel = "") {
    if (!canOpen() || !ids.length || !latest.current.features.accountTrash || latest.current.trash.actionBusy || latest.current.trash.verificationRequired) return;
    const snapshot = capture(ids, () => true), trigger = removalFocus.capture(), attempt = begin("delete");
    if (!attempt) return;
    let focusOperation;
    try {
      if (!await latest.current.confirm("确定将" + (emailLabel ? "账号 " + emailLabel : "选中的 " + ids.length + " 条记录对应的账号") + "移入回收站吗？同邮箱的全部历史记录、日志、密码、2FA 与授权文件将一起保留，可在回收站恢复。Sub2API 中的账号不受影响。", { title: "移入回收站", confirmLabel: "移入回收站" }) || !owns(attempt)) return;
      verify({ ids, snapshot }, () => true);
      focusOperation = removalFocus.begin(trigger);
      sending();
      const receipt = await latest.current.trash.mutate("move", ids);
      if (owns(attempt)) removalFocus.complete(focusOperation, receipt?.count > 0 || ["unknown", "partial"].includes(receipt?.outcome));
      else focusOperation?.guard.cancel();
      focusOperation = null;
    } catch (error) { if (owns(attempt)) latest.current.feedback.setError(error.message); }
    finally { removalFocus.complete(focusOperation, false); finish(attempt); }
  }
  async function cancelAllRunningJobs() {
    const runningCount = (latest.current.accounts.stats.active || 0) + (latest.current.accounts.stats.queued || 0);
    if (!canOpen() || !runningCount) return;
    const attempt = begin("cancel-all");
    if (!attempt) return;
    try {
      if (!await latest.current.confirm(`确定停止全部 ${runningCount} 条进行中和排队任务吗？`) || !owns(attempt)) return;
      sending();
      const data = await apiFetch(token, "/api/jobs/cancel-all", { method: "POST" });
      if (!owns(attempt)) return;
      decodeCancellationReceipt(data);
      removeConfirmed(attempt, [...attempt.selection]);
      latest.current.feedback.setError("");
    } catch (error) { if (owns(attempt)) latest.current.feedback.setError(error.message); }
    finally { finish(attempt); }
  }

  return { ...view,
    setEmail: email => publish({ email }), setBatchText: batchText => { if (view.batchScope === session.view.batchScope) publish({ batchText }); }, setBatchError: batchError => publish({ batchError }),
    setBatchOpen: batchOpen => { if (batchOpen ? canOpen() : current() && !session.pending && view.batchScope === session.view.batchScope) publish({ batchOpen, batchScope: batchOpen ? {} : null }); },
    clearDrafts: () => { if (current() && !session.pending) publish(emptyView()); },
    setTotpAction: () => closeDialog("totpAction"), setReloginAction: () => closeDialog("reloginAction"), setManualUploadAction: () => closeDialog("manualUploadAction"),
    createJob, createBatch, manualUploadProblem, requestManualUpload, submitManualUpload, setManualUploadTarget,
    reauthorizeSelected: () => openAuthorization([...latest.current.accounts.selectedJobIds], "reauthorize", true),
    forceReloginSelected: () => openAuthorization([...latest.current.accounts.selectedJobIds], "relogin", true),
    forceReloginAccount: job => openAuthorization([job.id], "relogin"), authorizeAccount: (job, kind) => openAuthorization([job.id], kind), submitReloginAction,
    setupTotpSelected: (reset = false) => openTotp([...latest.current.accounts.selectedJobIds], reset, true),
    setupTotpAccount: (job, reset, completeOnly) => openTotp([job.id], reset, false, completeOnly), submitTotpAction, addPasswordSelected,
    downloadSelected: () => downloadSelection(), exportSelectedSource: () => downloadSelection(true), deleteAccounts,
    deleteSelected: () => deleteAccounts([...latest.current.accounts.selectedJobIds]), cancelAllRunningJobs,
  };
}
