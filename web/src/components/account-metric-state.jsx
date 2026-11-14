import React from "react";
import "./account-metric-state.css";

// A static state marker, never a measured percentage or a loading animation.
export function AccountMetricState({ label, className = "", trackClassName = "", tone = "neutral" }) {
  return <span className={["account-metric-state", className].filter(Boolean).join(" ")} role="img" aria-label={label}>
    <span className="metric-state-line"><span className="metric-state-label" data-tone={tone}>{label}</span><span className="metric-state-action" aria-hidden="true">查看</span></span>
    <span className={["metric-state-track", trackClassName].filter(Boolean).join(" ")} aria-hidden="true" />
  </span>;
}
