import React from "react";
import { CheckCircle2, CircleAlert, Clock3, FileText, Filter, History, LogIn } from "lucide-react";

const icons = { CheckCircle2, CircleAlert, Clock3, FileText, Filter, History, LogIn };

export function MonitorResultNotice({ presentation, className = "", previous = false }) {
  const Icon = icons[presentation.icon] || FileText;
  return (
    <div className={`monitor-outcome ${presentation.tone} ${className}`} role="status">
      <span className="monitor-outcome-icon"><Icon size={17} aria-hidden="true" /></span>
      <div>
        <strong>{previous ? "上次巡检 · " : ""}{presentation.label}</strong>
        {presentation.text !== presentation.label && <p>{presentation.text}</p>}
      </div>
    </div>
  );
}
