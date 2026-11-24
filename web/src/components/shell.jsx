import React, { useEffect, useRef, useState } from "react";
import {
  Activity, Check, CheckCircle2, ChevronLeft, ChevronRight, CircleAlert, Clock3,
  Globe2, HardDrive, History, Hourglass, Layers3, LayoutGrid, LoaderCircle,
  MailCheck, Menu, Monitor, Moon, Network, Palette, Pause, PhoneIncoming,
  RefreshCw, Send, Settings2, ShieldCheck, SlidersHorizontal, Sun, Trash2, UsersRound,
} from "lucide-react";
import {
  ACCOUNT_PLAN_OPTIONS, formatDateTime, formatMailRequestSummary,
  normalizeExcludedPlanTypes,
  readLocalTextSetting, writeLocalTextSetting, THEME_STORAGE_KEY,
} from "../lib/console.js";
import { StatCard } from "./ui.jsx";
import { PlanOptionLabel } from "./PlanBadge.jsx";
import { describeMonitor } from "../lib/jobPresentation.js";
import { describeCPAMonitor } from "../lib/cpaMonitorPresentation.js";
import { MonitorResultNotice } from "./MonitorResultNotice.jsx";
import { ProtectedSettingsNotice } from "./protected-settings.jsx";

const themes = [
  { id: "light", label: "暖灰", icon: Sun },
  { id: "white", label: "纯白", icon: LayoutGrid },
  { id: "dark", label: "深色", icon: Moon },
  { id: "auto", label: "跟随系统", icon: Monitor },
];
const pages = [
  { id: "accounts", label: "账号管理", icon: UsersRound },
  { id: "trash", label: "回收站", icon: Trash2 },
  { id: "settings", label: "服务配置", icon: SlidersHorizontal },
  { id: "monitor", label: "号池监控", icon: Activity },
];

export function useTheme() {
  const [theme, setTheme] = useState(() => {
    const value = readLocalTextSetting(THEME_STORAGE_KEY);
    return themes.some((item) => item.id === value) ? value : "light";
  });
  const [error, setError] = useState("");
  useEffect(() => {
    document.documentElement.dataset.theme = theme;
  }, [theme]);
  const selectTheme = (value, persist = true) => {
    if (!themes.some((item) => item.id === value)) return;
    setTheme(value);
    setError(persist && !writeLocalTextSetting(THEME_STORAGE_KEY, value) ? "主题已在本页生效，但浏览器未能保存；刷新后可能恢复原主题。请检查站点存储权限。" : "");
  };
  return [theme, selectTheme, error];
}

