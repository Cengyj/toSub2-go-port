import React, { createContext, useCallback, useContext, useEffect, useRef, useState } from "react";
import { createPortal } from "react-dom";
import { ChevronDown, Ellipsis } from "lucide-react";

const ConfirmationContext = createContext(null);

export function ConfirmationProvider({ children }) {
  const [message, setMessage] = useState("");
  const [options, setOptions] = useState({});
  const resolver = useRef(null);
  const confirm = useCallback((text, preferences = {}) => {
    if (resolver.current) return Promise.resolve(false);
    return new Promise((resolve) => {
      resolver.current = resolve;
      setMessage(text);
      setOptions(preferences);
    });
  }, []);
  const finish = (accepted) => {
    const resolve = resolver.current;
    resolver.current = null;
    setMessage("");
    resolve?.(accepted);
  };
  useEffect(() => () => {
    resolver.current?.(false);
  }, []);

  return (
    <ConfirmationContext.Provider value={confirm}>
      {children}
      {message && createPortal(
        <DialogFrame onClose={() => finish(false)}>
          <section className="batch-dialog" role="dialog" aria-modal="true" aria-labelledby="confirmation-title">
            <div className="dialog-header">
              <h2 id="confirmation-title">{options.title || "确认操作"}</h2>
            </div>
            <p className="dialog-hint">{message}</p>
            <div className="dialog-footer">
              <button type="button" className="cancel-button" onClick={() => finish(false)}>
                取消
              </button>
              <button type="button" className={options.danger ? "primary-button danger-confirm" : "primary-button"} onClick={() => finish(true)}>
                {options.confirmLabel || "确认执行"}
              </button>
            </div>
          </section>
        </DialogFrame>, document.body,
      )}
    </ConfirmationContext.Provider>
  );
}

export function useConfirmation() {
  return useContext(ConfirmationContext);
}

export function ActionMenu({ label = "更多操作", ariaLabel, compact = false, children }) {
  const trigger = useRef(null);
  const menu = useRef(null);
  const [position, setPosition] = useState(null);
  useEffect(() => {
    if (!position) return;
    menu.current?.querySelector("button:not(:disabled)")?.focus();
    const dismiss = (event) => {
      if (!menu.current?.contains(event.target) && !trigger.current?.contains(event.target)) {
        setPosition(null);
      }
    };
    const close = () => setPosition(null);
    const scroll = (event) => {
      if (!menu.current?.contains(event.target)) close();
    };
    document.addEventListener("pointerdown", dismiss);
    window.addEventListener("resize", close);
    window.addEventListener("hashchange", close);
    document.addEventListener("scroll", scroll, true);
    return () => {
      document.removeEventListener("pointerdown", dismiss);
      window.removeEventListener("resize", close);
      window.removeEventListener("hashchange", close);
      document.removeEventListener("scroll", scroll, true);
    };
  }, [Boolean(position)]);
  return (
    <>
      <button
        ref={trigger}
        type="button"
        className={compact ? "icon-button action-menu-trigger compact" : "secondary-button action-menu-trigger"}
        aria-label={ariaLabel || label}
        title={compact ? label : undefined}
        aria-expanded={Boolean(position)}
        aria-haspopup="true"
        onClick={() => {
          if (position) {
            setPosition(null);
            return;
          }
          const rect = trigger.current.getBoundingClientRect();
          const below = window.innerHeight - rect.bottom > 240;
          const menuWidth = Math.min(220, window.innerWidth - 24);
          setPosition({
            right: Math.min(Math.max(12, window.innerWidth - rect.right), window.innerWidth - menuWidth - 12),
            ...(below
              ? { top: rect.bottom + 6, maxHeight: window.innerHeight - rect.bottom - 18 }
              : { bottom: window.innerHeight - rect.top + 6, maxHeight: rect.top - 18 }),
          });
        }}
      >
        {compact ? <Ellipsis size={18} aria-hidden="true" /> : <>{label}<ChevronDown size={14} aria-hidden="true" /></>}
      </button>
      {position && createPortal(
        <div
          ref={menu}
          className="action-menu-panel floating-action-menu"
          style={position}
          role="group"
          aria-label={ariaLabel || label}
          onClickCapture={(event) => {
            if (event.target.closest("button:not(:disabled)")) trigger.current?.focus();
          }}
          onClick={(event) => {
            if (event.target.closest("button:not(:disabled)")) setPosition(null);
          }}
          onKeyDown={(event) => {
            if (event.key === "Escape") {
              event.preventDefault();
              setPosition(null);
              trigger.current?.focus();
            }
            if (["ArrowDown", "ArrowUp", "Home", "End"].includes(event.key)) {
              event.preventDefault();
              const items = [...menu.current.querySelectorAll("button:not(:disabled)")];
              const current = items.indexOf(document.activeElement);
              const index = event.key === "Home"
                ? 0
                : event.key === "End"
                  ? items.length - 1
                  : (current + (event.key === "ArrowDown" ? 1 : -1) + items.length) % items.length;
              items[index]?.focus();
            }
          }}
          onBlur={(event) => {
            if (!event.currentTarget.contains(event.relatedTarget) && !trigger.current?.contains(event.relatedTarget)) {
              setPosition(null);
            }
          }}
        >
          {children}
        </div>,
        document.body,
      )}
    </>
  );
}

