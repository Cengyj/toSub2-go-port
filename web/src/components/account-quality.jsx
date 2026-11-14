import React, { useEffect, useRef, useState } from "react";
import { Activity, Ban, Check, CircleAlert, Clock3, LoaderCircle, RefreshCw, X } from "lucide-react";
import { describeQuality, qualityDate, qualityNeverChecked, qualityProbeHistory } from "../lib/accountQuality.js";
import { compareRequestTime } from "../lib/accountHTTP.js";
import { DialogFrame } from "./ui.jsx";
import "./account-indicators.css";
import "./account-quality.css";
import { ManualProbePanel } from "./manual-probe.jsx";
import { ProbeFailure } from "./probe-failure.jsx";
import { ProbeModelSelect, useProbeModels, probeModelChoices, probeEfforts, effortLabel } from "./probe-model-select.jsx";

// Five display slots stay aligned across accounts. Only populated bars represent
// actual candy attempts; empty slots carry no result, time, or probe identity.
export function AccountQualityStatus({ snapshot, authorizationMethod, onOpen, accountLabel = "", stateUncertain = false }) {
  const view = describeQuality(snapshot, authorizationMethod), history = qualityProbeHistory(snapshot, "candy");
  const pristine = qualityNeverChecked(snapshot);
  const attempt = snapshot?.lastAttempt || {}, latest = history[history.length - 1];
  const attemptTime = attempt.finishedAt || (attempt.status === "success" ? view.result?.checkedAt : "");
  const historyTimeOrder = latest && Number.isFinite(Date.parse(attemptTime)) ? compareRequestTime(latest.finishedAt, attemptTime) : -1;
  const sameAttempt = latest?.status === attempt.status && (!attempt.checkId || latest.id === attempt.checkId);
  const historyIsLatest = latest && !["结果未知", "暂不支持"].includes(view.label)
    && ((!attempt.status || attempt.status === "never") && !attempt.errorCode
      || historyTimeOrder > 0 || historyTimeOrder === 0 && sameAttempt);
  let label = "等待数据", tone = "neutral", detail = "读取后显示检测状态";
  if (view.active) {
    label = view.running ? "检测中" : "排队中";
    detail = view.running ? "等待本次检测完成" : "等待开始检测";
  } else if (stateUncertain && (pristine || !snapshot)) {
    label = "状态待同步";
    detail = history.length ? "保留已取得的检测记录" : "等待账号检测状态同步";
  } else if (pristine) {
    label = "尚未检测";
    detail = "当前授权尚无检测记录";
  } else if (snapshot) {
    if (view.result?.total > 1 && attempt.status === "success") { label = history.length ? "历史记录" : "暂无糖果结果"; detail = "旧版多题测量保留在详情中，不作为糖果题结果"; }
    else if (view.persistenceWarning) { label = "保存待核实"; tone = "warning"; detail = "检测记录的保存状态待确认"; }
    else if (historyIsLatest) {
      const historical = latest.kind === "candy" && latest.status === "success" && view.stale;
      label = historical ? "历史记录" : latest.label;
      tone = historical ? "neutral" : latest.tone;
      detail = historical ? [view.staleLabel, latest.title].filter(Boolean).join("；") : latest.title;
    } else {
      label = ({ "结果未知": "结果待确认", "未检测": "暂无有效结果" })[view.label] || view.label;
      tone = ["success", "warning", "danger"].includes(view.tone) ? view.tone : "neutral";
      detail = view.summary || (view.result ? "已有测量记录，查看检测详情" : "查看检测详情");
    }
  }
  const stateLabel = stateUncertain ? "状态待同步" : label;
  const outcome = stateUncertain ? "unknown" : view.running ? "running" : view.queued ? "queued" : ["通过", "糖果通过"].includes(label) ? "passed" : label === "答案不符" ? "incorrect" : label === "检测失败" ? "failed" : "unknown";
  const historyNotice = stateUncertain ? "状态待同步；保留已取得的历史结果" : view.persistenceWarning ? "记录保存待核实" : view.stale ? view.staleLabel : "";
  const title = ["糖果题当前状态：" + stateLabel, detail, `固定 5 个位置，${history.length} 条糖果探针记录，${5 - history.length} 个空位；每条记录代表一次探针，左旧右新；空位表示暂无可显示记录`, ...history.map(item => item.title), historyNotice, "点击查看详情，不会自动发起检测"].filter(Boolean).join("；");
  const emptyHint = ["空位 · 暂无可显示记录", !history.length && stateLabel, historyNotice].filter(Boolean).join("\n");
  return <button type="button" className="account-quality quality-state-summary" onClick={onOpen} aria-label={[accountLabel, title].filter(Boolean).join("；")} aria-busy={view.running} data-state-label={stateLabel} data-current-outcome={outcome} data-tone={tone}>
    <span className="quality-run-track" data-unconfirmed={stateUncertain || view.persistenceWarning || undefined}>
      {Array.from({ length: 5 - history.length }, (_, index) => <span key={`empty-${index}`} className="quality-run-placeholder" data-caption={emptyHint} aria-hidden="true" />)}
      {history.map((item, index) => {
        const result = item.measured ? item.tone === "success" ? "passed" : "incorrect" : item.tone === "danger" ? "failed" : "unknown";
        const isLatest = index === history.length - 1;
        const hint = [item.title, "一次已记录的糖果探针", isLatest && "最近一条记录", historyNotice].filter(Boolean).join("；");
        return <span key={item.id} className="quality-run-bar" data-probe-id={item.id} data-outcome={result} data-latest={isLatest || undefined} data-caption={[`${isLatest ? "最近一条 · " : ""}${item.label}`, qualityDate(item.finishedAt), historyNotice].filter(Boolean).join("\n")} role="img" aria-label={hint} />;
      })}
    </span>
    {view.active && <span className="quality-run-activity">{label}</span>}
  </button>;
}

