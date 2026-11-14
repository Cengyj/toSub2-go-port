import React from "react";
import { Activity, ChevronDown, CircleAlert, Clock3, ShieldCheck } from "lucide-react";
import { formatInsightDate, getInsightExpirationJob, insightWindows, insightSourceIsHistorical, describeInsightReset } from "../lib/accountInsights.js";
import { PlanBadge } from "./PlanBadge.jsx";
import { AccountExpiration } from "./account-expiration.jsx";
import { UsageMeter } from "./account-usage.jsx";
import { describeSubscriptionObservation, subscriptionDisplayPlan } from "../lib/accountSubscription.js";

export function ObservationTime({ value, stale = false }) {
  return <p className={"insight-observed" + (stale ? " is-historical" : "")}><Clock3 size={12} aria-hidden="true" /><span>{value ? "观察时间：" + formatInsightDate(value) : "尚未取得有效观察时间"}</span></p>;
}

export function InsightWindow({ window, now = Date.now() }) {
  return <div className="insight-window"><UsageMeter window={window} now={now} /></div>;
}

// This presentation stays read-only: opening diagnostics does not query a source.
export function InsightErrors({ errors }) {
  if (!errors.length) return null;
  const groups = [];
  for (const error of errors) {
    let group = groups.find(item => item.component === error.component);
    if (!group) { group = { component: error.component, label: error.sourceLabel, errors: [] }; groups.push(group); }
    group.errors.push(error);
  }
  return <section className="insight-errors" data-tone={errors.every(error => error.tone === "warning") ? "warning" : "danger"} aria-labelledby="account-insight-errors-title">
    <div className="insight-errors-heading"><h3 id="account-insight-errors-title"><CircleAlert size={15} aria-hidden="true" />查询错误详情</h3><span className="insight-section-count">{errors.length} 项</span></div>
    <p className="insight-errors-intro">显示最新请求的错误；已有用量和授权仍保留，仅展示已脱敏的信息。</p>
    <div className="insight-error-grid">{groups.map(group => <section className="insight-error-source" key={group.component}>
      <h4>{group.label}</h4>
      <div className="insight-error-items">{group.errors.map((error, index) => <article className="insight-error-card" data-tone={error.tone} key={index}>
        <div className="insight-error-primary"><span className="insight-http-status" data-tone={error.tone}>{error.code === "cache_unavailable" ? "本地缓存" : error.httpStatus ? "HTTP " + error.httpStatus + (error.httpStatus === 429 ? " · 限流" : "") : "HTTP 状态未提供"}</span><p className="insight-error-message">{error.message}</p></div>
        <details className="insight-error-details">
          <summary tabIndex={0} aria-label={group.label + (group.errors.length > 1 ? "第 " + (index + 1) + " 项" : "") + "：诊断与恢复"}>诊断与恢复<ChevronDown size={12} aria-hidden="true" /></summary>
          <div className="insight-error-expanded">
            <p className="insight-error-origin">{error.originLabel}{error.originNote ? " · " + error.originNote : ""}</p>
            <dl className="insight-error-diagnostics"><dt>错误分类</dt><dd><code>{error.code || "错误码未提供"}</code></dd>
              {error.upstreamCode && <><dt>上游错误码</dt><dd>{error.upstreamCode}</dd></>}
              {error.upstreamType && <><dt>上游类型</dt><dd>{error.upstreamType}</dd></>}
              {error.transportReason && <><dt>网络原因</dt><dd>{error.transportReason}</dd></>}
            </dl>
            {!error.upstreamCode && !error.upstreamType && !error.transportReason && error.httpStatus > 0 && <p className="insight-error-history">此记录未包含进一步的安全诊断；旧缓存中未保存的内容无法恢复。</p>}
            <p className="insight-error-cooldown"><Clock3 size={12} aria-hidden="true" /><span>{error.cooldownText}</span></p>
            <p className="insight-error-next"><strong>下一步</strong><span>{error.hint}</span></p>
          </div>
        </details>
      </article>)}</div>
    </section>)}</div>
  </section>;
}

