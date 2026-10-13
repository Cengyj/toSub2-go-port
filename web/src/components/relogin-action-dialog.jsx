import React, { useEffect, useRef, useState } from "react";
import { Check, ChevronRight, CircleAlert, LoaderCircle, LogIn, Send, Settings2, X } from "lucide-react";
import { DialogFrame } from "./ui.jsx";
import { AuthorizationMethodPicker, useAuthorizationMethodPreference } from "./authorization-method.jsx";
import "./account-actions.css";

export const RELOGIN_WORKFLOW_PREFERENCES_KEY = "tosub2.relogin-workflow-preferences-v1";

function readUploadPreference() {
  try {
    return JSON.parse(window.localStorage.getItem(RELOGIN_WORKFLOW_PREFERENCES_KEY) || "null")?.uploadAfterRelogin === true;
  } catch {
    return false;
  }
}

export function ReloginActionDialog({ action, uploadAvailable, uploadConfigured, onConfigureUpload, onSubmit, onClose }) {
  const kind = action.kind || "relogin";
  const isRelogin = kind === "relogin";
  const title = kind === "create" || action.initial ? "开始登录授权" : action.lockedMethod ? "继续授权流程" : isRelogin ? "重新登录并授权" : "重新授权";
  const [authorizationMethod, setAuthorizationMethod, authorizationMethodSaved] = useAuthorizationMethodPreference(action);
  const [preferredUpload, setPreferredUpload] = useState(readUploadPreference);
  const [uploadPreferenceSaved, setUploadPreferenceSaved] = useState(true);
  const saved = authorizationMethodSaved && uploadPreferenceSaved;
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const submitting = useRef(false);
  const mounted = useRef(false);
  const uploadAfterRelogin = Boolean(isRelogin && uploadAvailable && uploadConfigured && preferredUpload);
  useEffect(() => {
    mounted.current = true;
    return () => { mounted.current = false; };
  }, []);

  function changeUpload(checked) {
    setPreferredUpload(checked);
    try {
      window.localStorage.setItem(RELOGIN_WORKFLOW_PREFERENCES_KEY, JSON.stringify({ uploadAfterRelogin: checked }));
      setUploadPreferenceSaved(true);
    } catch {
      setUploadPreferenceSaved(false);
    }
  }

  async function submit(event) {
    event.preventDefault();
    if (submitting.current) return;
    submitting.current = true;
    setBusy(true);
    setError("");
    try {
      await onSubmit({ uploadAfterRelogin, authorizationMethod });
    } catch (requestError) {
      if (mounted.current) setError(requestError.message);
    } finally {
      submitting.current = false;
      if (mounted.current) setBusy(false);
    }
  }

  return <DialogFrame onClose={onClose} busy={busy}>
    <section className="batch-dialog relogin-action-dialog" role="dialog" aria-modal="true" aria-labelledby="relogin-action-title">
      <div className="dialog-header">
        <div className="account-dialog-heading"><span className="account-dialog-icon" aria-hidden="true"><LogIn size={21} /></span><div><h2 id="relogin-action-title">{title}</h2><p>{action.batch ? `已选 ${action.eligibleCount} 个可授权账号` : action.email}</p></div></div>
        <button type="button" className="icon-button" onClick={onClose} disabled={busy} aria-label="关闭重登授权窗口"><X size={18} /></button>
      </div>
      <form onSubmit={submit}>
        <p className="account-dialog-description">{action.lockedMethod ? "从已保存的授权进度继续执行。" : isRelogin ? "完整重新登录并生成新的授权文件，可在成功后同步到 Sub2API。" : kind === "create" || action.initial ? "选择授权方式后开始登录，验证码等输入会显示在账号行。" : "同一方式优先刷新已有授权；切换方式时将完整重新登录。"}</p>
        {action.skipped > 0 && <p className="dialog-hint">另有 {action.skipped} 个账号不符合条件，将跳过。</p>}
        <AuthorizationMethodPicker value={authorizationMethod} onChange={setAuthorizationMethod} disabled={busy} locked={action.lockedMethod} />
        {isRelogin && <div className="relogin-upload-option">
          <label className={`totp-workflow-option ${uploadAfterRelogin ? "is-selected" : ""} ${!uploadAvailable || !uploadConfigured ? "is-disabled" : ""}`}>
            <span className="totp-option-icon" aria-hidden="true"><Send size={18} /></span>
            <span className="totp-option-copy"><strong>成功后自动上传 Sub2API</strong><span>{!uploadAvailable ? "当前服务暂不支持 Sub2API 上传。" : !uploadConfigured ? "请先配置 Sub2API 地址和管理员 API Key。" : "授权成功后同步；已有账号更新授权并保留号池配置。"}</span></span>
            <span className="totp-option-switch"><input type="checkbox" role="switch" aria-label="成功后自动上传 Sub2API" checked={uploadAfterRelogin} disabled={busy || !uploadAvailable || !uploadConfigured} onChange={(event) => changeUpload(event.target.checked)} /><span aria-hidden="true" /></span>
          </label>
          {uploadAvailable && !uploadConfigured && <button type="button" className="totp-configure-link" disabled={busy} onClick={onConfigureUpload}><Settings2 size={13} />配置 Sub2API<ChevronRight size={13} /></button>}
        </div>}
        {isRelogin && <div className="relogin-flow-preview" aria-label="重登授权执行流程">
          <span className="relogin-flow-step"><LogIn size={15} aria-hidden="true" /><strong>重新登录</strong></span><ChevronRight size={13} aria-hidden="true" />
          <span className="relogin-flow-step"><Check size={15} aria-hidden="true" /><strong>保存新授权</strong></span>
          {uploadAfterRelogin && <><ChevronRight size={13} aria-hidden="true" /><span className="relogin-flow-step"><Send size={15} aria-hidden="true" /><strong>同步 Sub2API</strong></span></>}
        </div>}
        {!action.lockedMethod && <p className={`totp-preferences-hint ${saved ? "" : "unsaved"}`} role="status">{saved ? <Check size={13} aria-hidden="true" /> : <CircleAlert size={13} aria-hidden="true" />}{saved ? "选项自动保存在当前浏览器，仅确认后执行。" : "浏览器阻止保存，部分选项仅在当前窗口有效。"}</p>}
        {error && <p className="dialog-error" role="alert"><CircleAlert size={16} />{error}</p>}
        <div className="dialog-actions"><button type="button" className="secondary-button" disabled={busy} onClick={onClose}>取消</button><button type="submit" className="primary-button" disabled={busy}>{busy ? <LoaderCircle size={16} className="spin" /> : <LogIn size={16} />}{busy ? "正在提交…" : action.lockedMethod ? "继续流程" : isRelogin ? "开始重登授权" : "开始授权"}</button></div>
      </form>
    </section>
  </DialogFrame>;
}
