export const MONITOR_TARGET_PREFERENCE_KEY = "tosub2.monitor-target-v1";

export function normalizeMonitorTarget(value) { return value === "cpa" ? "cpa" : "sub2api"; }
export function readMonitorTargetPreference() {
  try { return normalizeMonitorTarget(window.localStorage.getItem(MONITOR_TARGET_PREFERENCE_KEY)); }
  catch { return "sub2api"; }
}
export function saveMonitorTargetPreference(value) {
  try { window.localStorage.setItem(MONITOR_TARGET_PREFERENCE_KEY, normalizeMonitorTarget(value)); return true; }
  catch { return false; }
}