export function AccountRequestErrors({ errors }) {
  if (!errors.length) return null;
  const sources = [...new Set(errors.map(error => error.source))];
  return <section className="insight-errors insight-account-request-errors" data-tone={errors.every(error => error.tone === "warning") ? "warning" : "danger"} aria-labelledby="account-request-errors-title">
    <div className="insight-errors-heading"><h3 id="account-request-errors-title"><CircleAlert size={15} aria-hidden="true" />最新账户请求</h3><span className="insight-section-count">{errors.length} 项</span></div>
    <p className="insight-errors-intro">只显示最新一次已完成请求的状态；更新的成功结果会替换旧错误。</p>
    <div className="insight-error-grid">{sources.map(source => {
      const items = errors.filter(error => error.source === source);
      return <section className="insight-error-source" key={source}><h4>{items[0].sourceLabel}</h4><div className="insight-error-items">{items.map((error, index) => <article className="insight-error-card" data-tone={error.tone} key={index}>
        <div className="insight-error-primary"><span className="insight-http-status" data-tone={error.tone}>{error.historical ? "上次 " : ""}{error.httpStatus ? `HTTP ${error.httpStatus}${error.httpStatus < 300 ? " · " + error.label : error.limited ? " · 限流" : ""}` : error.label + " · HTTP 状态未提供"}</span><p className="insight-error-message">{error.message}</p></div>
        <ObservationTime value={error.observedAt} stale={error.historical} />
        {error.historical && <p className="insight-error-history">该记录早于最近一次账号操作，当前状态需由此来源再次查询确认。</p>}
        {error.code && <dl className="insight-error-diagnostics"><dt>错误分类</dt><dd><code>{error.code}</code></dd></dl>}
      </article>)}</div></section>;
    })}</div>
  </section>;
}

