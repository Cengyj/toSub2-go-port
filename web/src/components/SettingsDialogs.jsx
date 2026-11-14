import React from "react";
import { Check, CircleAlert, ExternalLink, KeyRound, LoaderCircle, RefreshCw, RotateCcw, Settings2, X } from "lucide-react";
import { DialogFrame } from "./ui.jsx";
import { SMS_PROVIDER_EXTERNAL_LINKS, withSmsProviderDefaults, formatSmsCountryName, formatSmsPriceOption, formatSub2ApiProxy } from "../lib/console.js";

export function SmsSettingsDialog({
  inline = false,
  setSmsSettingsOpen,
  saveSmsSettings,
  smsProviderDefinitions,
  draftSmsProvider,
  setSmsSettingsDraft,
  setSmsSettingsError,
  smsNumberOptions,
  smsOptionsLoading,
  updateSmsProviderConfig,
  loadSmsNumberOptions,
  smsSettingsError,
}) {
  return (
    <DialogFrame inline={inline} onClose={() => setSmsSettingsOpen(false)} busy={false}>
      <form className="batch-dialog sms-settings-dialog" onSubmit={saveSmsSettings} role={inline ? undefined : "dialog"} aria-modal={inline ? undefined : true} aria-labelledby="sms-settings-title">
        <div className="dialog-header">
          <div>
            <h2 id="sms-settings-title">接码平台配置</h2>
            <span>配置保存在当前浏览器</span>
          </div>
          <button type="button" className="icon-button" onClick={() => setSmsSettingsOpen(false)} title="关闭">
            <X size={18} />
          </button>
        </div>

        <div className="provider-tabs" role="tablist" aria-label="选择接码平台">
          {smsProviderDefinitions.map((provider) => (
            <button
              key={provider.id}
              type="button"
              role="tab"
              aria-selected={draftSmsProvider.id === provider.id}
              className={draftSmsProvider.id === provider.id ? "active" : ""}
              onClick={() => {
                setSmsSettingsDraft((current) => withSmsProviderDefaults(smsProviderDefinitions, {
                  ...current,
                  selectedProviderId: provider.id,
                }));
                setSmsSettingsError("");
              }}
            >
              {provider.name}
            </button>
          ))}
        </div>

        {draftSmsProvider.definition && (
          <div className="provider-config-section">
            <div className="provider-description-row">
              <div className="provider-description">{draftSmsProvider.definition.description}</div>
              {SMS_PROVIDER_EXTERNAL_LINKS[draftSmsProvider.id] && (
                <a
                  className="provider-external-link"
                  href={SMS_PROVIDER_EXTERNAL_LINKS[draftSmsProvider.id].href}
                  target="_blank"
                  rel="noopener noreferrer"
                >
                  <ExternalLink size={13} aria-hidden="true" />
                  {SMS_PROVIDER_EXTERNAL_LINKS[draftSmsProvider.id].label}
                </a>
              )}
            </div>
            <div className="provider-config-grid">
              {draftSmsProvider.definition.fields.filter((field) => field.type !== "hidden").map((field) => (
                <label key={field.key} className={`settings-field ${["price-select", "textarea"].includes(field.type) ? "wide-settings-field" : ""}`}>
                  <span>{field.label}</span>
                  {field.type === "price-select" ? (
                    <div className="price-select-row">
                      <div className="price-select-box">
                        <Settings2 size={15} />
                        <select
                          value={draftSmsProvider.config.country || ""}
                          onChange={(event) => {
                            const selected = smsNumberOptions.find((option) => option.country === event.target.value);
                            if (!selected) return;
                            updateSmsProviderConfig(draftSmsProvider.id, {
                              country: selected.country,
                              maxPrice: String(selected.price),
                              countryLabel: formatSmsCountryName(selected),
                            });
                          }}
                          disabled={!smsNumberOptions.length || smsOptionsLoading}
                          aria-label="SMSBower 国家与价格"
                        >
                          {!smsNumberOptions.length && (
                            <option value={draftSmsProvider.config.country || ""}>
                              {smsOptionsLoading ? "正在查询实时价格..." : "请先查询实时价格"}
                            </option>
                          )}
                          {smsNumberOptions.map((option) => (
                            <option key={option.country} value={option.country}>{formatSmsPriceOption(option)}</option>
                          ))}
                        </select>
                      </div>
                      <button
                        type="button"
                        className="price-refresh-button"
                        onClick={() => loadSmsNumberOptions()}
                        disabled={smsOptionsLoading || !draftSmsProvider.config.apiKey}
                      >
                        {smsOptionsLoading ? <LoaderCircle className="spin" size={15} /> : <RefreshCw size={15} />}
                        {smsOptionsLoading ? "查询中" : "查询价格"}
                      </button>
                    </div>
                  ) : field.type === "textarea" ? (
                    <textarea
                      className="settings-textarea"
                      value={draftSmsProvider.config[field.key] || ""}
                      onChange={(event) => updateSmsProviderConfig(draftSmsProvider.id, { [field.key]: event.target.value })}
                      placeholder={field.placeholder}
                      rows="8"
                      autoComplete="off"
                      spellCheck="false"
                    />
                  ) : (
                    <div>
                      {field.type === "password" ? <KeyRound size={15} /> : <Settings2 size={15} />}
                      <input
                        type={field.type || "text"}
                        value={draftSmsProvider.config[field.key] || ""}
                        onChange={(event) => updateSmsProviderConfig(draftSmsProvider.id, { [field.key]: event.target.value })}
                        placeholder={field.placeholder}
                        autoComplete="off"
                        spellCheck="false"
                      />
                    </div>
                  )}
                </label>
              ))}
            </div>
          </div>
        )}

        {smsSettingsError && <div className="dialog-error" role="alert"><CircleAlert size={15} />{smsSettingsError}</div>}
        <div className="dialog-footer">
          <button type="button" className="cancel-button" onClick={() => setSmsSettingsOpen(false)}>取消</button>
          <button type="submit" className="primary-button" disabled={!draftSmsProvider.definition}>
            <Check size={17} />保存配置
          </button>
        </div>
      </form>
    </DialogFrame>
  );
}

