// Allow the server's complete read budget plus time to deliver its response.
// Sub2 options serially reads groups and proxies (120s each); SMS uses 20s.
export const SUB2_OPTIONS_TIMEOUT_MS = 245000;
export const SMS_OPTIONS_TIMEOUT_MS = 25000;

function canceledRead() {
  const error = new Error("读取已取消");
  error.name = "AbortError";
  return error;
}

// A user-triggered read replaces its predecessor; it never retries a request.
export function createLatestRead({ onLoading, timeoutMs = 10000, environment }) {
  const env = environment || { window, AbortController };
  let active = null;
  const cancel = () => {
    const pending = active;
    active = null;
    if (!pending) return;
    env.window.clearTimeout(pending.timer);
    pending.reject(canceledRead());
    pending.controller.abort();
    onLoading(false);
  };
  return {
    cancel,
    async run(request, isCurrent = () => true) {
      cancel();
      const pending = { controller: new env.AbortController() };
      active = pending;
      onLoading(true);
      const deadline = new Promise((_, reject) => {
        pending.reject = reject;
        pending.timer = env.window.setTimeout(() => {
          reject(new Error("配置读取超时，请稍后重试"));
          pending.controller.abort();
        }, timeoutMs);
      });
      try {
        const result = await Promise.race([Promise.resolve().then(() => request(pending.controller.signal)), deadline]);
        if (active !== pending || !isCurrent()) throw canceledRead();
        return result;
      } catch (error) {
        if (active !== pending || !isCurrent()) throw canceledRead();
        throw error;
      } finally {
        env.window.clearTimeout(pending.timer);
        if (active === pending) { active = null; onLoading(false); }
      }
    },
  };
}
