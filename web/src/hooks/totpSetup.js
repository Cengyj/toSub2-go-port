import { useEffect, useRef, useState } from "react";
import { apiFetch } from "../lib/console.js";
import { createPollingTask } from "../lib/polling.js";

// Only retrieve an already-created seed. This hook never starts or repeats any
// 2FA operation, and keeps private material out of the shared account cache.
export function useTotpSetupSecret({ token, jobId, status, lastOperationAt, terminal }) {
  const identity = useRef(null);
  const values = [token, jobId, status, lastOperationAt, terminal];
  if (!identity.current || values.some((value, index) => !Object.is(value, identity.current.values[index]))) identity.current = { values };
  const session = identity.current;
  const [result, setResult] = useState({ session, secret: "", error: "" });
  const reader = useRef(null);
  useEffect(() => {
    setResult({ session, secret: "", error: "" });
    if (!token || terminal || status !== "totp_setup_otp") return;
    const task = createPollingTask({
      intervalMs: 1000, maxIntervalMs: 10000,
      request: signal => apiFetch(token, `/api/jobs/${encodeURIComponent(jobId)}/totp-setup`, { signal, cache: "no-store" }),
      onData: data => {
        if (typeof data?.secret !== "string" || !data.secret) throw new Error("2FA 密钥读取响应不完整，请重新读取。");
        setResult({ session, secret: data.secret, error: "" });
        task.stop();
      },
      onError: error => setResult({ session, secret: "", error: error.message }),
    });
    const currentReader = { task, session };
    reader.current = currentReader;
    return () => { task.stop(); if (reader.current === currentReader) reader.current = null; };
  }, [session]);
  // Clearing in an effect alone exposes the previous secret for one render.
  const current = result.session === session ? result : { secret: "", error: "" };
  return { secret: current.secret, error: current.error, retry: () => { if (identity.current === session && reader.current?.session === session) reader.current.task.refresh(); } };
}
