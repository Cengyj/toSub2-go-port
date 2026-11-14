import { useEffect, useRef, useState } from "react";
import { apiFetch } from "../lib/console.js";
import { subscriptionDisplayPlan, subscriptionResponseIsCurrent, subscriptionWarnings, validateSubscriptionRefresh } from "../lib/accountSubscription.js";

// This action reads a saved authorization through one explicit server endpoint.
// It has no polling, token refresh, login, or retry fallback.
export function useAccountSubscription({ token, enabled = false, jobs = [], onUpdated, onVerify }) {
  const [states, setStates] = useState({});
  const authority = useRef(null);
  const pending = useRef(new Map());
  const mounted = useRef(true);
  authority.current = { token, enabled: enabled === true, jobs, onUpdated, onVerify };
  useEffect(() => {
    mounted.current = true;
    setStates({});
    return () => { mounted.current = false; for (const cancel of pending.current.values()) cancel(); pending.current.clear(); };
  }, [token, enabled]);

  async function refresh(requested) {
    const current = authority.current;
    const job = current.jobs.find(value => value.id === requested?.id);
    if (!mounted.current || pending.current.has(requested?.id)) return null;
    if (!current.token || !current.enabled || job?.canRefreshSubscription !== true) {
      if (job) setStates(previous => ({ ...previous, [job.id]: { ...previous[job.id], busy: false, failed: false, error: job.subscriptionRefreshUnavailableReason || "当前账号暂不可刷新套餐；不会重新登录。" } }));
      return null;
    }
    const controller = new AbortController();
    let timer, cancel;
    const live = () => mounted.current && authority.current.token === current.token && authority.current.enabled && pending.current.get(job.id) === cancel;
    const requestJob = { id: job.id, email: job.email, authorizationMethod: job.authorizationMethod, createdAt: job.createdAt, attempt: job.attempt, lastOperationAt: job.lastOperationAt };
    setStates(previous => ({ ...previous, [job.id]: { ...previous[job.id], requestJob, checkedAt: job.subscriptionPlanCheckedAt, busy: true, failed: subscriptionDisplayPlan(job, previous[job.id]) === "unknown", error: "", warnings: [], uncertain: false } }));
    const deadline = new Promise((_, reject) => {
      cancel = () => { controller.abort(); reject(new Error("套餐读取已取消，已保留上次结果。")); };
      pending.current.set(job.id, cancel);
      timer = window.setTimeout(() => { controller.abort(); reject(new Error("套餐查询超时，已保留上次结果；未重新登录或自动重试。")); }, 30000);
    });
    try {
      const data = await Promise.race([apiFetch(current.token, `/api/jobs/${encodeURIComponent(job.id)}/refresh-subscription`, { method: "POST", body: "{}", signal: controller.signal }), deadline]);
      const verified = validateSubscriptionRefresh(data, job);
      if (!live()) return null;
      if (!subscriptionResponseIsCurrent(verified.job, job, authority.current.jobs.find(value => value.id === job.id))) {
        authority.current.onVerify?.();
        setStates(previous => ({ ...previous, [job.id]: { ...previous[job.id], busy: false, failed: false, error: "账号记录已变化，正在核对本地状态；未采用过期的查询响应。" } }));
        return null;
      }
      authority.current.onUpdated?.(verified.job);
      setStates(previous => ({ ...previous, [job.id]: { busy: false, failed: false, error: "", subscription: verified.subscription, warnings: subscriptionWarnings(verified.warnings) } }));
      return verified;
    } catch (error) {
      if (live()) {
        const uncertain = error.data?.code === "persistence_uncertain" && error.data?.outcome === "unknown";
        if (uncertain) {
          if (subscriptionResponseIsCurrent(error.data.job, job, authority.current.jobs.find(value => value.id === job.id))) authority.current.onUpdated?.(error.data.job);
          else authority.current.onVerify?.();
        }
        if (!uncertain) authority.current.onVerify?.();
        setStates(previous => ({ ...previous, [job.id]: { ...previous[job.id], busy: false, failed: true, checkedAt: uncertain ? error.data?.job?.subscriptionPlanCheckedAt || job.subscriptionPlanCheckedAt : job.subscriptionPlanCheckedAt, uncertain, error: uncertain ? "套餐已查询，本地保存待核实；不会自动重复查询。" : error.message || "套餐查询失败，已保留上次结果。" } }));
      }
      return null;
    } finally {
      window.clearTimeout(timer);
      if (pending.current.get(job.id) === cancel) pending.current.delete(job.id);
    }
  }
  return { states, refresh };
}
