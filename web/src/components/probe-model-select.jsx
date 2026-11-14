import React, { useEffect, useRef, useState } from "react";
import { Check, ChevronDown, LoaderCircle, RefreshCw, Filter } from "lucide-react";
import { QUALITY_REASONING_EFFORTS } from "../lib/accountQuality.js";

export const PROBE_EFFORTS = QUALITY_REASONING_EFFORTS;
export const effortLabel = value => ({ none: "关闭", minimal: "最低", low: "低", medium: "中", high: "高", xhigh: "极高", max: "Max", ultra: "Ultra" })[value] || value;
export function probeEfforts(model) {
  const supported = model?.reasoningEfforts.filter(value => PROBE_EFFORTS.includes(value)) || [];
  return supported.length ? supported : PROBE_EFFORTS;
}

const PROBE_MODEL_ORDER = [
  "gpt-6-astra", "gpt-6.1-sol", "gpt-6.1-terra", "gpt-6-sol", "gpt-6-luna",
  "gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna",
];
const PROBE_MODEL_RANK = new Map(PROBE_MODEL_ORDER.map((id, index) => [id, index]));

export function probeModelChoices(catalog) {
  const seen = new Set(), choices = [];
  for (const [models, suggested] of [[catalog.models || [], false], [catalog.suggestedModels || [], true]]) {
    for (const model of models) {
      if (seen.has(model.id)) continue;
      seen.add(model.id);
      choices.push({ ...model, suggested });
    }
  }
  return choices.sort((a, b) => (PROBE_MODEL_RANK.get(a.id.toLowerCase()) ?? PROBE_MODEL_ORDER.length) - (PROBE_MODEL_RANK.get(b.id.toLowerCase()) ?? PROBE_MODEL_ORDER.length));
}

const emptyCatalog = (accountId, loading = false) => ({ accountId, models: [], suggestedModels: [], loading, loaded: false, error: "" });

export function useProbeModels(quality, accountId) {
  const [state, setState] = useState(() => emptyCatalog(""));
  const [revision, setRevision] = useState(0), api = useRef(quality.modelsAPI);
  const authority = useRef(null);
  if (!authority.current || authority.current.accountId !== accountId || authority.current.session !== quality.manualSession) {
    authority.current = { accountId, session: quality.manualSession };
  }
  const scope = authority.current;
  api.current = quality.modelsAPI;
  useEffect(() => {
    if (!accountId) { setState({ ...emptyCatalog(""), scope }); return; }
    let current = true;
    const readModels = api.current;
    const controller = new AbortController(), timer = window.setTimeout(() => controller.abort(), 20000);
    setState({ ...emptyCatalog(accountId, true), scope });
    Promise.resolve().then(() => current && readModels(accountId, { signal: controller.signal, cache: "no-store" })).then(data => {
      if (!current || authority.current !== scope) return;
      const suggestedModels = data?.suggestedModels === undefined ? [] : data.suggestedModels;
      const validModels = models => Array.isArray(models) && models.every(model => model && typeof model.id === "string" && model.id.trim() && Array.isArray(model.reasoningEfforts));
      if (!data || !validModels(data.models) || !validModels(suggestedModels)) throw new Error("模型列表不完整，请重试。");
      setState({ accountId, scope, models: data.models, suggestedModels, loading: false, loaded: true, error: "" });
    }).catch(error => { if (current && authority.current === scope) setState({ ...emptyCatalog(accountId), scope, error: error.name === "AbortError" ? "读取模型超时，请重试。" : error.message || "暂时无法读取模型。" }); }).finally(() => window.clearTimeout(timer));
    return () => { current = false; controller.abort(); window.clearTimeout(timer); };
  }, [scope, revision]);
  const current = state.scope === scope ? state : emptyCatalog(accountId, Boolean(accountId));
  return { ...current, reload: () => { if (authority.current === scope) setRevision(value => value + 1); } };
}