export function AppShell({ page, onNavigate, theme, onThemeChange, connected, total, children, trashAvailable = false }) {
  const [collapsed, setCollapsed] = useState(false);
  const [mobileOpen, setMobileOpen] = useState(false);
  const mobileTrigger = useRef(null);
  const sidebar = useRef(null);
  const mainArea = useRef(null);
  const mainContent = useRef(null);
  const skipLink = useRef(null);

  useEffect(() => {
    const mobileViewport = window.matchMedia("(max-width: 768px)");
    const updateViewport = () => { if (!mobileViewport.matches) setMobileOpen(false); };
    mobileViewport.addEventListener("change", updateViewport);
    return () => mobileViewport.removeEventListener("change", updateViewport);
  }, []);

  useEffect(() => {
    if (!mobileOpen) return;
    const background = [mainArea.current, skipLink.current].filter(Boolean);
    const inertBefore = background.map((element) => element.hasAttribute("inert"));
    const overflowBefore = document.body.style.overflow;
    const controls = () => [...(sidebar.current?.querySelectorAll("button:not(:disabled), a[href], [tabindex='0']") || [])]
      .filter((element) => element.getClientRects().length && getComputedStyle(element).visibility !== "hidden");
    const focusNavigation = () => {
      const current = sidebar.current?.querySelector('[aria-current="page"]');
      (current || controls()[0] || sidebar.current)?.focus({ preventScroll: true });
    };

    focusNavigation();
    background.forEach((element) => element.setAttribute("inert", ""));
    document.body.style.overflow = "hidden";
    const keyboard = (event) => {
      if (event.key === "Escape") {
        event.preventDefault();
        setMobileOpen(false);
        return;
      }
      if (event.key !== "Tab") return;
      const items = controls();
      const first = items[0], last = items[items.length - 1];
      if (!first) { event.preventDefault(); return; }
      const inside = sidebar.current?.contains(document.activeElement);
      if (event.shiftKey && (!inside || document.activeElement === first)) {
        event.preventDefault();
        last.focus();
      } else if (!event.shiftKey && (!inside || document.activeElement === last)) {
        event.preventDefault();
        first.focus();
      }
    };
    document.addEventListener("keydown", keyboard);
    return () => {
      document.removeEventListener("keydown", keyboard);
      background.forEach((element, index) => {
        if (!inertBefore[index]) element.removeAttribute("inert");
      });
      document.body.style.overflow = overflowBefore;
      if (mobileTrigger.current?.getClientRects().length) mobileTrigger.current.focus({ preventScroll: true });
      else if (sidebar.current?.isConnected) focusNavigation();
    };
  }, [mobileOpen]);

  function skipToContent(event) {
    event.preventDefault();
    mainContent.current?.focus();
    mainContent.current?.scrollIntoView({ block: "start" });
  }

  return (
    <div className={`app-shell ${collapsed ? "sidebar-collapsed" : ""} ${mobileOpen ? "mobile-nav-open" : ""}`}>
      <a ref={skipLink} className="skip-link" href="#main-content" onClick={skipToContent}>跳转到主要内容</a>
      {mobileOpen && (
        <button
          type="button"
          className="sidebar-backdrop"
          aria-label="关闭导航菜单"
          tabIndex={-1}
          onClick={() => setMobileOpen(false)}
        />
      )}
      <aside
        ref={sidebar}
        id="main-navigation"
        className="sidebar"
        aria-label="主导航"
        role={mobileOpen ? "dialog" : undefined}
        aria-modal={mobileOpen ? true : undefined}
        tabIndex={-1}
      >
        <div className="sidebar-brand">
          <div className="brand-mark"><Layers3 size={22} aria-hidden="true" /></div>
          <div className="brand-name"><strong>toSub2</strong><span>ACCOUNT CONSOLE</span></div>
        </div>
        <span className="sidebar-label">工作空间</span>
        <nav className="sidebar-nav">
          {pages.filter(item => item.id !== "trash" || trashAvailable).map(({ id, label, icon: Icon }) => (
            <button
              key={id}
              type="button"
              className={`nav-link ${page === id ? "active" : ""}`}
              aria-current={page === id ? "page" : undefined}
              title={label}
              onClick={() => {
                onNavigate(id);
                setMobileOpen(false);
              }}
            >
              <Icon size={19} aria-hidden="true" /><span>{label}</span>
              {id === "accounts" && <span className="nav-count">{total}</span>}
            </button>
          ))}
        </nav>
        <div className="sidebar-footer">
          <span className="sidebar-label">本地工作空间</span>
          <div className="sidebar-local"><HardDrive size={16} aria-hidden="true" /><span>本地控制台</span></div>
          <p>账号授权与安全管理</p>
        </div>
        <button
          type="button"
          className="sidebar-collapse"
          onClick={() => setCollapsed((value) => !value)}
          aria-label={collapsed ? "展开侧栏" : "收起侧栏"}
          title={collapsed ? "展开侧栏" : "收起侧栏"}
        >
          {collapsed ? <ChevronRight size={16} /> : <ChevronLeft size={16} />}
          <span>{collapsed ? "展开侧栏" : "收起侧栏"}</span>
        </button>
      </aside>
      <div ref={mainArea} className="main-area">
        <header className="topbar">
          <div className="breadcrumb">
            <button
              ref={mobileTrigger}
              type="button"
              className="icon-button mobile-menu-button"
              onClick={() => setMobileOpen((value) => !value)}
              aria-label="切换导航菜单"
              aria-expanded={mobileOpen}
              aria-controls="main-navigation"
            >
              <Menu size={18} aria-hidden="true" />
            </button>
            <span>工作空间</span><ChevronRight size={13} />
            <h1>{pages.find((item) => item.id === page)?.label}</h1>
          </div>
          <div className="header-actions">
            <span className={`connection-status ${connected ? "connected" : "offline"}`}>
              {connected ? "服务已连接" : "连接中断"}
            </span>
            <label className="theme-select">
              <Palette size={15} aria-hidden="true" />
              <select value={theme} onChange={(event) => onThemeChange(event.target.value)} aria-label="外观主题">
                {themes.map((item) => <option key={item.id} value={item.id}>{item.label}</option>)}
              </select>
            </label>
          </div>
        </header>
        <main ref={mainContent} className={`workspace${page === "accounts" ? " workspace-accounts" : ""}`} id="main-content" tabIndex={-1}>{children}</main>
      </div>
    </div>
  );
}

