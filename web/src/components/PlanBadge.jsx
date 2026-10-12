import React from "react";
import { UserRound, Zap, Plus, Crown, Gem, BriefcaseBusiness, UsersRound, Building2, GraduationCap, CircleHelp } from "lucide-react";
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

export function PlanBadge({ value, short = false }) {
  const plan = normalizeAccountPlan(value);
  const label = accountPlanLabel(plan);
  const tier = planTier(plan);
  return (
    <span className="plan-badge" data-tier={tier} data-plan={plan}
      title={plan === "unknown" ? "未获取到已识别的套餐信息" : `账号套餐：${label}`}
      aria-label={`套餐：${label}`}>
      {short && plan === "team" ? "Team" : short && plan === "unknown" ? "未知" : label}
    </span>
  );
}
