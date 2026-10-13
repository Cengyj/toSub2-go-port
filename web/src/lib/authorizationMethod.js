export const AUTHORIZATION_METHOD_PREFERENCE_KEY = "tosub2.authorization-method-v1";

export function normalizeAuthorizationMethod(value) {
  return value === "bps" ? "bps" : "chatgpt";
}

export function authorizationMethodLabel(value) {
  return normalizeAuthorizationMethod(value) === "bps" ? "BPS" : "ChatGPT";
}

export function readAuthorizationMethodPreference(action = {}) {
  const fallback = normalizeAuthorizationMethod(action.authorizationMethod);
  // A resumed flow must keep the method associated with its saved progress.
  if (action.lockedMethod) return fallback;
  try {
    const stored = window.localStorage.getItem(AUTHORIZATION_METHOD_PREFERENCE_KEY);
    if (stored === "bps" || stored === "chatgpt") return stored;
  } catch {}
  return fallback;
}

export function saveAuthorizationMethodPreference(value) {
  if (value !== "bps" && value !== "chatgpt") return false;
  try {
    window.localStorage.setItem(AUTHORIZATION_METHOD_PREFERENCE_KEY, value);
    return true;
  } catch {
    return false;
  }
}