export function DialogFrame({ children, onClose, busy = false, inline = false }) {
  const container = useRef(null);
  const close = useRef(onClose);
  const locked = useRef(busy);
  close.current = onClose;
  locked.current = busy;
  useEffect(() => {
    if (inline) return;
    const previous = document.activeElement;
    const oldOverflow = document.body.style.overflow;
    document.body.style.overflow = "hidden";
    const elements = () => [...(container.current?.querySelectorAll(
      'button:not(:disabled), input:not(:disabled), textarea:not(:disabled), select:not(:disabled), a[href], [tabindex="0"]',
    ) || [])].filter((element) => element.getClientRects().length);
    const active = container.current?.contains(document.activeElement);
    if (!active) elements()[0]?.focus();
    const keyboard = (event) => {
      if (event.key === "Escape" && !locked.current) {
        event.preventDefault();
        close.current();
      }
      if (event.key !== "Tab") return;
      const focusable = elements();
      const first = focusable[0], last = focusable[focusable.length - 1];
      if (!first) {
        event.preventDefault();
        return;
      }
      if (event.shiftKey && (document.activeElement === first || !container.current?.contains(document.activeElement))) {
        event.preventDefault();
        last.focus();
      } else if (!event.shiftKey && (document.activeElement === last || !container.current?.contains(document.activeElement))) {
        event.preventDefault();
        first.focus();
      }
    };
    document.addEventListener("keydown", keyboard);
    return () => {
      document.body.style.overflow = oldOverflow;
      document.removeEventListener("keydown", keyboard);
      if (previous?.isConnected) previous.focus();
    };
  }, [inline]);
  if (inline) return <div className="settings-editor">{children}</div>;
  return (
    <div
      ref={container}
      className="modal-backdrop"
      role="presentation"
      onMouseDown={(event) => {
        if (event.target === event.currentTarget && !busy) onClose();
      }}
    >
      {children}
    </div>
  );
}

export function PageHeader({ eyebrow, title, description, children }) {
  return (
    <div className="page-header">
      <div>
        {eyebrow && <span className="eyebrow">{eyebrow}</span>}
        <h1>{title}</h1>
        <p>{description}</p>
      </div>
      {children && <div className="page-header-actions">{children}</div>}
    </div>
  );
}

export function StatCard({ icon: Icon, label, value, caption, tone = "neutral" }) {
  return (
    <div className={`stat-card ${tone}`}>
      <div className="stat-icon"><Icon size={20} aria-hidden="true" /></div>
      <div className="stat-content">
        <div className="stat-label">{label}</div>
        <div className="stat-value">{value}</div>
        <div className="stat-caption">{caption}</div>
      </div>
    </div>
  );
}