export function AccountsOverview({ stats, total, insightsEnabled = false, refreshSummary, refreshError = "", onVerifyRefresh }) {
  const description = refreshSummary?.description || "展示当前账号查询状态；可通过选中刷新或账号详情更新用量。";
  return (
    <section className="accounts-overview" aria-labelledby="account-status-title">
      <div className="accounts-overview-header">
        <div>
          <h2 id="account-status-title">{insightsEnabled ? "账号状态与用量" : "账号状态"}</h2>
          {insightsEnabled && <p role="status">{description}</p>}
        </div>
      </div>
      {insightsEnabled && refreshError && <div className="insight-refresh-warning" role="status"><span>刷新状态暂不可确认：{refreshError}。最近用量仍保留，核对状态不会重新提交查询。</span>{onVerifyRefresh && <button type="button" className="secondary-button" onClick={onVerifyRefresh}>核对刷新状态</button>}</div>}
    <div className="stats-grid" aria-label="账号与任务统计">
      <StatCard icon={UsersRound} label="全部账号" value={total} caption="当前工作空间" />
      <StatCard icon={Activity} label="进行中" value={stats.active || 0} caption="正在执行账号操作" tone="active" />
      <StatCard icon={Hourglass} label="排队中" value={stats.queued || 0} caption="等待执行账号操作" />
      <StatCard icon={CheckCircle2} label="授权可用" value={stats.available ?? stats.completed ?? 0} caption="已保存可下载的授权" tone="success" />
    </div>
    </section>
  );
}

const sections = [
  { id: "overview", label: "配置概览", icon: LayoutGrid },
  { id: "sms", label: "接码平台", icon: PhoneIncoming },
  { id: "mail", label: "邮件 API", icon: MailCheck },
  { id: "sub2api", label: "Sub2API", icon: Send },
  { id: "cpa", label: "CPA", icon: Send },
  { id: "insights", label: "账号查询", icon: Activity },
  { id: "quality", label: "推理探针", icon: Activity },
  { id: "trash", label: "回收站", icon: Trash2 },
  { id: "network", label: "代理网络", icon: Network },
  { id: "appearance", label: "外观与数据", icon: Palette },
];

function ConfigurationCard({ icon: Icon, title, description, summary, ready, tone, onOpen }) {
  return (
    <article className={`config-card tone-${tone}`}>
      <div className="config-card-head">
        <div className="config-card-icon"><Icon size={21} aria-hidden="true" /></div>
        <h2>{title}</h2>
        <span className={`config-card-status ${ready ? "ready" : "incomplete"}`}>
          {ready ? <Check size={12} /> : <CircleAlert size={12} />}
          {ready ? "已配置" : "待配置"}
        </span>
      </div>
      <div className="config-card-body">
        <p>{description}</p>
        <div className="config-summary">{summary}</div>
      </div>
      <div className="config-card-footer">
        <button type="button" className="text-action" onClick={onOpen} aria-label={`管理${title}配置`}>
          管理配置<ChevronRight size={15} />
        </button>
      </div>
    </article>
  );
}

