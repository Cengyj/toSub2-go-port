import React, { useEffect, useRef, useState } from "react";
import {
  Activity, Check, CheckCircle2, ChevronLeft, ChevronRight, CircleAlert, Clock3,
  Globe2, HardDrive, History, Hourglass, Layers3, LayoutGrid, LoaderCircle,
  MailCheck, Menu, Monitor, Moon, Network, Palette, Pause, PhoneIncoming,
  RefreshCw, Send, Settings2, ShieldCheck, SlidersHorizontal, Sun, Trash2, UsersRound,
} from "lucide-react";
import {
  formatDateTime, formatMailRequestSummary,
  normalizeExcludedPlanTypes,
  readLocalTextSetting, writeLocalTextSetting, THEME_STORAGE_KEY,
} from "../lib/console.js";
import { PageHeader, StatCard } from "./ui.jsx";
import { PlanBadge } from "./PlanBadge.jsx";
import { describeMonitor } from "../lib/jobPresentation.js";
import { MonitorResultNotice } from "./MonitorResultNotice.jsx";

const themes = [
  { id: "light", label: "暖灰", icon: Sun },
  { id: "white", label: "纯白", icon: LayoutGrid },
  { id: "dark", label: "深色", icon: Moon },
  { id: "auto", label: "跟随系统", icon: Monitor },
];
const pages = [
  { id: "accounts", label: "账号管理", icon: UsersRound },
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

export function AppShell({ page, onNavigate, theme, onThemeChange, connected, total, children }) {
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
          {pages.map(({ id, label, icon: Icon }) => (
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
            <strong>{pages.find((item) => item.id === page)?.label}</strong>
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
        <main ref={mainContent} className="workspace" id="main-content" tabIndex={-1}>{children}</main>
      </div>
    </div>
  );
}

export function AccountsOverview({ stats, total }) {
  return (
    <div className="stats-grid" aria-label="任务统计">
      <StatCard icon={UsersRound} label="全部账号" value={total} caption="当前工作空间" />
      <StatCard icon={Activity} label="进行中" value={stats.active || 0} caption="正在执行账号操作" tone="active" />
      <StatCard icon={Hourglass} label="排队中" value={stats.queued || 0} caption="等待执行账号操作" />
      <StatCard icon={CheckCircle2} label="授权可用" value={stats.available ?? stats.completed ?? 0} caption="已保存可下载的授权" tone="success" />
    </div>
  );
}

const sections = [
  { id: "overview", label: "配置概览", icon: LayoutGrid },
  { id: "sms", label: "接码平台", icon: PhoneIncoming },
  { id: "mail", label: "邮件 API", icon: MailCheck },
  { id: "sub2api", label: "Sub2API", icon: Send },
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
  section, onSectionChange, dirty, saving, sms, mail, sub2api,
  proxy, proxyDraft, onProxyDraftChange, onProxySave, theme, onThemeChange,
  onClear, clearDisabled, clearError, children,
}) {
  const [proxyError, setProxyError] = useState("");
  const [savedNotice, setSavedNotice] = useState(false);

  function saveProxy(event) {
    event.preventDefault();
    const value = proxyDraft.trim();
    try {
      if (value && !["http:", "https:", "socks5:", "socks5h:"].includes(new URL(value).protocol)) throw new Error();
    } catch {
      setProxyError("请输入有效的 HTTP、HTTPS、SOCKS5 或 SOCKS5H 代理地址。");
      return;
    }
    if (!onProxySave(value)) {
      setProxyError("浏览器未能保存代理配置，请检查站点存储权限后重试。现有配置未变更。");
      setSavedNotice(false);
      return;
    }
    setProxyError("");
    setSavedNotice(true);
  }

  return (
    <section className="configuration-page">
      <PageHeader title="服务配置" description="统一管理收码服务、上传目标与账号网络。">
        <span className={`configuration-state ${saving ? "saving" : dirty ? "dirty" : "synced"}`} role="status">
          {saving
            ? <LoaderCircle size={14} className="spin" aria-hidden="true" />
            : dirty
              ? <CircleAlert size={14} aria-hidden="true" />
              : <CheckCircle2 size={14} aria-hidden="true" />}
          {saving ? "正在保存…" : dirty ? "有未保存的更改" : "无待保存更改"}
        </span>
      </PageHeader>
      <nav className="settings-tabs" aria-label="配置分类">
        {sections.map(({ icon: Icon, ...item }) => (
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
              description="上传授权结果，设置目标号池与账号监控。"
              summary={sub2api.baseUrl || "尚未配置上传目标"}
              ready={Boolean(sub2api.baseUrl && sub2api.adminApiKey)}
              onOpen={() => onSectionChange("sub2api")}
            />
            <ConfigurationCard
              icon={Network}
              tone="network"
              title="代理网络"
              description="为新任务指定出口，支持 HTTP 与 SOCKS 代理。"
              summary={proxy ? "已保存账号代理" : "当前使用本地网络"}
              ready={Boolean(proxy)}
              onOpen={() => onSectionChange("network")}
            />
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
            {savedNotice && (
              <p className="provider-ready" role="status"><Check size={14} />代理配置已保存</p>
            )}
          </div>
          <div className="dialog-footer">
            <button
              type="button"
              className="cancel-button"
              disabled={proxyDraft === proxy}
              onClick={() => {
                onProxyDraftChange(proxy);
                setProxyError("");
              }}
            >
              放弃更改
            </button>
            <button type="submit" className="primary-button" disabled={proxyDraft === proxy}>
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
              <p>清除当前浏览器保存的接码平台、邮件 API、Sub2API、代理、主题和账号操作偏好。服务端任务与凭据需通过本地服务清理。</p>
            </div>
            <div className="card-body">
              <button type="button" className="secondary-button danger-outline" onClick={onClear} disabled={clearDisabled}>
                <Trash2 size={16} />清除本机网页配置
              </button>
              {clearError && <p className="dialog-error" role="alert">{clearError}</p>}
            </div>
          </section>
        </div>
      )}
    </section>
  );
}

export function MonitorPage({ status, onConfigure, onCheck, checking, available }) {
  const presentation = describeMonitor({ ...status, running: status.running || checking });
  const result = presentation.result;
  const running = status.running || checking;
  const excludedPlanTypes = normalizeExcludedPlanTypes(status.excludedPlanTypes);
  const monitorState = running ? "running" : presentation.error ? "warning" : status.enabled ? "enabled" : "disabled";
  const StatusIcon = running ? Activity : presentation.error ? CircleAlert : status.enabled ? ShieldCheck : Pause;
  return (
    <section>
      <PageHeader title="号池监控" description="检查 Sub2API 账号健康状态，跟踪异常账号的恢复进度。">
        <button className="secondary-button" type="button" onClick={onConfigure}>
          <Settings2 size={16} />监控配置
        </button>
        <button
          className="primary-button"
          type="button"
          onClick={onCheck}
          disabled={!available || !status.enabled || checking || status.running}
        >
          {checking || status.running ? <LoaderCircle className="spin" size={16} /> : <RefreshCw size={16} />}
          立即检查
        </button>
      </PageHeader>
      <div className="monitor-grid">
        <section className="section-card monitor-summary">
          <div className="card-header">
            <h2>监控状态</h2>
            <span className={`config-card-status ${presentation.status.tone === "success" ? "ready" : "incomplete"}`}>
              {running ? <LoaderCircle size={12} className="spin" aria-hidden="true" /> : presentation.error ? <CircleAlert size={12} aria-hidden="true" /> : status.enabled ? <Check size={12} aria-hidden="true" /> : <Pause size={12} aria-hidden="true" />}
              {presentation.status.label}
            </span>
          </div>
          <div className="card-body">
            <div className={`monitor-status ${monitorState}`}>
              <div className="monitor-status-icon"><StatusIcon size={26} aria-hidden="true" /></div>
              <h3>{running ? "正在检查账号状态" : presentation.error ? "上次巡检未完成" : status.enabled ? "自动巡检已开启" : "开启自动巡检"}</h3>
              <p>
                {status.enabled
                  ? `每 ${status.intervalMinutes || 5} 分钟检查一次目标号池。`
                  : "配置 Sub2API 并启用监控后，在这里查看巡检和恢复结果。"}
              </p>
              {excludedPlanTypes.length ? (
                <div className="monitor-plan-rule">
                  <span>自动重登排除套餐</span>
                  <div className="monitor-plan-badges" role="group" aria-label="自动重登排除套餐">
                    {excludedPlanTypes.map((plan) => <PlanBadge key={plan} value={plan} />)}
                  </div>
                </div>
              ) : <p className="monitor-plan-rule">自动重登不排除任何套餐</p>}
              {!status.enabled && (
                <button type="button" className="secondary-button" onClick={onConfigure}>
                  前往配置<ChevronRight size={15} />
                </button>
              )}
            </div>
          </div>
        </section>
        <section className="section-card">
          <div className="card-header"><h2>巡检记录</h2></div>
          <dl className="card-body monitor-times">
            <div>
              <dt className="monitor-time-label"><History size={15} aria-hidden="true" /><span>上次检查</span></dt>
              <dd>{status.lastCheckAt ? formatDateTime(status.lastCheckAt) : "尚未执行"}</dd>
            </div>
            <div>
              <dt className="monitor-time-label"><Clock3 size={15} aria-hidden="true" /><span>下次检查</span></dt>
              <dd>{status.nextCheckAt ? formatDateTime(status.nextCheckAt) : "等待启用"}</dd>
            </div>
            <div>
              <dt className="monitor-time-label"><RefreshCw size={15} aria-hidden="true" /><span>执行间隔</span></dt>
              <dd>{status.intervalMinutes || 5} 分钟</dd>
            </div>
            <div>
              <dt className="monitor-time-label"><Globe2 size={15} aria-hidden="true" /><span>服务连接</span></dt>
              <dd>{status.configured ? "已配置" : "待配置"}</dd>
            </div>
          </dl>
        </section>
      </div>
      <section className="section-card monitor-result">
        <div className="card-header"><h2>{presentation.resultTitle}</h2></div>
        <div className="card-body">
          {(result || presentation.error) && <MonitorResultNotice presentation={presentation.resultPresentation} previous={Boolean(running)} />}
          {result ? (
            <>
              <div className="monitor-metrics" aria-label="最近一次巡检统计">
                <div><strong>{Number(result.checked || 0)}</strong><span>已检查异常记录</span></div>
                <div><strong>{Number(result.started || 0)}</strong><span>已排队自动重登</span></div>
                <div><strong>{Number(result.updated || 0)}</strong><span>已更新账号</span></div>
                <div><strong>{Number(result.excludedPlan || 0)}</strong><span>按套餐跳过</span></div>
              </div>
            </>
          ) : !presentation.error && (
            <div className="empty-panel">
              <div className="empty-state-icon"><History size={25} aria-hidden="true" /></div>
              <h3>暂无巡检记录</h3>
              <p>执行检查后，结果会显示在这里。</p>
            </div>
          )}
        </div>
      </section>
    </section>
  );
}
