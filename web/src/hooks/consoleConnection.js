import { useEffect, useState } from "react";
import { readResponse } from "../lib/console.js";
import { createPollingTask } from "../lib/polling.js";

export function useConsoleConnection() {
  const [connected, setConnected] = useState(false);
  const [token, setToken] = useState("");
  const [features, setFeatures] = useState({});
  const [connectionError, setConnectionError] = useState("");
  useEffect(() => {
    const polling = createPollingTask({
      intervalMs: 2000,
      request: (signal) => fetch("/api/bootstrap", { signal }).then(readResponse),
      onData: (data) => {
        setToken(data.token);
        setFeatures(data.features || {});
        setConnectionError("");
        polling.stop();
      },
      onError: (error) => setConnectionError(error.message),
    });
    return polling.stop;
  }, []);

  return { connected, setConnected, token, setToken, features, setFeatures, connectionError, setConnectionError };
}