export function ConfigurationPage({
  section, onSectionChange, dirty, saving, sms, mail, sub2api, cpaAvailable = false, cpa,
  proxy, proxyDraft, onProxyDraftChange, onProxySave, proxyProtection, theme, onThemeChange,
  onClear, clearDisabled, clearError, children, insightSettings, insightSettingsLoaded, insightSettingsError, unconfirmed = false, trashAvailable = false, trashSettings, qualityAvailable = false, quality,
}) {
  const [proxyError, setProxyError] = useState("");
  const [savedNotice, setSavedNotice] = useState(false);
  const proxyContext = useRef(null);
  proxyContext.current = proxyProtection?.context;

  async function saveProxy(event) {
    event.preventDefault();
    const context = proxyContext.current;
    const value = proxyDraft.trim();
    try {
      if (value && !["http:", "https:", "socks5:", "socks5h:"].includes(new URL(value).protocol)) throw new Error();
    } catch {
      setProxyError("请输入有效的 HTTP、HTTPS、SOCKS5 或 SOCKS5H 代理地址。");
      return;
    }
    const saved = await onProxySave(value);
    if (context !== proxyContext.current || proxyProtection?.enabled) return;
    if (!saved) {
      setProxyError("浏览器未能保存代理配置，请检查站点存储权限后重试。现有配置未变更。");
      setSavedNotice(false);
      return;
    }
    setProxyError("");
    setSavedNotice(true);
  }

  return (
    <section className="configuration-page" aria-label="服务配置">
      <div className="page-toolbar configuration-toolbar">
      <nav className="settings-tabs" aria-label="配置分类">
        {sections.filter(item => (item.id !== "trash" || trashAvailable) && (item.id !== "quality" || qualityAvailable) && (item.id !== "cpa" || cpaAvailable)).map(({ icon: Icon, ...item }) => (
          <button
            type="button"
            key={item.id}
            className={section === item.id ? "active" : ""}
            aria-current={section === item.id ? "page" : undefined}
            disabled={saving}
            onClick={() => onSectionChange(item.id)}
          >
            <Icon size={15} aria-hidden="true" /><span>{item.label}</span>
            {section === item.id && dirty && <i className="dirty-dot" />}
          </button>
        ))}
      </nav>
        <span className={`configuration-state ${saving ? "saving" : dirty || unconfirmed ? "dirty" : "synced"}`} role="status">
          {saving
            ? <LoaderCircle size={14} className="spin" aria-hidden="true" />
            : dirty || unconfirmed
              ? <CircleAlert size={14} aria-hidden="true" />
              : <CheckCircle2 size={14} aria-hidden="true" />}
          {saving ? "正在保存…" : unconfirmed ? "保存待核实" : dirty ? "有未保存的更改" : "无待保存更改"}
        </span>
      </div>
      {section === "overview" && (
        <>
          <div className="config-section-title">
            <h2>连接服务</h2>
            <p>配置用于之后发起的账号操作。</p>
          </div>
          <div className="config-grid">
            <ConfigurationCard
              icon={PhoneIncoming}
              tone="sms"
              title="短信接码平台"
              description="配置服务商、号码资源与短信验证码获取方式。"
              summary={sms.name || "尚未选择服务商"}
              ready={sms.ready}
              onOpen={() => onSectionChange("sms")}
            />
            <ConfigurationCard
              icon={MailCheck}
              tone="mail"
              title="邮件 API"
              description="通过邮箱接口自动获取登录验证码。"
              summary={`${mail.method} · ${formatMailRequestSummary(mail)}`}
              ready={Boolean(mail.url || mail.headersText !== "{}" || mail.method === "POST")}
              onOpen={() => onSectionChange("mail")}
            />
            <ConfigurationCard
              icon={Send}
              tone="sub2api"
              title="Sub2API"
              description="上传授权结果，设置目标号池与账号参数。"
              summary={sub2api.baseUrl || "尚未配置上传目标"}
              ready={Boolean(sub2api.baseUrl && sub2api.adminApiKey)}
              onOpen={() => onSectionChange("sub2api")}
            />
            {cpaAvailable && <ConfigurationCard icon={Send} tone="sub2api" title="CPA" description="推送 ChatGPT / Codex 授权到 CLIProxyAPI。" summary={cpa?.loaded ? cpa.settings?.baseUrl || "尚未配置推送目标" : "请读取服务端配置"} ready={Boolean(cpa?.configured)} onOpen={() => onSectionChange("cpa")} />}
            <ConfigurationCard
              icon={Network}
              tone="network"
              title="代理网络"
              description="为新任务指定出口，支持 HTTP 与 SOCKS 代理。"
              summary={proxy ? "已保存账号代理" : "当前使用本地网络"}
              ready={Boolean(proxy)}
              onOpen={() => onSectionChange("network")}
            />
            <ConfigurationCard icon={Activity} tone="sub2api" title="账号用量与状态" description="独立查询额度、BPS 权限和订阅信息，不触发重新登录。" summary={!insightSettingsLoaded ? insightSettingsError ? "配置暂不可读，请进入核对" : "正在读取服务端配置…" : insightSettings?.enabled ? insightSettings.automatic ? `自动查询 · 每 ${insightSettings.intervalMinutes} 分钟` : "已启用 · 仅手动查询" : "已关闭 · 历史缓存仍可查看"} ready={insightSettingsLoaded && insightSettings?.enabled === true} onOpen={() => onSectionChange("insights")} />
            {qualityAvailable && <ConfigurationCard icon={Activity} tone="sub2api" title="推理探针" description="主动发送一道糖果推理题（candy-v1），检测模型表现并设置上传前策略。会消耗额度。" summary={!quality?.settingsLoaded ? quality?.settingsError ? "配置待核实" : "正在读取服务端配置…" : quality.settings.enabled ? quality.settings.beforeUpload ? "已启用 · 上传前检查" : "已启用 · 仅主动检测" : "已关闭 · 历史测量仍可查看"} ready={quality?.settingsLoaded && quality.settings.enabled && !quality.settingsVerificationRequired} onOpen={() => onSectionChange("quality")} />}
          </div>
        </>
      )}
      {children}
      {section === "network" && (
        <form className="section-card settings-form" onSubmit={saveProxy}>
          <div className="card-header">
            <h2>账号代理</h2>
            <p>留空时使用本地网络。新配置用于之后发起的账号操作。</p>
          </div>
          <div className="card-body">
            <label className="settings-field">
              <span>代理地址</span>
              <input
                className="input"
                value={proxyDraft}
                onChange={(event) => {
                  onProxyDraftChange(event.target.value);
                  setSavedNotice(false);
                }}
                placeholder="socks5h://用户名:密码@主机:端口"
                autoComplete="off"
                spellCheck="false"
                aria-label="代理 IP 地址"
              />
            </label>
            <p className="dialog-hint">
              支持 http://、https://、socks5:// 和 socks5h://。用户名包含 -sid- 时自动轮换会话编号。
            </p>
            {proxyError && <p className="dialog-error" role="alert">{proxyError}</p>}
            <ProtectedSettingsNotice state={proxyProtection} />
            {savedNotice && (
              <p className="provider-ready" role="status"><Check size={14} />代理配置已保存</p>
            )}
          </div>
          <div className="dialog-footer">
            <button
              type="button"
              className="cancel-button"
              disabled={proxyDraft === proxy || proxyProtection?.saving}
              onClick={() => {
                onProxyDraftChange(proxy);
                setProxyError("");
              }}
            >
              放弃更改
            </button>
            <button type="submit" className="primary-button" disabled={proxyProtection?.enabled ? !proxyProtection.canSave || proxyDraft === proxy && !proxyProtection.migrationRequired : proxyDraft === proxy}>
              <Check size={15} />保存配置
            </button>
          </div>
        </form>
      )}
      {section === "appearance" && (
        <div className="settings-stack">
          <section className="section-card">
            <div className="card-header">
              <h2>界面外观</h2>
              <p>主题偏好保存在当前浏览器，切换后立即生效。</p>
            </div>
            <div className="card-body theme-options">
              {themes.map(({ icon: Icon, ...item }) => (
                <button
                  type="button"
                  key={item.id}
                  className={`theme-option ${theme === item.id ? "selected" : ""}`}
                  aria-pressed={theme === item.id}
                  onClick={() => onThemeChange(item.id)}
                >
                  <span className={`theme-preview ${item.id}`} aria-hidden="true"><i /><i /><i /></span>
                  <span><span className="theme-option-label"><Icon size={14} aria-hidden="true" />{item.label}</span>{theme === item.id && <Check size={15} aria-hidden="true" />}</span>
                </button>
              ))}
            </div>
          </section>
          <section className="section-card">
            <div className="card-header">
              <h2>本地配置</h2>
              <p>{proxyProtection?.enabled ? "清除当前浏览器的主题和账号操作偏好。服务端保护配置、任务和凭据，以及尚未迁移的浏览器秘密均会保留。" : "清除当前浏览器保存的接码平台、邮件 API、Sub2API、代理、主题和账号操作偏好。服务端任务与凭据需通过本地服务清理。"}</p>
            </div>
            <div className="card-body">
              <button type="button" className="secondary-button danger-outline" onClick={onClear} disabled={clearDisabled}>
                <Trash2 size={16} />{proxyProtection?.enabled ? "清除浏览器偏好" : "清除本机网页配置"}
              </button>
              {clearError && <p className="dialog-error" role="alert">{clearError}</p>}
            </div>
          </section>
        </div>
      )}
    </section>
  );
}

