import { useEffect, useMemo, useRef, useState } from "react";
import { apiFetch, POLL_INTERVAL_MS, withMonitorSettings } from "../lib/console.js";
import { createPollingTask } from "../lib/polling.js";
import { createLatestRead } from "../lib/latestRead.js";

export function useLatestRead(scopeKey, onLoading, timeoutMs = 10000) {
  const currentScope = useRef(scopeKey);
  currentScope.current = scopeKey;
  const gate = useMemo(() => createLatestRead({ onLoading, timeoutMs }), [onLoading, timeoutMs]);
  useEffect(() => () => gate.cancel(), [gate, scopeKey]);
  return (request) => {
    const scope = currentScope.current;
    return gate.run(request, () => currentScope.current === scope);
  };
}

export function useAccountPolling({ token, page, emailFilter, setJobs, setJobSelectionIndex, setPagination, setStats, setConnected, setPage, setConnectionError }) {
  useEffect(() => {
    if (!token) return;
    const polling = createPollingTask({
      intervalMs: POLL_INTERVAL_MS,
      request: (signal) => emailFilter.length
        ? apiFetch(token, "/api/jobs/query", { signal, readOnly: true, method: "POST", body: JSON.stringify({ page, emails: emailFilter }) })
        : apiFetch(token, `/api/jobs?page=${page}`, { signal }),
      onData: (data) => {
        setJobs(data.jobs);
        setJobSelectionIndex(data.selection || data.jobs);
        setPagination(data.pagination || { page, pageSize: 20, total: data.jobs.length, totalPages: 1 });
        setStats(data.stats || { active: 0, queued: 0, completed: 0 });
        setConnected(true);
        if (data.pagination?.page && data.pagination.page !== page) setPage(data.pagination.page);
        setConnectionError("");
      },
      onError: (error) => { setConnectionError(error.message); setConnected(false); },
    });
    return polling.stop;
  }, [token, page, emailFilter]);
}

export function useMonitorPolling({ token, enabled, version, setStatus, setSettings }) {
  const [error, setError] = useState("");
  useEffect(() => {
    if (!token || !enabled) return;
    const polling = createPollingTask({
      intervalMs: 5000,
      request: async (signal) => {
        const revision = version.current;
        const data = await apiFetch(token, "/api/sub2api/monitor", { signal });
        return { revision, data };
      },
      onData: ({ revision, data }) => {
        if (revision !== version.current) return;
        setStatus(data);
        setSettings((current) => withMonitorSettings(current, data));
        setError("");
      },
      onError: (error) => setError(error.message),
    });
    return polling.stop;
  }, [token, enabled]);
  return error;
}
