import { useEffect, useRef, useState } from "react";

// A completed action can remove its trigger or temporarily disable it. Observe
// only this operation, and yield permanently if the user moves focus or acts.
export function createRemovalFocusGuard(trigger, getFallback, owner = trigger?.ownerDocument || globalThis.document) {
  if (!trigger || !owner?.addEventListener) return null;
  const closingDialog = owner.activeElement?.closest?.('[role="dialog"]');
  let disposed = false;
  function cancel() {
    if (disposed) return;
    disposed = true;
    owner.removeEventListener("focusin", onFocus, true);
    owner.removeEventListener("pointerdown", onInteraction, true);
    owner.removeEventListener("keydown", onInteraction, true);
  }
  function onFocus(event) {
    const target = event.target;
    if (target === trigger || target === owner.body || target === owner.documentElement || closingDialog?.contains(target)) return;
    cancel();
  }
  function onInteraction(event) {
    if (!closingDialog?.contains(event.target)) cancel();
  }
  owner.addEventListener("focusin", onFocus, true);
  owner.addEventListener("pointerdown", onInteraction, true);
  owner.addEventListener("keydown", onInteraction, true);
  return {
    cancel,
    check(settled = false) {
      if (disposed) return true;
      if (trigger.isConnected && !settled) return false;
      const focused = owner.activeElement;
      const lost = !focused || focused === owner.body || focused === owner.documentElement || !focused.isConnected;
      if (lost) {
        const available = element => element?.isConnected && !element.disabled && element.getClientRects?.().length;
        const target = available(trigger) ? trigger : getFallback();
        if (available(target)) target.focus();
      }
      cancel();
      return true;
    },
  };
}
export function useRemovedRowFocus(rows, fallbackRef, busy = false) {
  const latestRows = useRef(rows), pending = useRef(null);
  const [completion, setCompletion] = useState(0);
  latestRows.current = rows;
  useEffect(() => {
    const operation = pending.current;
    if (operation?.complete && !busy && operation.guard.check(!operation.observeRemoval || rows !== operation.baseline)) pending.current = null;
  }, [rows, busy, completion]);
  useEffect(() => () => pending.current?.guard.cancel(), []);
  return {
    capture: () => globalThis.document?.activeElement || null,
    begin(trigger) {
      pending.current?.guard.cancel();
      const guard = createRemovalFocusGuard(trigger, () => fallbackRef.current);
      const operation = guard ? { guard, complete: false, baseline: latestRows.current } : null;
      pending.current = operation;
      return operation;
    },
    complete(operation, observeRemoval) {
      if (!operation || pending.current !== operation) return;
      operation.baseline = latestRows.current;
      operation.complete = true;
      operation.observeRemoval = observeRemoval;
      // Check after React commits the busy state and row changes. A timer could
      // restore focus before the button is enabled, or after a new user action.
      setCompletion(value => value + 1);
    },
  };
}
