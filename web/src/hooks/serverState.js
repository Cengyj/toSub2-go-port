import { useEffect, useMemo, useRef, useState } from "react";
import { apiFetch, POLL_INTERVAL_MS, withMonitorSettings } from "../lib/console.js";
import { createPollingTask } from "../lib/polling.js";
import { createLatestRead } from "../lib/latestRead.js";

function validateAccountSnapshot(data, page) {
  const object = value => value && typeof value === "object" && !Array.isArray(value);
  const records = value => Array.isArray(value) && value.every(item => object(item) && typeof item.id === "string" && item.id && typeof item.email === "string")
    && new Set(value.map(item => item.id)).size === value.length;
  const count = value => Number.isSafeInteger(value) && value >= 0;
  const invalid = () => { throw new Error("账号列表响应不完整"); };
  if (!object(data) || !records(data.jobs)) invalid();
  const selection = data.selection ?? data.jobs;
  const pagination = data.pagination ?? { page, pageSize: 20, total: data.jobs.length, totalPages: 1 };
  const stats = data.stats ?? { active: 0, queued: 0, completed: 0 };
  if (!records(selection) || !object(pagination) || !object(stats)
    || !["page", "pageSize", "totalPages"].every(key => count(pagination[key]) && pagination[key] > 0)
    || !count(pagination.total) || pagination.totalAll !== undefined && !count(pagination.totalAll)
    || !["active", "queued", "completed"].every(key => count(stats[key]))
    || stats.available !== undefined && !count(stats.available)) invalid();
  return { jobs: data.jobs, selection, pagination, stats };
}

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
  const current = useRef(null);
  useEffect(() => {
    if (!token) return;
    const polling = createPollingTask({
      intervalMs: POLL_INTERVAL_MS,
      request: (signal) => emailFilter.length
        ? apiFetch(token, "/api/jobs/query", { signal, readOnly: true, method: "POST", body: JSON.stringify({ page, emails: emailFilter }) })
        : apiFetch(token, `/api/jobs?page=${page}`, { signal }),
      onData: (data) => {
        // Validate the complete read before publishing any part of it. A later
        // exception cannot undo state setters that already replaced good data.
        const snapshot = validateAccountSnapshot(data, page);
        setJobs(snapshot.jobs);
        setJobSelectionIndex(snapshot.selection);
        setPagination(snapshot.pagination);
        setStats(snapshot.stats);
        setConnected(true);
        if (data.pagination?.page && data.pagination.page !== page) setPage(data.pagination.page);
        setConnectionError("");
      },
      onError: (error) => { setConnectionError(error.message); setConnected(false); },
    });
    current.current = polling;
    return () => { polling.stop(); if (current.current === polling) current.current = null; };
  }, [token, page, emailFilter]);
  return () => current.current?.refresh();
}

export function useMonitorPolling({ token, enabled, version, setStatus, setSettings }) {
  const [error, setError] = useState("");
  useEffect(() => {
    if (!token || !enabled) return;
    let readVersion = version.current;
    const polling = createPollingTask({
      intervalMs: 5000,
      request: async (signal) => {
        const revision = readVersion = version.current;
        const data = await apiFetch(token, "/api/sub2api/monitor", { signal });
        return { revision, data };
      },
      onData: ({ revision, data }) => {
        if (revision !== version.current) return;
        setStatus(data);
        setSettings((current) => withMonitorSettings(current, data));
        setError("");
      },
      onError: (error) => { if (readVersion === version.current) setError(error.message); },
    });
    return polling.stop;
  }, [token, enabled]);
  return error;
}