export function MailSettingsDialog({
  inline = false,
  setMailRequestSettingsOpen,
  mailRequestSettingsSaving,
  saveMailRequestSettings,
  mailRequestSettingsDraft,
  setMailRequestSettingsDraft,
  mailRequestSettingsError,
  token,
}) {
  return (
    <DialogFrame inline={inline} onClose={() => setMailRequestSettingsOpen(false)} busy={mailRequestSettingsSaving}>
      <form className="batch-dialog mail-request-settings-dialog" onSubmit={saveMailRequestSettings} role={inline ? undefined : "dialog"} aria-modal={inline ? undefined : true} aria-labelledby="mail-request-settings-title">
        <div className="dialog-header">
          <div>
            <h2 id="mail-request-settings-title">邮件 API 请求配置</h2>
            <span>配置保存在当前浏览器，请求内容不会写入任务日志</span>
          </div>
          <button type="button" className="icon-button" onClick={() => setMailRequestSettingsOpen(false)} disabled={mailRequestSettingsSaving} title="关闭">
            <X size={18} />
          </button>
        </div>

        <div className="provider-tabs mail-method-tabs" role="tablist" aria-label="邮件 API 请求方式">
          {["GET", "POST"].map((method) => (
            <button
              key={method}
              type="button"
              role="tab"
              aria-selected={mailRequestSettingsDraft.method === method}
              className={mailRequestSettingsDraft.method === method ? "active" : ""}
              onClick={() => setMailRequestSettingsDraft((current) => ({ ...current, method }))}
            >
              {method}
            </button>
          ))}
        </div>

        <div className="provider-config-grid mail-request-config-grid">
          {mailRequestSettingsDraft.method === "POST" && (
            <label className="settings-field wide-settings-field">
              <span className="mail-request-url-label">
                统一 POST 请求 URL
                <small>必填</small>
              </span>
              <input
                className="mail-request-url-input"
                type="url"
                value={mailRequestSettingsDraft.url}
                onChange={(event) => setMailRequestSettingsDraft((current) => ({ ...current, url: event.target.value }))}
                placeholder="https://mail.example/api/messages"
                autoComplete="off"
                spellCheck="false"
              />
              <small>每个账号自己的请求体请在批量添加账号时一并导入</small>
            </label>
          )}
          <label className="settings-field wide-settings-field">
            <span>请求头 JSON</span>
            <textarea
              className="settings-textarea"
              value={mailRequestSettingsDraft.headersText}
              onChange={(event) => setMailRequestSettingsDraft((current) => ({ ...current, headersText: event.target.value }))}
              placeholder={'{"Authorization":"Bearer ...","Referer":"https://mail.example/"}'}
              rows="7"
              autoComplete="off"
              spellCheck="false"
            />
          </label>
        </div>

        {mailRequestSettingsError && <div className="dialog-error" role="alert"><CircleAlert size={15} />{mailRequestSettingsError}</div>}
        <div className="dialog-footer">
          <button type="button" className="cancel-button" onClick={() => setMailRequestSettingsOpen(false)} disabled={mailRequestSettingsSaving}>取消</button>
          <button type="submit" className="primary-button" disabled={mailRequestSettingsSaving || !token}>
            {mailRequestSettingsSaving ? <LoaderCircle className="spin" size={17} /> : <Check size={17} />}
            保存配置
          </button>
        </div>
      </form>
    </DialogFrame>
  );
}

