// Persist an unacknowledged local request across a lost response or page reload.
// The server binds it to each selected job; it is never sent to the provider.
function intentKey(ids) {
  return `tosub2.totp-intent.${[...ids].sort().join(",")}`;
}
export function pendingTotpIntent(ids) {
  const key = intentKey(ids);
  try {
    const saved = sessionStorage.getItem(key);
    if (/^[A-Za-z0-9_-]{8,128}$/.test(saved || "")) return saved;
  } catch {}
  const id = globalThis.crypto?.randomUUID?.() || `totp_${Date.now()}_${Math.random().toString(36).slice(2)}`;
  try { sessionStorage.setItem(key, id); } catch {}
  return id;
}
export function acknowledgeTotpIntent(ids, id) {
  try {
    const key = intentKey(ids);
    if (sessionStorage.getItem(key) === id) sessionStorage.removeItem(key);
  } catch {}
}
