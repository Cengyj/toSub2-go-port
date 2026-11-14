import React from "react";
import { UserRound, Zap, Plus, Crown, Gem, BriefcaseBusiness, UsersRound, Building2, GraduationCap, CircleHelp, RefreshCw } from "lucide-react";
import { accountPlanLabel, normalizeAccountPlan } from "../lib/console.js";

const planIcons = { free: UserRound, go: Zap, plus: Plus, pro: Crown, prolite: Gem, business_premium: BriefcaseBusiness, team: UsersRound, enterprise: Building2, edu: GraduationCap, unknown: CircleHelp };
const planTier = (plan) => plan === "pro" ? "elite" : ["prolite", "business_premium"].includes(plan) ? "premium" : "plain";

export function PlanOptionLabel({ value }) {
  const plan = normalizeAccountPlan(value);
  const Icon = planIcons[plan];
  return (
    <span className="plan-option-content" data-tier={planTier(plan)} data-plan={plan}>
      <Icon size={15} strokeWidth={1.8} aria-hidden="true" focusable="false" />
      <PlanBadge value={plan} />
    </span>
  );
}

export function PlanBadge({ value, short = false, onRefresh, refreshDisabled = false, refreshing = false, accountLabel = "", refreshHint = "" }) {
  const plan = normalizeAccountPlan(value);
  const label = plan === "unknown" ? "Unknown" : accountPlanLabel(plan);
  const tier = planTier(plan);
  const interactive = typeof onRefresh === "function";
  const disabled = refreshDisabled || refreshing;
  const Tag = interactive ? "button" : "span";
  const hint = refreshing ? "正在查询套餐" : refreshHint || (disabled ? "当前账号暂不可刷新套餐" : "点击刷新套餐 · 使用已保存授权，不会重新登录");
  return (
    <Tag className={"plan-badge" + (interactive ? " is-interactive" : "")} data-tier={tier} data-plan={plan}
      type={interactive ? "button" : undefined} disabled={interactive ? disabled : undefined}
      onClick={interactive && !disabled ? onRefresh : undefined} aria-busy={interactive ? Boolean(refreshing) : undefined}
      title={interactive ? hint : plan === "unknown" ? "未获取到已识别的套餐信息" : `账号套餐：${label}`}
      aria-label={interactive ? `${refreshing ? "正在查询套餐" : "刷新套餐"} ${accountLabel || label}，当前套餐：${label}` : `套餐：${label}`}>
      {short && plan === "team" ? "Team" : label}
      {interactive && <RefreshCw size={10} className={"plan-badge-refresh-icon" + (refreshing ? " spin" : "")} aria-hidden="true" focusable="false" />}
    </Tag>
  );
}