export function Sub2ApiSettingsDialog({
  inline = false,
  setSub2apiSettingsOpen,
  saveSub2ApiSettings,
  sub2apiSettingsDraft,
  setSub2apiSettingsDraft,
  features,
  sub2apiGroups,
  setSub2ApiGroupChecked,
  sub2apiProxies,
  sub2apiSettingsError,
  loadSub2ApiOptions,
  sub2apiGroupsLoading,
  token,
  sub2apiSettingsSaving,
  sub2apiSettingsVerificationRequired = false,
  sub2apiSettingsVerifying = false,
  verifySub2ApiSettings,
  onOpenQuality,
}) {
  return (
    <DialogFrame inline={inline} onClose={() => setSub2apiSettingsOpen(false)} busy={sub2apiSettingsSaving}>
      <form className="batch-dialog sub2api-settings-dialog" onSubmit={saveSub2ApiSettings} role={inline ? undefined : "dialog"} aria-modal={inline ? undefined : true} aria-labelledby="sub2api-settings-title">
        <div className="dialog-header">
          <div>
            <h2 id="sub2api-settings-title">Sub2API 配置</h2>
            <span>管理服务连接、推送号池与账号参数</span>
          </div>
          <button type="button" className="icon-button" onClick={() => setSub2apiSettingsOpen(false)} title="关闭"><X size={18} /></button>
        </div>
        <div className="provider-config-grid sub2api-config-grid">
          <label className="settings-field wide-settings-field">
            <span>Sub2API 后端地址</span>
            <input
              type="url"
              value={sub2apiSettingsDraft.baseUrl}
              onChange={(event) => setSub2apiSettingsDraft((current) => ({ ...current, baseUrl: event.target.value }))}
              placeholder="例如 http://127.0.0.1:8080"
              autoComplete="url"
            />
          </label>
          <label className="settings-field wide-settings-field">
            <span>管理员 API Key</span>
            <input
              type="password"
              value={sub2apiSettingsDraft.adminApiKey}
              onChange={(event) => setSub2apiSettingsDraft((current) => ({ ...current, adminApiKey: event.target.value }))}
              placeholder="输入 sub2api 管理员 API Key"
              autoComplete="off"
            />
          </label>
          <fieldset className="settings-field wide-settings-field sub2api-group-field">
            <legend>目标号池（可多选）</legend>
            <section className="sub2api-group-picker" aria-label="目标号池">
              {sub2apiGroups.length ? sub2apiGroups.map((group) => {
                const groupId = String(group.id);
                return (
                  <label key={group.id} className="sub2api-group-option">
                    <input
                      type="checkbox"
                      checked={sub2apiSettingsDraft.groupIds.includes(groupId)}
                      onChange={(event) => setSub2ApiGroupChecked(groupId, event.target.checked)}
                    />
                    <span>{group.name}</span>
                    <small>ID: {group.id}</small>
                  </label>
                );
              }) : <div className="sub2api-group-empty">暂无可选号池</div>}
            </section>
            <section className="sub2api-group-selection" aria-label="号池选择操作">
              <span>已选 {sub2apiSettingsDraft.groupIds.length} 个</span>
              <button
                type="button"
                onClick={() => setSub2apiSettingsDraft((current) => ({ ...current, groupIds: sub2apiGroups.map((group) => String(group.id)) }))}
                disabled={!sub2apiGroups.length || sub2apiGroups.every((group) => sub2apiSettingsDraft.groupIds.includes(String(group.id)))}
              >
                全选
              </button>
              <button
                type="button"
                onClick={() => setSub2apiSettingsDraft((current) => ({ ...current, groupIds: [] }))}
                disabled={!sub2apiSettingsDraft.groupIds.length}
              >
                清空
              </button>
            </section>
          </fieldset>
          <label className="settings-field wide-settings-field">
            <span>代理 IP</span>
            <select
              value={sub2apiSettingsDraft.proxyId}
              onChange={(event) => setSub2apiSettingsDraft((current) => ({ ...current, proxyId: event.target.value }))}
            >
              <option value="">使用账号原配置</option>
              {sub2apiProxies.map((proxy) => <option key={proxy.id} value={String(proxy.id)}>{formatSub2ApiProxy(proxy)}</option>)}
            </select>
          </label>
          <label className="settings-field wide-settings-field">
            <span>Codex 指纹收敛</span>
            <select
              value={sub2apiSettingsDraft.codexFingerprintMode}
              onChange={(event) => setSub2apiSettingsDraft((current) => ({ ...current, codexFingerprintMode: event.target.value }))}
            >
              <option value="off">关闭（透传）</option>
              <option value="device">仅设备</option>
              <option value="session">设备+会话（推荐）</option>
              <option value="full">完全收敛</option>
            </select>
          </label>
          <label className="settings-field wide-settings-field">
            <span>OpenAI 请求时区</span>
            <input
              type="text"
              value={sub2apiSettingsDraft.openaiRequestTimezone}
              onChange={(event) => setSub2apiSettingsDraft((current) => ({ ...current, openaiRequestTimezone: event.target.value }))}
              placeholder="America/Los_Angeles"
              list="sub2api-request-timezones"
              autoComplete="off"
              spellCheck="false"
            />
            <datalist id="sub2api-request-timezones">
              <option value="America/Los_Angeles">洛杉矶</option>
              <option value="America/New_York">纽约</option>
              <option value="Asia/Shanghai">上海</option>
              <option value="Asia/Tokyo">东京</option>
              <option value="Europe/London">伦敦</option>
              <option value="UTC">UTC</option>
            </datalist>
            <small className="settings-field-help">上传和自动重登后同步到账号。支持 IANA 时区名称，留空使用 America/Los_Angeles。</small>
          </label>
          <label className="settings-field">
            <span>并发数</span>
            <input
              type="number"
              min="0"
              max="10000"
              step="1"
              value={sub2apiSettingsDraft.concurrency}
              onChange={(event) => setSub2apiSettingsDraft((current) => ({ ...current, concurrency: event.target.value }))}
              placeholder="留空使用账号原值"
            />
          </label>
          <label className="settings-field">
            <span>负载因子</span>
            <input
              type="number"
              min="0"
              max="10000"
              step="1"
              value={sub2apiSettingsDraft.loadFactor}
              onChange={(event) => setSub2apiSettingsDraft((current) => ({ ...current, loadFactor: event.target.value }))}
              placeholder="留空使用账号原值"
            />
          </label>
          <label className="settings-field">
            <span>优先级</span>
            <input
              type="number"
              min="0"
              max="10000"
              step="1"
              value={sub2apiSettingsDraft.priority}
              onChange={(event) => setSub2apiSettingsDraft((current) => ({ ...current, priority: event.target.value }))}
              placeholder="留空使用账号原值"
            />
          </label>
          <label className="settings-field wide-settings-field">
            <span>允许使用的模型</span>
            <textarea
              className="sub2api-model-textarea"
              value={sub2apiSettingsDraft.modelWhitelist}
              onChange={(event) => setSub2apiSettingsDraft((current) => ({ ...current, modelWhitelist: event.target.value }))}
              placeholder={"每行一个模型，也支持逗号分隔，例如：\ngpt-5\ngpt-5-mini\ngpt-4.1"}
              rows="5"
              spellCheck="false"
            />
          </label>
        </div>
        <div className="dialog-hint">已有账号更新授权和请求时区并保留其他配置；新账号使用这里的推送参数，未选择号池时使用默认号池。</div>
        {features.accountQuality === true && <p className="dialog-hint">上传前可选执行推理探针；低于阈值或检测失败时是否上传，由服务端统一策略决定。<button type="button" className="text-action" onClick={onOpenQuality}>管理推理探针策略</button></p>}
        {sub2apiSettingsError && <div className="dialog-error" role="alert"><CircleAlert size={15} />{sub2apiSettingsError}</div>}
        <div className="dialog-actions sub2api-dialog-actions">
          {sub2apiSettingsVerificationRequired && <button type="button" className="secondary-button" onClick={verifySub2ApiSettings} disabled={sub2apiSettingsVerifying || sub2apiSettingsSaving}>
            {sub2apiSettingsVerifying ? <LoaderCircle className="spin" size={16} /> : <RefreshCw size={16} />}核对保存结果
          </button>}
          <button type="button" className="secondary-button" onClick={() => loadSub2ApiOptions()} disabled={sub2apiGroupsLoading || !token}>
            {sub2apiGroupsLoading ? <LoaderCircle className="spin" size={16} /> : <RotateCcw size={16} />}读取配置
          </button>
          <span className="dialog-actions-spacer" />
          <button type="button" className="cancel-button" onClick={() => setSub2apiSettingsOpen(false)} disabled={sub2apiSettingsSaving}>取消</button>
          <button type="submit" className="primary-button" disabled={sub2apiSettingsSaving || sub2apiSettingsVerificationRequired || sub2apiSettingsVerifying}>
            {sub2apiSettingsSaving ? <LoaderCircle className="spin" size={16} /> : <Check size={16} />}保存配置
          </button>
        </div>
      </form>
    </DialogFrame>
  );
}
