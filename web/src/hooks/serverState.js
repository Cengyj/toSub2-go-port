import { useEffect, useMemo, useRef, useState } from "react";
import { apiFetch, POLL_INTERVAL_MS, withMonitorSettings } from "../lib/console.js";
import { createPollingTask } from "../lib/polling.js";
import { createLatestRead } from "../lib/latestRead.js";

const selectionStrings = ["id", "email", "createdAt", "lastOperationAt", "authorizationMethod", "status", "operationStatus", "totpWorkflowStatus"];
const selectionBooleans = ["canResume", "canDownload", "canRegenerate", "canRetry", "canForceRelogin", "canSetupTotp", "canResetTotp", "canAddPassword", "canCompleteTotp", "totpExportBlocked", "totpRecoveryPending", "totpCredentialInvalidated", "totpCompletionPending"];
const selectionFields = [...selectionStrings, "attempt", ...selectionBooleans];
const selectionTimestamp = value => /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d{1,9})?(?:Z|[+-]\d{2}:\d{2})$/.test(value) && Number.isFinite(Date.parse(value));

function validateAccountSnapshot(data, page) {
  const object = value => value && typeof value === "object" && !Array.isArray(value);
  const records = value => Array.isArray(value) && value.every(item => object(item) && typeof item.id === "string" && item.id && typeof item.email === "string")
    && new Set(value.map(item => item.id)).size === value.length;
  const count = value => Number.isSafeInteger(value) && value >= 0;
  const invalid = () => { throw new Error("账号列表响应不完整"); };
  if (!object(data) || !records(data.jobs)) invalid();
  // Older servers omit the marker and retain their full selection contract.
  // Compact responses must carry every identity and safety field, including
  // explicit false and zero values, before any part of the page is published.
  if (data.selectionMode !== undefined && data.selectionMode !== "compact") invalid();
  if (data.selectionMode === "compact" && (!records(data.selection) || !data.selection.every(item =>
    selectionStrings.every(key => typeof item[key] === "string")
    && selectionBooleans.every(key => typeof item[key] === "boolean")
    && item.status && ["chatgpt", "bps"].includes(item.authorizationMethod)
    && count(item.attempt) && selectionTimestamp(item.createdAt) && selectionTimestamp(item.lastOperationAt)))) invalid();
  const selection = data.selection ?? data.jobs;
  const pagination = data.pagination ?? { page, pageSize: 20, total: data.jobs.length, totalPages: 1 };
  const stats = data.stats ?? { active: 0, queued: 0, completed: 0 };
  if (!records(selection) || !object(pagination) || !object(stats)
    || !["page", "pageSize", "totalPages"].every(key => count(pagination[key]) && pagination[key] > 0)
    || !count(pagination.total) || pagination.totalAll !== undefined && !count(pagination.totalAll)
    || !["active", "queued", "completed"].every(key => count(stats[key]))
    || stats.available !== undefined && !count(stats.available)) invalid();
  if (data.selectionMode === "compact") {
    const index = new Map(selection.map(item => [item.id, item]));
    const expectedRows = Math.min(pagination.pageSize, pagination.total - (pagination.page - 1) * pagination.pageSize);
    if (!object(data.pagination) || selection.length !== pagination.total
      || pagination.page > pagination.totalPages || pagination.totalPages !== Math.max(1, Math.ceil(pagination.total / pagination.pageSize))
      || data.jobs.length !== expectedRows || pagination.totalAll !== undefined && pagination.totalAll < pagination.total
      || !data.jobs.every(item => index.has(item.id) && selectionFields.every(key => item[key] === index.get(item.id)[key]))) invalid();
  }
  return { jobs: data.jobs, selection, pagination, stats };
}

export function useLatestRead(scopeKey, onLoading, timeoutMs = 10000) {
  const currentScope = useRef(null);
  if (!currentScope.current || currentScope.current.key !== scopeKey) currentScope.current = { key: scopeKey, active: true };
  const scope = currentScope.current;
  const gate = useMemo(() => createLatestRead({ onLoading, timeoutMs }), [onLoading, timeoutMs]);
  useEffect(() => {
    scope.active = true;
    return () => { scope.active = false; gate.cancel(); };
  }, [gate, scope]);
  return (request) => {
    if (currentScope.current !== scope || !scope.active) {
      const error = new Error("读取已取消");
      error.name = "AbortError";
      return Promise.reject(error);
    }
    return gate.run(request, () => currentScope.current === scope && scope.active);
  };
}

export function useAccountPolling({ token, page, emailFilter, setJobs, setJobSelectionIndex, setPagination, setStats, setConnected, setPage, setConnectionError }) {
  const owner = useRef(null);
  const filterKey = JSON.stringify(emailFilter);
  if (!owner.current || owner.current.token !== token || owner.current.page !== page || owner.current.filterKey !== filterKey) {
    if (owner.current) owner.current.active = false;
    owner.current = { token, page, filterKey, emails: [...emailFilter], active: true, version: 0, polling: null };
  }
  const scope = owner.current;
  const current = () => owner.current === scope && scope.active;
  useEffect(() => {
    if (!token || owner.current !== scope) return;
    scope.active = true;
    let requestedVersion = scope.version;
    const polling = createPollingTask({
      intervalMs: POLL_INTERVAL_MS,
      request: async (signal) => {
        if (!current()) throw new Error("读取已取消");
        const version = requestedVersion = scope.version;
        const data = await (scope.emails.length
        ? apiFetch(token, "/api/jobs/query?selection=compact", { signal, readOnly: true, method: "POST", body: JSON.stringify({ page, emails: scope.emails }) })
        : apiFetch(token, "/api/jobs?selection=compact&page=" + page, { signal }));
        return { data, version };
      },
      onData: ({ data, version: readVersion }) => {
        if (!current() || readVersion !== scope.version) return;
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
      onError: (error, source) => { if (current() && (source === "availability" || requestedVersion === scope.version)) { setConnectionError(error.message); setConnected(false); } },
    });
    scope.polling = polling;
    return () => { scope.active = false; polling.stop(); if (scope.polling === polling) scope.polling = null; };
  }, [scope]);
  const refresh = () => { if (current()) { scope.version += 1; scope.polling?.refresh(); } };
  refresh.invalidate = () => { if (current()) scope.version += 1; };
  return refresh;
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