export function InsightDataSections({ job, snapshot, now, subscriptionAvailable = false, subscriptionRefreshState = {}, onRefreshSubscription }) {
  const usage = snapshot?.usage, bps = snapshot?.bps, subscription = snapshot?.subscription, remote = snapshot?.remote;
  const planObservation = describeSubscriptionObservation(job);
  const hasPlanObservation = Boolean(planObservation.observedAt);
  const displayPlan = subscriptionDisplayPlan(hasPlanObservation || subscriptionAvailable ? job : { ...job, planType: subscription?.planType }, subscriptionRefreshState);
  const observedSubscription = subscriptionRefreshState.subscription;
  const currentSubscription = observedSubscription?.identityVerified === true && Date.parse(observedSubscription.observedAt) === Date.parse(planObservation.observedAt) ? observedSubscription : subscription;
  const expirationSnapshot = currentSubscription === subscription ? snapshot : { ...snapshot, id: job.id, authorizationMethod: job.authorizationMethod, subscription: currentSubscription };
  const credits = usage?.credits;
  const windows = insightWindows(snapshot).filter(window => window && typeof window === "object");
  const historicalUsage = insightSourceIsHistorical(snapshot, "usage");
  const resetPending = windows.some(window => describeInsightReset(window, now).pending);
  const hasBPS = job.authorizationMethod === "bps" || Boolean(bps);
  const usageStatus = usage?.limitReached === true ? historicalUsage || resetPending ? "上次查询报告额度已用尽，当前是否恢复仍需查询确认。" : "上游明确报告额度已用尽；不代表授权失效。"
    : usage?.allowed === true ? "额度接口报告允许使用；不保证所有模型均可生成。"
    : usage?.allowed === false ? "额度接口报告当前不允许使用，请结合各窗口与错误原因判断。" : "额度允许状态未知。";
  return <div className="insight-detail-grid">
    <section className="insight-block insight-usage-block">
      <div className="insight-block-heading"><h3><Activity size={16} aria-hidden="true" />Codex 额度</h3><span className="insight-source-label">已用比例 · 额度接口</span></div>
      <ObservationTime value={usage?.observedAt} stale={historicalUsage} />
      {usage ? <>
        <div className="insight-window-grid">{windows.map((window, index) => <InsightWindow key={window.name || index} window={window} now={now} />)}</div>
        {!windows.length && <p className="insight-empty">上游未提供额度窗口。</p>}
        <p className="insight-source-note insight-usage-note">{usageStatus}</p>
        {(Array.isArray(usage.features) ? usage.features : []).map((feature, index) => <div className="insight-feature" key={index}><h4>{feature.name || feature.meteredFeature || "额外功能额度"}</h4><div className="insight-window-grid">{(Array.isArray(feature.windows) ? feature.windows : []).filter(window => window && typeof window === "object").map((window, number) => <InsightWindow key={number} window={window} now={now} />)}</div></div>)}
      </> : <p className="insight-empty">暂无可用的额度结果。BPS 权限可读不代表额度接口可读。</p>}
    </section>
    <div className={"insight-secondary-grid" + (hasBPS ? " has-bps" : "")}>
      <section className="insight-block insight-credits-block">
        <div className="insight-block-heading"><h3>可消费 credits</h3><span className="insight-source-label">Codex</span></div>
        <p className={"insight-credit-value" + (credits?.unlimited !== true && typeof credits?.balance !== "string" && typeof credits?.balance !== "number" ? " is-unknown" : "")}>{credits?.unlimited === true ? "无限" : typeof credits?.balance === "string" || typeof credits?.balance === "number" ? String(credits.balance) : "未知"}</p>
        <ObservationTime value={usage?.observedAt} stale={historicalUsage} />
        <p className="insight-source-note">Codex credits，不是美元、Token 数或 BPS 独立余额。</p>
        <div className="insight-reset-credits"><h4>额度重置券</h4>{usage?.resetCredits ? <>
          <p className="insight-reset-credit-count">可用数量<strong>{typeof usage.resetCredits.availableCount === "number" ? usage.resetCredits.availableCount : "未知"}</strong></p>
          <ObservationTime value={usage.resetCredits.observedAt} stale={insightSourceIsHistorical(snapshot, "resetCredits")} />
          {(Array.isArray(usage.resetCredits.credits) ? usage.resetCredits.credits : []).map((credit, index) => <p className="insight-credit-expiry" key={index}>券 {index + 1} 到期：{formatInsightDate(credit.expiresAt)}</p>)}
        </> : <p className="insight-empty">未读取或无可用明细；本功能不会消费重置券。</p>}</div>
      </section>
      {hasBPS && <section className="insight-block insight-bps-block">
        <div className="insight-block-heading"><h3><ShieldCheck size={15} aria-hidden="true" />BPS 模型权限</h3><span className="insight-source-label">独立来源</span></div>
        <ObservationTime value={bps?.observedAt} stale={insightSourceIsHistorical(snapshot, "bps")} />
        <p>{!bps ? "尚未取得 BPS 权限信息。" : bps.allowed === true ? "权限接口允许访问；尚未执行模型生成实测。" : bps.allowed === false ? "权限接口报告受限，不等于整个账号被封禁。" : "总体访问状态未知。"}</p>
        <div className="insight-model-list">{(Array.isArray(bps?.models) ? bps.models : []).map((model, index) => <div className="insight-model" key={index}><span>{model.label || model.id || "未命名模型"}</span><span className={model.restricted || model.available === false ? "is-restricted" : model.available === true ? "is-available" : ""}>{model.restricted ? "受限" : model.available === true ? "可访问" : model.available === false ? "暂不可访问" : "未知"}</span></div>)}</div>
      </section>}
      <section className="insight-block insight-subscription-block">
        <div className="insight-block-heading"><h3>账号与订阅观察</h3><span className="insight-source-label">{job.authorizationMethod === "bps" ? "BPS" : "ChatGPT"}</span></div>
        <dl className="insight-facts"><dt>套餐</dt><dd><PlanBadge value={displayPlan}
          onRefresh={subscriptionAvailable ? onRefreshSubscription : undefined}
          refreshDisabled={job.canRefreshSubscription !== true} refreshing={subscriptionRefreshState.busy} accountLabel={job.email}
          refreshHint={job.canRefreshSubscription !== true ? job.subscriptionRefreshUnavailableReason || "当前账号暂不可刷新套餐" : ""} /></dd>{subscriptionAvailable && <><dt>套餐来源</dt><dd>{hasPlanObservation ? planObservation.sourceLabel || "已核验的订阅查询" : "授权快照；不代表当前套餐"}</dd></>}<dt>已保存授权文件</dt><dd>{job.canDownload ? "是；不代表已验证实时可用" : "否"}</dd><dt>期限来源</dt><dd>{currentSubscription?.identityVerified === true ? "已核对账号归属的独立订阅记录" : "尚未取得可信订阅查询"}</dd><dt>到期时间</dt><dd><AccountExpiration job={getInsightExpirationJob(job, expirationSnapshot)} now={now} /></dd></dl>
        {subscriptionAvailable && onRefreshSubscription && <p className="insight-source-note">{job.canRefreshSubscription !== true ? job.subscriptionRefreshUnavailableReason || "当前账号暂不可刷新套餐" : "点击套餐胶囊使用已保存授权查询，不会重新登录；无需开启用量查询。"}</p>}
        {hasPlanObservation && currentSubscription?.observedAt && Date.parse(currentSubscription.observedAt) !== Date.parse(planObservation.observedAt) && <p className="insight-source-note">到期记录观察于 {formatInsightDate(currentSubscription.observedAt)}，与套餐查询时间独立。</p>}
        <p className="insight-source-note">手动到期优先；Token 到期、额度重置时间与订阅到期各自独立。</p>
      </section>
    </div>
    <section className="insight-block insight-remote-block">
      <div className="insight-block-heading"><h3>Sub2 远端状态</h3><span className="insight-source-label">独立巡检记录</span></div>
      <div className="insight-remote-summary"><p>{remote?.status ? "最近巡检记录：" + remote.status : "远端状态未知，尚无可用巡检记录。"}</p><ObservationTime value={remote?.observedAt} stale /></div>
      <p className="insight-source-note">来源：{remote?.source || "尚未读取巡检缓存"}。{remote?.reason || "已保存授权和可读额度均不代表 Sub2 远端正常。"}</p>
    </section>
  </div>;
}