export function ProbeModelSelect({ id, value, onChange, catalog, disabled = false, onManualChange }) {
  const [open, setOpen] = useState(false), [manual, setManual] = useState(false), [query, setQuery] = useState(""), [active, setActive] = useState(0);
  const [menuLayout, setMenuLayout] = useState({ above: false, height: 195 });
  const search = useRef(null), trigger = useRef(null), list = useRef(null);
  const choices = probeModelChoices(catalog);
  const models = choices.filter(model => `${model.id} ${model.displayName || ""}`.toLowerCase().includes(query.trim().toLowerCase()));
  const selected = choices.find(model => model.id === value);
  useEffect(() => { if (open) search.current?.focus(); }, [open]);
  useEffect(() => {
    if (!open || !trigger.current) return;
    const place = () => {
      const rect = trigger.current.getBoundingClientRect(), viewport = window.visualViewport;
      const top = viewport?.offsetTop || 0, bottom = top + (viewport?.height || window.innerHeight);
      const below = bottom - rect.bottom - 12, above = rect.top - top - 12, flip = below < 170 && above > below;
      setMenuLayout({ above: flip, height: Math.max(60, Math.min(195, (flip ? above : below) - 52)) });
    };
    place(); window.addEventListener("resize", place); window.visualViewport?.addEventListener("resize", place);
    return () => { window.removeEventListener("resize", place); window.visualViewport?.removeEventListener("resize", place); };
  }, [open]);
  useEffect(() => { if (open) list.current?.children[active]?.scrollIntoView?.({ block: "nearest" }); }, [open, active, query]);
  useEffect(() => { setOpen(false); setQuery(""); setActive(0); }, [catalog.accountId, disabled]);
  function choose(model) { onChange(model.id); setOpen(false); setQuery(""); trigger.current?.focus(); }
  function keys(event) {
    if (event.key === "Escape") { event.preventDefault(); event.stopPropagation(); setOpen(false); trigger.current?.focus(); }
    else if (event.key === "ArrowDown" || event.key === "ArrowUp") { event.preventDefault(); setActive(index => Math.max(0, Math.min(models.length - 1, index + (event.key === "ArrowDown" ? 1 : -1)))); }
    else if (event.key === "Enter") { event.preventDefault(); if (models[active]) choose(models[active]); }
  }
  return <div className="probe-model-field" onBlur={event => { if (!event.currentTarget.contains(event.relatedTarget)) setOpen(false); }}>
    <div className="probe-field-heading"><label htmlFor={id}>模型</label><button type="button" className="text-action" disabled={disabled} onClick={() => { setManual(!manual); setOpen(false); onManualChange?.(!manual); }}>{manual ? "从列表选择" : "输入模型"}</button></div>
    {manual ? <input id={id} className="probe-input" aria-label="自定义模型 ID" value={value} maxLength={128} disabled={disabled} onChange={event => onChange(event.target.value)} placeholder="输入模型 ID" autoComplete="off" spellCheck="false" /> : <div className="probe-model-picker">
      <button ref={trigger} id={id} type="button" className="probe-model-trigger" aria-haspopup="listbox" aria-expanded={open} aria-controls={`${id}-list`} disabled={disabled} onClick={() => { setOpen(!open); setQuery(""); setActive(0); }} onKeyDown={event => { if (event.key === "ArrowDown" || event.key === "ArrowUp") { event.preventDefault(); setOpen(true); } }}>
        <span className={value ? "" : "is-placeholder"} title={value}>{selected?.displayName || value || (catalog.loading ? "正在读取模型…" : "选择模型")}</span>{catalog.loading ? <LoaderCircle size={14} className="spin" /> : <ChevronDown size={14} />}
      </button>
      {open && <div className="probe-model-menu" style={menuLayout.above ? { top: "auto", bottom: "calc(100% + 5px)" } : undefined}>
        <div className="probe-model-search"><Filter size={14} aria-hidden="true" /><input ref={search} value={query} onChange={event => { setQuery(event.target.value); setActive(0); }} onKeyDown={keys} placeholder="搜索模型名称或 ID" aria-label="搜索模型" role="combobox" aria-expanded="true" aria-autocomplete="list" aria-controls={`${id}-list`} aria-activedescendant={models[active] ? `${id}-option-${active}` : undefined} /></div>
        <div ref={list} id={`${id}-list`} role="listbox" aria-label="可选模型" className="probe-model-options" style={{ maxHeight: menuLayout.height }}>
          {models.map((model, index) => <button id={`${id}-option-${index}`} key={model.id} type="button" role="option" aria-selected={value === model.id} tabIndex={-1} className={index === active ? "is-focused" : ""} onMouseDown={event => event.preventDefault()} onMouseEnter={() => setActive(index)} onClick={() => choose(model)}><span><strong>{model.displayName || model.id}</strong>{model.displayName && model.displayName !== model.id && <small>{model.id}</small>}</span><span className="probe-model-option-meta">{model.suggested && <small className="probe-model-origin">常用</small>}{value === model.id && <Check size={14} />}</span></button>)}
          {!models.length && <p>{catalog.loading ? "正在读取模型…" : catalog.error ? "读取失败，请重试或输入模型 ID" : query ? "没有匹配的模型" : "此账号未提供模型列表"}</p>}
        </div>
      </div>}
    </div>}
    <div className={`probe-model-hint${catalog.error ? " has-error" : ""}`}>
      <span>{manual ? "使用账号具备调用权限的模型 ID" : catalog.error || (catalog.loading ? "正在读取账号模型列表" : selected?.suggested ? "常用模型，账号目录未列出；调用结果以实际检测为准" : catalog.loaded && !choices.length ? "无可选模型，可切换账号或输入模型" : value && !selected ? "当前模型不在此账号的目录中" : choices.some(model => model.suggested) ? "从账号目录或常用模型选择" : "从当前账号的模型目录选择")}</span>
      {catalog.accountId && !catalog.loading && <button type="button" className="text-action" disabled={disabled} aria-label="重新读取模型列表" title="重新读取模型列表" onClick={catalog.reload}><RefreshCw size={12} /></button>}
    </div>
  </div>;
}
