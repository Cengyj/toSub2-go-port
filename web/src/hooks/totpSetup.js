import { useEffect, useRef, useState } from "react";
import { apiFetch } from "../lib/console.js";
import { createPollingTask } from "../lib/polling.js";

// Only retrieve an already-created seed. This hook never starts or repeats any
// 2FA operation, and keeps private material out of the shared account cache.
export function useTotpSetupSecret({ token, jobId, status, lastOperationAt, terminal }) {
  const [secret, setSecret] = useState("");
  const [error, setError] = useState("");
  const reader = useRef(null);
  useEffect(() => {
    setSecret("");
    setError("");
    if (!token || terminal || status !== "totp_setup_otp") return;
    const task = createPollingTask({
      intervalMs: 1000, maxIntervalMs: 10000,
      request: signal => apiFetch(token, `/api/jobs/${encodeURIComponent(jobId)}/totp-setup`, { signal, cache: "no-store" }),
      onData: data => {
        if (typeof data?.secret !== "string" || !data.secret) throw new Error("2FA 密钥读取响应不完整，请重新读取。");
        setSecret(data.secret);
        setError("");
        task.stop();
      },
      onError: error => setError(error.message),
    });
    reader.current = task;
    return () => { task.stop(); if (reader.current === task) reader.current = null; };
  }, [token, jobId, status, lastOperationAt, terminal]);
  return { secret, error, retry: () => reader.current?.refresh() };
}