// A row opens read-only details. Probe invocation remains in the dialog.
// Only actual completed attempts occupy history cells, ordered oldest to newest.
export function AccountQualitySummary({ snapshot, authorizationMethod, onOpen, accountLabel = "" }) {
  const view = describeQuality(snapshot, authorizationMethod);
  const history = qualityProbeHistory(snapshot);
  if (!history.length && !view.active) return null;
  const ActivityIcon = view.running ? LoaderCircle : Clock3;
  const activityLabel = view.running ? "检测中" : "排队中";
  const context = [history.length && `最近 ${history.length} 次探针，左旧右新`, view.active && activityLabel, view.stale && view.staleLabel, view.persistenceWarning && "保存未确认"].filter(Boolean).join("；");
  const detailsHint = [context, ...history.map(item => item.title), "查看推理探针详情；不代表标准 IQ"].filter(Boolean).join("；");
  return <button type="button" className="account-quality quality-history-summary" onClick={onOpen} title={detailsHint} aria-label={[accountLabel, detailsHint].filter(Boolean).join("，")} aria-busy={view.running}>
    {history.length > 0 && <span className="quality-history-grid">{history.map(item => <span key={item.kind + ":" + item.id} className="quality-history-cell" data-tone={item.tone} data-kind={item.kind} title={item.title} role="img" aria-label={item.title} />)}</span>}
    {view.active && <span className="quality-history-activity"><ActivityIcon size={11} className={view.running ? "spin" : undefined} aria-hidden="true" />{activityLabel}</span>}
  </button>;
}
const numericSettings = [
  { key: "timeoutSeconds", label: "糖果题超时", unit: "秒", min: 1, max: 300 },
  { key: "cooldownSeconds", label: "检测间隔", unit: "秒", min: 0, max: 86400 },
  { key: "resultTTLSeconds", label: "结果有效期", unit: "秒", min: 1, max: 86400 },
];
export function AccountQualitySettings({ quality, jobs = [] }) {
  const { draft, setDraft, settingsLoaded, settingsLoading, saving, dirty, settingsError, uncertain, settingsVerificationRequired } = quality;
  const [saved, setSaved] = useState(false), [sourceId, setSourceId] = useState("");
  const sources = jobs.filter(job => ["bps", "chatgpt"].includes(job.authorizationMethod));
  const source = sources.find(job => job.id === sourceId)?.id || sources[0]?.id || "";
  const catalog = useProbeModels(quality, source);
  const efforts = probeEfforts(probeModelChoices(catalog).find(item => item.id === draft.model));
  const blocked = !settingsLoaded || settingsLoading || saving || settingsVerificationRequired;
  const change = (key, value) => { setSaved(false); setDraft(current => ({ ...current, [key]: value })); };
  async function save(event) { event.preventDefault(); setSaved(await quality.saveSettings()); }
  return <form className="section-card settings-form quality-settings" onSubmit={save} aria-label="推理探针配置">
    <div className="card-header"><h2>推理探针</h2><p>配置默认模型、检测时限与上传前检查。</p></div>
    <div className="card-body quality-settings-fields">
      {settingsLoading && <p role="status"><LoaderCircle size={14} className="spin" />正在读取服务端配置…</p>}
      <label className="insight-check quality-enable"><input type="checkbox" checked={draft.enabled} disabled={blocked} onChange={event => change("enabled", event.target.checked)} /><span><strong>启用推理探针</strong><small>糖果题答对即通过；鹈鹕绘图可在账号详情中手动检测。</small></span></label>
      <section className="quality-config-section" aria-labelledby="quality-default-heading">
        <div className="quality-section-heading"><h3 id="quality-default-heading">默认检测参数</h3><p>用于批量检测和上传前检查；账号详情支持单次选择。</p></div>
        <div className="quality-settings-grid">
          <label className="settings-field"><span>模型目录来源</span><select value={source} disabled={blocked || !sources.length} onChange={event => setSourceId(event.target.value)} aria-label="模型目录来源">{!sources.length && <option value="">暂无支持的账号</option>}{sources.map(job => <option key={job.id} value={job.id}>{job.email || job.id} · {job.authorizationMethod === "bps" ? "BPS" : "Codex"}</option>)}</select><small>读取所选账号的模型列表，选择后点击保存。</small></label>
          <ProbeModelSelect id="quality-default-model" value={draft.model} onChange={value => change("model", value)} catalog={catalog} disabled={blocked} />
        </div>
        <div className="quality-settings-grid quality-timing-grid">
          <label className="settings-field"><span>推理强度</span><select value={draft.reasoningEffort} disabled={blocked} onChange={event => change("reasoningEffort", event.target.value)}>{!efforts.includes(draft.reasoningEffort) && <option value={draft.reasoningEffort} disabled>当前配置：{effortLabel(draft.reasoningEffort)}</option>}{efforts.map(value => <option key={value} value={value}>{effortLabel(value)}</option>)}</select></label>
          {numericSettings.map(field => <label className="settings-field" key={field.key}><span>{field.label}<small>{field.unit}</small></span><input type="number" min={field.min} max={field.max} step="1" required value={draft[field.key]} disabled={blocked} onChange={event => change(field.key, event.target.value)} /></label>)}
        </div>
      </section>
      <fieldset className="quality-upload-policy"><legend>上传前检查</legend>
        <label className="insight-check"><input type="checkbox" checked={draft.beforeUpload} disabled={blocked || !draft.enabled} onChange={event => change("beforeUpload", event.target.checked)} /><span><strong>上传前检查探针结果</strong><small>适用于手动上传、重登、2FA 与巡检同步。</small></span></label>
        <div className="quality-policy-options"><label className="insight-check"><input type="checkbox" checked={draft.blockDegraded} disabled={blocked || !draft.enabled || !draft.beforeUpload} onChange={event => change("blockDegraded", event.target.checked)} /><span>答案不符时阻止上传</span></label>
        <label className="insight-check"><input type="checkbox" checked={draft.blockFailed} disabled={blocked || !draft.enabled || !draft.beforeUpload} onChange={event => change("blockFailed", event.target.checked)} /><span>失败或未知时阻止上传</span></label></div>
        <p className="dialog-hint">批量上传整批预检，任一账号未通过时整批不上传。过期结果会重新检查。</p>
      </fieldset>
      <p className="quality-settings-note">检测消耗模型额度。配置保存在本地服务，仅主动检测或上传前策略会发起请求。</p>
      {settingsError && <p className="dialog-error" role="alert">{settingsError}</p>}
      {(uncertain || quality.settings.persistenceWarning) && <p className="quality-warning" role="status">{quality.settings.persistenceWarning || "上次保存的持久性尚未确认，请重新读取核对，再明确保存。"}</p>}
      {(settingsError || settingsVerificationRequired || uncertain) && <button type="button" className="secondary-button" disabled={saving || settingsLoading} onClick={() => { setSaved(false); void quality.reloadSettings(); }}><RefreshCw size={14} />重新读取配置（放弃草稿）</button>}
      {saved && !dirty && !uncertain && !settingsError && <p className="provider-ready" role="status"><Check size={14} />推理探针配置已保存</p>}
    </div>
    <div className="dialog-footer"><button type="button" className="cancel-button" disabled={saving || !dirty} onClick={() => { quality.resetDraft(); setSaved(false); }}>放弃更改</button><button type="submit" className="primary-button" disabled={blocked || !dirty && !uncertain}>{saving ? <LoaderCircle size={15} className="spin" /> : <Check size={15} />}保存探针配置</button></div>
  </form>;
}
export function AccountQualityDialog({ job, quality, onClose, onCheck }) {
  const snapshot = quality.snapshots[job.id], view = describeQuality(snapshot, job.authorizationMethod), result = view.result, attempt = snapshot?.lastAttempt || {};
  const statusLabel = ["历史通过", "历史低分"].includes(view.label) ? "已过期" : view.label;
  const [error, setError] = useState(""), [question, setQuestion] = useState("candy"), [model, setModel] = useState(quality.settings.model || ""), [effort, setEffort] = useState(quality.settings.reasoningEffort || "high"), [manualBusy, setManualBusy] = useState(false);
  const mounted = useRef(true), modelChosen = useRef(false), catalog = useProbeModels(quality, job.id);
  const choices = probeModelChoices(catalog);
  const efforts = probeEfforts(choices.find(item => item.id === model));
  const supportedEffort = efforts.includes(effort) ? effort : efforts.includes("medium") ? "medium" : efforts[0];
  const locked = view.active || manualBusy || Boolean(quality.actionBusy) || quality.verificationRequired;
  useEffect(() => { mounted.current = true; return () => { mounted.current = false; }; }, []);
  useEffect(() => { if (catalog.loaded && !modelChosen.current && choices.length && !choices.some(item => item.id === model)) setModel((choices.find(item => !item.suggested) || choices[0]).id); }, [catalog.loaded, catalog.accountId]);
  useEffect(() => { if (!locked && effort !== supportedEffort) setEffort(supportedEffort); }, [locked, effort, supportedEffort]);
  const options = { model: model.trim(), reasoningEffort: effort };
  async function cancel() { setError(""); try { await quality.cancel([job.id]); } catch (failure) { if (mounted.current) setError(failure.message); } }
  const details = [
    ["模型", result?.model || "—"], ["规则", result?.ruleVersion || "—"],
    ["测量时间", qualityDate(result?.checkedAt)], ["耗时", Number.isFinite(result?.durationMs) ? `${(result.durationMs / 1000).toFixed(1)} 秒` : "—"],
    ["最新尝试", { never: "未检测", queued: "排队中", running: "检测中", success: "检测完成", failed: "检测失败", cancelled: "已取消", skipped: "已跳过" }[attempt.status] || "结果未知"],
    ["下次可检测", snapshot?.nextAllowedAt ? qualityDate(snapshot.nextAllowedAt) : "随时"],
  ];
  const independentResult = result && view.staleReason === "options_changed" && attempt.status === "success" && !view.active && !view.persistenceWarning;
  const measuredResult = result && (!view.historical || independentResult);
  const ResultIcon = view.running ? LoaderCircle : measuredResult ? result.verdict === "normal" ? Check : CircleAlert : view.tone === "danger" || view.tone === "warning" ? CircleAlert : Activity;
  const hasFailure = !view.active && (attempt.status === "failed" || view.label === "结果未知");
  const resultTime = hasFailure ? attempt.finishedAt || attempt.startedAt : result?.checkedAt;
  const resultTitle = view.active ? statusLabel : measuredResult ? result.total === 1 ? result.score === 1 ? "糖果题通过" : "答案不符" : `${result.score}/${result.total} 题正确` : statusLabel;
  return <DialogFrame onClose={onClose}><section className="batch-dialog quality-dialog" role="dialog" aria-modal="true" aria-labelledby="account-quality-title">
    <div className="dialog-header probe-dialog-header"><div className="probe-dialog-identity"><span className="probe-dialog-icon"><Activity size={19} /></span><div><h2 id="account-quality-title">推理探针</h2><p className="quality-email" title={job.email}>{job.email}</p></div></div><button type="button" className="icon-button" aria-label="关闭推理探针" onClick={onClose}><X size={18} /></button></div>
    <div className="probe-parameters">
      <label className="probe-question-field settings-field"><span>检测题目</span><select value={question} disabled={locked} aria-label="检测题目" onChange={event => setQuestion(event.target.value)}><option value="candy">糖果题 · 逻辑推理</option><option value="pelican">鹈鹕骑自行车 · 2D 动画</option></select></label>
      <ProbeModelSelect id="quality-run-model" value={model} onChange={value => { modelChosen.current = true; setModel(value); }} onManualChange={() => { modelChosen.current = true; }} catalog={catalog} disabled={locked} />
      <label className="probe-effort-field settings-field"><span>推理强度</span><select value={effort} disabled={locked} aria-label="推理强度" onChange={event => setEffort(event.target.value)}>{efforts.map(value => <option key={value} value={value}>{effortLabel(value)}</option>)}</select></label>
    </div>
    {question === "pelican" ? <ManualProbePanel job={job} quality={quality} options={options} onActivityChange={setManualBusy} /> : <div className="probe-candy-panel">
      <div className="probe-runbar"><p>答对即通过<span>{catalog.loading ? "正在读取模型，完成后可开始" : "本次选择仅用于当前检测"}</span></p><div><button type="button" className="primary-button" disabled={catalog.loading || !model.trim() || !efforts.includes(effort) || !quality.canSubmit([job.id])} onClick={() => onCheck(options)}>{view.running ? <LoaderCircle size={14} className="spin" /> : <Activity size={14} />}{view.active ? statusLabel : "开始检测"}</button>{view.active && <button type="button" className="secondary-button" disabled={Boolean(quality.actionBusy)} onClick={cancel}><Ban size={14} />取消</button>}</div></div>
      {(!quality.settings.enabled || snapshot?.unavailableReason) && <p className="dialog-hint">{!quality.settings.enabled ? "推理探针已关闭，请先在服务配置中启用。" : snapshot.unavailableReason}</p>}
      {(error || quality.error) && <p className="dialog-error" role="alert">{error || quality.error}</p>}
      {quality.verificationRequired && <p className="quality-warning" role="status">操作结果待核实。<button type="button" className="text-action" onClick={() => void quality.reload()}>核对检测状态</button></p>}
      <div className={`probe-result-area probe-candy-result is-${independentResult ? result.verdict === "normal" ? "success" : "warning" : view.tone}`} aria-busy={view.running}>
        <div className="probe-result-heading"><span>检测结果</span>{resultTime && <time dateTime={resultTime}>{qualityDate(resultTime, true)}</time>}</div>
        {hasFailure ? <div className="probe-failed-result"><ProbeFailure message={view.message} code={attempt.errorCode} httpStatus={attempt.httpStatus} title={resultTitle} tone={view.tone} />{result && <p className="probe-retained-result">上次有效测量：{result.score}/{result.total}<span>{qualityDate(result.checkedAt, true)} · {result.model}</span></p>}</div> :
        <div className="probe-result-content"><span className="probe-result-icon"><ResultIcon size={22} className={view.running ? "spin" : undefined} /></span><strong>{resultTitle}</strong><p>{view.active ? view.running ? "正在等待模型回答…" : "任务已受理，等待执行" : result ? independentResult ? "本次参数与默认配置不同，不用于默认上传策略" : view.historical ? `保留测量 ${result.score}/${result.total} · ${view.stale ? view.staleLabel : "请查看最新尝试说明"}` : result.total === 1 ? result.score === 1 ? "答案符合糖果题判定规则" : "已取得完整答案，与标准答案不符" : "保留此前规则的测量结果" : "选择题目与模型，开始一次检测"}</p>{result?.model && <small title={result.model}>{result.model}</small>}</div>}
      </div>
      {!hasFailure && (view.message || attempt.errorCode) && <div className="quality-diagnostic"><strong>最新尝试说明{attempt.errorCode ? ` · ${attempt.errorCode}` : ""}</strong><p>{view.message || "服务未返回详细原因。"}</p></div>}
      {view.persistenceWarning && <p className="quality-warning" role="status">检测结果保存未确认，上传前检查需要重新核实。</p>}
    </div>}
    <details className="probe-disclosure quality-question"><summary>题目与判定<span>{question === "candy" ? "糖果题" : "2D 动画"}</span></summary>{question === "candy" ? <div><p>黑袋中有三种口味、两种形状的糖果。圆形与五角星形可以凭手感分辨，口味无法分辨。最少取出多少颗，才能保证同时拿到形状不同的苹果味和桃子味糖果？</p><table><thead><tr><th scope="col">形状</th><th scope="col">苹果</th><th scope="col">桃子</th><th scope="col">西瓜</th></tr></thead><tbody><tr><th scope="row">圆形</th><td>7</td><td>9</td><td>8</td></tr><tr><th scope="row">五角星</th><td>7</td><td>6</td><td>4</td></tr></tbody></table><p>标准答案为 21。答对通过，完整整数答案不符时标记低分；不完整输出不计分。</p></div> : <p>创建一个 HTML，内容是 SVG 绘制一个鹈鹕骑自行车的 2D 动画，你不需要任何测试。生成结果可直接预览、放大、查看源码和下载，不参与糖果题评分。</p>}</details>
    {question === "candy" && <details className="probe-disclosure"><summary>检测详情<span>{result ? `${result.total === 1 ? "糖果题" : "此前测量"} · ${result.score}/${result.total}` : "暂无测量"}</span></summary><dl className="quality-detail-grid">{details.map(([label, value]) => <div key={label}><dt>{label}</dt><dd>{value}</dd></div>)}</dl></details>}
    <p className="probe-dialog-note">检测消耗模型额度，结果仅供参考。关闭窗口后，已受理的任务继续执行。</p>
  </section></DialogFrame>;
}
