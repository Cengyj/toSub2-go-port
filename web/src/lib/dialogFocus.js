const scopes = new WeakMap();

// One modal owns keyboard focus at a time, including nested confirmations.
export function attachDialogFocus(container, { document: owner = document, onClose, isBusy }) {
  let state = scopes.get(owner);
  if (!state) {
    state = { stack: [], overflow: owner.body.style.overflow };
    scopes.set(owner, state);
    owner.body.style.overflow = "hidden";
  }
  const previous = owner.activeElement;
  const scope = { container };
  state.stack.push(scope);
  const isTop = () => state.stack[state.stack.length - 1] === scope;
  const elements = () => [...container.querySelectorAll(
    'button:not(:disabled), input:not(:disabled), textarea:not(:disabled), select:not(:disabled), a[href], [tabindex="0"]',
  )].filter((element) => !element.closest('[hidden], [inert]') && element.getClientRects().length);
  const focusFirst = () => (elements()[0] || container)?.focus();
  if (!container.contains(owner.activeElement)) focusFirst();
  const keyboard = (event) => {
    if (!isTop() || event.defaultPrevented) return;
    if (event.key === "Escape") {
      event.preventDefault();
      event.stopPropagation();
      if (!isBusy()) onClose();
      return;
    }
    if (event.key !== "Tab") return;
    const focusable = elements();
    const first = focusable[0], last = focusable[focusable.length - 1];
    if (!first) { event.preventDefault(); container.focus(); return; }
    if (event.shiftKey && (owner.activeElement === first || !container.contains(owner.activeElement))) {
      event.preventDefault(); last.focus();
    } else if (!event.shiftKey && (owner.activeElement === last || !container.contains(owner.activeElement))) {
      event.preventDefault(); first.focus();
    }
  };
  const focus = () => {
    if (isTop() && !container.contains(owner.activeElement)) focusFirst();
  };
  owner.addEventListener("keydown", keyboard);
  owner.addEventListener("focusin", focus);
  return () => {
    const wasTop = isTop();
    state.stack = state.stack.filter((entry) => entry !== scope);
    owner.removeEventListener("keydown", keyboard);
    owner.removeEventListener("focusin", focus);
    if (!state.stack.length) {
      owner.body.style.overflow = state.overflow;
      scopes.delete(owner);
    }
    if (wasTop && previous?.isConnected) previous.focus();
  };
}
