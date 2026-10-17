// Poll only read operations. Mutations must never be retried by this scheduler.
export function createPollingTask({ request, onData, onError, onStart, intervalMs, timeoutMs = 10000, maxIntervalMs = 30000, environment }) {
  const env = environment || { window, document, navigator, AbortController };
  let stopped = false;
  let timer;
  let active = null;
  let failures = 0;
  let refreshPending = false;
  const interval = () => typeof intervalMs === "function" ? intervalMs() : intervalMs;
  const available = () => !env.document.hidden && env.navigator.onLine !== false;
  const report = (error) => {
    onError?.(error);
  };
  const clearTimer = () => {
    env.window.clearTimeout(timer);
    timer = undefined;
  };
  const schedule = (delay) => {
    clearTimer();
    if (!stopped && available()) timer = env.window.setTimeout(run, delay);
  };
  const suspend = () => {
    clearTimer();
    refreshPending = false;
    const pending = active;
    active = null;
    if (pending) {
      env.window.clearTimeout(pending.timeout);
      pending.controller.abort();
      pending.cancel();
    }
  };
  async function run() {
    if (stopped || !available()) return;
    if (active) { refreshPending = true; return; }
    clearTimer();
    const pending = { controller: new env.AbortController(), retry: true };
    active = pending;
    onStart?.();
    const deadline = new Promise((_, reject) => {
      pending.cancel = () => reject(new Error("读取已取消"));
      pending.timeout = env.window.setTimeout(() => {
        pending.controller.abort();
        reject(new Error("状态读取超时，稍后自动重试"));
      }, timeoutMs);
    });
    try {
      const data = await Promise.race([Promise.resolve().then(() => request(pending.controller.signal)), deadline]);
      if (stopped || active !== pending) return;
      failures = 0;
      onData(data);
    } catch (error) {
      if (stopped || active !== pending) return;
      failures += 1;
      // A client/business refusal needs a changed request or intentional
      // refresh; it must not enter a transport retry loop.
      pending.retry = !(error.status >= 400 && error.status < 500 && ![408, 425, 429].includes(error.status));
      report(error);
    } finally {
      env.window.clearTimeout(pending.timeout);
      if (active === pending) {
        active = null;
        const delay = refreshPending ? 0 : Math.min(maxIntervalMs, interval() * (2 ** Math.min(failures, 5)));
        refreshPending = false;
        if (pending.retry) schedule(delay);
      }
    }
  }
  const availabilityChanged = () => {
    if (stopped) return;
    if (!available()) {
      suspend();
      if (env.navigator.onLine === false) report(new Error("浏览器已离线，联网后自动恢复"));
    } else {
      failures = 0;
      void run();
    }
  };
  env.document.addEventListener("visibilitychange", availabilityChanged);
  env.window.addEventListener("online", availabilityChanged);
  env.window.addEventListener("offline", availabilityChanged);
  availabilityChanged();
  return {
    refresh: () => { failures = 0; void run(); },
    stop: () => {
      stopped = true;
      suspend();
      env.document.removeEventListener("visibilitychange", availabilityChanged);
      env.window.removeEventListener("online", availabilityChanged);
      env.window.removeEventListener("offline", availabilityChanged);
    },
  };
}