export function MonitorPage({ target = "sub2api", onTargetChange, status, onConfigure, onCheck, checking, available, targetAvailability = {}, checkDisabled = false, loading = false, error = "", notice = "", onRefresh, policy = {}, queryOverview }) {
  const [monitorTab, setMonitorTab] = useState("config");
  const tabButtons = useRef({});
  const monitorTabs = [{ id: "records", label: "巡检记录", icon: History }, { id: "config", label: "巡检配置", icon: SlidersHorizontal }];
  function handleTabKey(event, current) {
    const index = monitorTabs.findIndex(tab => tab.id === current);
    const nextIndex = event.key === "Home" ? 0 : event.key === "End" ? monitorTabs.length - 1
      : event.key === "ArrowRight" ? (index + 1) % monitorTabs.length
      : event.key === "ArrowLeft" ? (index + monitorTabs.length - 1) % monitorTabs.length : -1;
    if (nextIndex < 0) return;
    event.preventDefault();
    const next = monitorTabs[nextIndex].id;
    setMonitorTab(next);
    tabButtons.current[next]?.focus();
  }
  const targetLabel = target === "cpa" ? "CPA" : "Sub2API";
  const snapshot = status || {};
  const savedTarget = snapshot.target || target;
  const isCPA = savedTarget === "cpa";
  const savedTargetLabel = isCPA ? "CPA" : "Sub2API";
  const targetChanged = target !== savedTarget;
  const targetConfigured = snapshot.targets?.[target]?.configured ?? (!targetChanged && snapshot.configured);
  const waitingForState = loading && !snapshot.lastCheckAt;
  const running = snapshot.running || checking;
  const presentation = (isCPA ? describeCPAMonitor : describeMonitor)({ ...snapshot, running });
  const result = presentation.result;
  const draft = policy.draft || { target, enabled: snapshot.enabled, excludedPlanTypes: snapshot.excludedPlanTypes, intervalMinutes: snapshot.intervalMinutes, cooldownMinutes: snapshot.cooldownMinutes };
  const excludedPlans = normalizeExcludedPlanTypes(draft.excludedPlanTypes);
  const policyBusy = Boolean(policy.saving || policy.loading);
  const policyDisabled = !available || !policy.loaded || policyBusy || checking || policy.uncertain || policy.conflict;
  const statusWarning = Boolean(presentation.error || snapshot.configurationError || snapshot.uncertain);
  const monitorEnabled = snapshot.enabled && snapshot.configured && !snapshot.configurationError && !snapshot.uncertain;
  const statusLabel = waitingForState ? "正在读取" : snapshot.configurationError || snapshot.uncertain ? "配置待核实" : presentation.status.label;
  const StatusIcon = running ? LoaderCircle : statusWarning ? CircleAlert : monitorEnabled ? ShieldCheck : Pause;
  const alerts = [...new Set([error, policy.error].filter(Boolean))];
  const notices = [...new Set([notice, policy.notice].filter(Boolean))];
  const metrics = isCPA ? [
    ["checked", "已检查授权文件"], ["started", "已排队自动重登"], ["excludedPlan", "按套餐跳过"], ["cooldown", "冷却中"],
    ["busy", "正在处理"], ["ineligible", "不符合重登条件"], ["missing", "未匹配本地账号"], ["failed", "入队失败"],
  ] : [
    ["checked", "已检查异常记录"], ["started", "已排队自动重登"], ["updated", "已更新账号"],
    ["excludedPlan", "按套餐跳过"], ["autoUploadPaused", "自动回传暂停"],
  ];
  return (
    <section className="monitor-page" aria-label="号池监控">
      <div className="monitor-toolbar">
      <div className="settings-tabs monitor-tabs" role="tablist" aria-label="号池监控视图">
        {monitorTabs.map(({ id, label, icon: Icon }) => <button key={id} type="button" role="tab" id={`pool-monitor-${id}-tab`}
          ref={node => { tabButtons.current[id] = node; }} aria-controls={`pool-monitor-${id}-panel`} aria-selected={monitorTab === id}
          aria-label={label} tabIndex={monitorTab === id ? 0 : -1} className={monitorTab === id ? "active" : ""}
          onClick={() => setMonitorTab(id)} onKeyDown={event => handleTabKey(event, id)}>
          <Icon size={15} aria-hidden="true" /><span>{label}</span>
          {id === "config" && policy.dirty && <span className="monitor-tab-draft" aria-hidden="true">未保存</span>}
        </button>)}
      </div>
      <div className="monitor-toolbar-actions" role="group" aria-label="巡检操作">
        {onRefresh && <button className="secondary-button" type="button" onClick={onRefresh} disabled={!available || loading || checking || policy.saving}>
          <RefreshCw className={loading ? "spin" : ""} size={15} />读取状态
        </button>}
        <button className="primary-button" type="button" onClick={onCheck}
          disabled={!available || checkDisabled || policy.dirty || policy.uncertain || policy.conflict || policy.repairRequired || policyBusy || !snapshot.configured || !snapshot.enabled || loading || running}>
          {running ? <LoaderCircle className="spin" size={15} /> : <RefreshCw size={15} />}立即检查
        </button>
      </div>
      </div>
      {!available && <p className="dialog-error" role="status">当前服务暂不支持 {targetLabel} 巡检。</p>}
      {alerts.map(message => <p className="dialog-error" role="alert" key={message}><CircleAlert size={16} />{message}</p>)}
      {notices.map(message => <p className="dialog-hint" role="status" key={message}>{message}</p>)}
      <div role="tabpanel" id="pool-monitor-config-panel" aria-labelledby="pool-monitor-config-tab" hidden={monitorTab !== "config"}>
      <div className="monitor-workspace">
        <form className="section-card monitor-policy" onSubmit={policy.save} aria-labelledby="monitor-policy-title">
          <div className="card-header">
            <div><h2 id="monitor-policy-title"><SlidersHorizontal size={16} aria-hidden="true" />巡检配置</h2><p>选择巡检对象，统一设置异常账号的自动恢复规则。</p></div>
            <span className={`monitor-save-state ${policy.dirty || policy.repairRequired ? "unsaved" : ""}`}>{policy.saving ? "正在保存" : policy.uncertain ? "结果待核实" : policy.conflict ? "配置已变更" : policy.dirty ? "未保存" : policy.repairRequired ? "待重新保存" : policy.loaded ? "已保存" : "待读取"}</span>
          </div>
          <div className="card-body monitor-policy-body">
            <div className="monitor-destination">
              <label className="settings-field monitor-destination-field">
                <span>巡检对象</span>
                <select name="pool-monitor-target" aria-label="巡检对象" aria-describedby="monitor-target-help" value={target}
                  disabled={!policy.loaded || policyBusy || checking || policy.uncertain || policy.conflict}
                  onChange={event => onTargetChange?.(event.target.value)}>
                  <option value="sub2api" disabled={targetAvailability.sub2api === false}>Sub2API</option>
                  <option value="cpa" disabled={targetAvailability.cpa === false}>CPA</option>
                </select>
              </label>
              <div className="monitor-destination-connection">
                <span className={targetConfigured ? "configured" : ""}><Globe2 size={13} aria-hidden="true" />{targetConfigured ? "连接已配置" : "尚未配置连接"}</span>
                <button type="button" className="text-action" onClick={onConfigure} disabled={policy.saving}><Settings2 size={13} />服务配置<ChevronRight size={13} /></button>
              </div>
              <p className="monitor-field-help" id="monitor-target-help">{targetChanged ? `保存后切换至 ${targetLabel}，下方规则保持不变。` : "仅巡检所选服务，重登成功后回传至该服务。"}</p>
            </div>
            <label className="monitor-auto-toggle">
              <span className="monitor-toggle-copy"><strong>自动巡检与重登</strong><small>定时检查异常账号，重登成功后同步到 {targetLabel}。</small></span>
              <input type="checkbox" role="switch" aria-label="自动巡检" name="pool-monitor-enabled" checked={Boolean(draft.enabled)} disabled={policyDisabled || !targetConfigured && !draft.enabled}
                onChange={event => policy.setDraft?.(current => ({ ...current, enabled: event.target.checked }))} />
              <span className="monitor-switch" aria-hidden="true" />
            </label>
            <div className="monitor-policy-timing">
                <label className="settings-field"><span>巡检间隔</span><span className="monitor-number-input">
                  <input type="number" name="pool-monitor-interval" aria-label="巡检间隔（分钟）" min="1" max="1440" step="1" value={draft.intervalMinutes ?? "5"} disabled={policyDisabled}
                    onChange={event => policy.setDraft?.(current => ({ ...current, intervalMinutes: event.target.value }))} /><span>分钟</span>
                </span></label>
                <label className="settings-field"><span>重登冷却</span><span className="monitor-number-input">
                  <input type="number" name="pool-monitor-cooldown" aria-label="重登冷却（分钟）" min="0" max="1440" step="1" value={draft.cooldownMinutes ?? "5"} disabled={policyDisabled}
                    onChange={event => policy.setDraft?.(current => ({ ...current, cooldownMinutes: event.target.value }))} /><span>分钟</span>
                </span></label>
            </div>
            <p className="monitor-field-help">巡检间隔为 1–1440 分钟；重登冷却为 0–1440 分钟，0 表示不限制同一账号再次自动重登的冷却时间。</p>
            <fieldset className="monitor-plan-picker" disabled={policyDisabled} aria-labelledby="monitor-plan-title" aria-describedby="monitor-plan-help">
              <legend><span id="monitor-plan-title">自动重登排除套餐</span></legend>
              <div className="monitor-plan-toolbar"><span>{excludedPlans.length ? `已排除 ${excludedPlans.length} 类套餐` : "未选择，不排除任何套餐"}</span><button type="button" disabled={policyDisabled || !excludedPlans.length} onClick={() => policy.setDraft?.(current => ({ ...current, excludedPlanTypes: [] }))}>清空</button></div>
              <div className="monitor-plan-options">
                {ACCOUNT_PLAN_OPTIONS.map(plan => <label className="monitor-plan-option" key={plan.value}>
                  <input type="checkbox" aria-label={plan.label} checked={excludedPlans.includes(plan.value)}
                    onChange={event => policy.setDraft?.(current => ({ ...current, excludedPlanTypes: event.target.checked ? [...(current.excludedPlanTypes || []), plan.value] : (current.excludedPlanTypes || []).filter(value => value !== plan.value) }))} />
                  <PlanOptionLabel value={plan.value} /><span className="monitor-plan-check" aria-hidden="true"><Check size={11} /></span>
                </label>)}
              </div>
              <p className="monitor-field-help" id="monitor-plan-help">本地、远端或新授权中任一已知套餐命中即跳过。未知不等于 Free；不限制手动重登或手动上传。</p>
            </fieldset>
          </div>
          <div className="monitor-policy-footer">
            <span>{targetChanged ? `保存后切换至 ${targetLabel}` : policy.repairRequired ? "核对后重新保存以恢复巡检" : policy.dirty ? "保存后生效" : "切换目标保留全部规则"}</span>
            <div>
              <button type="button" className="secondary-button" disabled={policyBusy || checking || !available} onClick={policy.reload}><RefreshCw size={14} />{policy.loading ? "正在读取" : policy.uncertain || policy.conflict ? "读取并核实" : policy.dirty ? "放弃更改" : "读取规则"}</button>
              <button type="submit" className="primary-button" disabled={policyDisabled || !targetConfigured && draft.enabled || !policy.dirty && !policy.repairRequired}>{policy.saving ? <LoaderCircle size={14} className="spin" /> : <Check size={14} />}保存巡检规则</button>
            </div>
          </div>
        </form>
          <section className="monitor-scope-note">
            <span><ShieldCheck size={16} aria-hidden="true" />处理范围</span>
            <p>{isCPA ? "仅处理本工具上传、与本地账号身份匹配的 Codex 授权文件。" : snapshot.groupIds?.length ? `检查已配置的 ${snapshot.groupIds.length} 个号池中的 OpenAI 账号。` : "未限定号池时，检查全部 OpenAI 账号。"}</p>
            <p>仅自动处理上次完整登录无需人工输入的账号。新授权命中排除或上传待核实时，保留授权并暂停回传。</p>
            <p>巡检排队不代表重登或上传完成，执行结果请在账号列表查看。</p>
          </section>
      </div>
      </div>
      <div role="tabpanel" id="pool-monitor-records-panel" aria-labelledby="pool-monitor-records-tab" hidden={monitorTab !== "records"}>
      <section className="section-card monitor-records-card" aria-labelledby="monitor-records-title">
        <div className="card-header monitor-records-header">
          <div className="monitor-records-intro"><h2 id="monitor-records-title"><History size={16} aria-hidden="true" />巡检与查询记录</h2><p>查看异常账号处理结果与账号信息查询状态。</p></div>
          <aside className="monitor-live-summary" aria-label="当前巡检状态">
            <div className="monitor-live-heading">
              <span className="monitor-live-label">监控状态</span><span className="monitor-active-target">{savedTargetLabel}</span>
              <span className={`monitor-live-state ${statusWarning ? "warning" : monitorEnabled ? "enabled" : ""}`}><StatusIcon size={14} className={running ? "spin" : ""} aria-hidden="true" /><strong>{statusLabel}</strong></span>
            </div>
            <dl className="monitor-live-times">
              <div><dt><History size={12} aria-hidden="true" />上次检查</dt><dd>{snapshot.lastCheckAt ? formatDateTime(snapshot.lastCheckAt) : "尚未执行"}</dd></div>
              <div><dt><Clock3 size={12} aria-hidden="true" />下次检查</dt><dd>{snapshot.nextCheckAt ? formatDateTime(snapshot.nextCheckAt) : monitorEnabled ? "等待调度" : "未安排"}</dd></div>
            </dl>
            {(snapshot.configurationError || snapshot.uncertain) && <p className="monitor-live-warning">自动巡检已暂停，请按提示核实配置。</p>}
          </aside>
        </div>
        <section className="monitor-record-section" aria-labelledby="monitor-result-title">
          <div className="monitor-record-heading"><h3 id="monitor-result-title">{presentation.resultTitle}</h3><span className="monitor-result-caption">{savedTargetLabel} · 最近一次检查</span></div>
          {(result || presentation.error) && <MonitorResultNotice presentation={presentation.resultPresentation} previous={Boolean(running)} />}
          {result ? <div className={`monitor-metrics${isCPA ? " cpa-monitor-metrics" : ""}`} aria-label="最近一次巡检统计">
            {metrics.map(([key, label]) => <div key={key}><strong>{Number(result[key] || 0)}</strong><span>{label}</span></div>)}
          </div> : !presentation.error && <div className="monitor-empty-result"><History size={23} aria-hidden="true" /><div><h3>{waitingForState ? "正在读取巡检记录" : "暂无巡检记录"}</h3><p>{waitingForState ? "正在获取当前目标的监控状态。" : "启用巡检后，检查结果和处理统计将显示在这里。"}</p></div></div>}
        </section>
        {queryOverview}
      </section>
      </div>
    </section>
  );
}
