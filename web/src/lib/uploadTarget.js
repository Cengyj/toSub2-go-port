export const UPLOAD_TARGET_PREFERENCE_KEY = "tosub2.upload-target-v1";

export function normalizeUploadTarget(value) { return value === "cpa" ? "cpa" : "sub2api"; }
export function uploadTargetLabel(value) { return normalizeUploadTarget(value) === "cpa" ? "CPA" : "Sub2API"; }
export function readUploadTargetPreference() {
  try { return normalizeUploadTarget(window.localStorage.getItem(UPLOAD_TARGET_PREFERENCE_KEY)); }
  catch { return "sub2api"; }
}
export function saveUploadTargetPreference(value) {
  try { window.localStorage.setItem(UPLOAD_TARGET_PREFERENCE_KEY, normalizeUploadTarget(value)); return true; }
  catch { return false; }
}
export function uploadTargetConflict(target, authorizationMethod) {
  return normalizeUploadTarget(target) === "cpa" && authorizationMethod === "bps";
}
export const CPA_AUTHORIZATION_HELP = "CPA 仅支持 ChatGPT / Codex 授权。请选择 ChatGPT 授权方式，或关闭自动上传。";

export function confirmedCPAUploadIds(receipt, requestedIds) {
  const requested = new Set(requestedIds), seen = new Set();
  if (!Array.isArray(receipt?.results) || receipt.results.some(item => {
    if (!item || typeof item.id !== "string" || !requested.has(item.id) || seen.has(item.id)
      || !["succeeded", "failed", "unknown", "skipped"].includes(item.outcome)) return true;
    seen.add(item.id);
    return false;
  })) return [];
  return receipt.results.filter(item => item.outcome === "succeeded").map(item => item.id);
}
