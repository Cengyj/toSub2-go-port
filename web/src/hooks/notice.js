import { useEffect, useRef, useState } from "react";

// A receipt acknowledges a request; ongoing work remains in its own status UI.
// Only explicitly transient notices expire. Errors and operation progress stay.
export function useNotice() {
  const [notice, setNotice] = useState({ message: "", tone: "info" });
  const timer = useRef(null);
  const version = useRef(0);

  function cancelDismissal() {
    version.current++;
    if (timer.current !== null) window.clearTimeout(timer.current);
    timer.current = null;
  }

  function clearNotice() {
    cancelDismissal();
    setNotice({ message: "", tone: "info" });
  }

  function showNotice(message, tone = "info", { dismissAfterMs = 0 } = {}) {
    cancelDismissal();
    setNotice({ message, tone });
    if (message && Number.isFinite(dismissAfterMs) && dismissAfterMs > 0) {
      const expectedVersion = version.current;
      timer.current = window.setTimeout(() => {
        // An already queued timeout must not dismiss a newer operation's notice.
        if (version.current === expectedVersion) clearNotice();
      }, dismissAfterMs);
    }
  }

  useEffect(() => cancelDismissal, []);
  return { ...notice, showNotice, clearNotice };
}
