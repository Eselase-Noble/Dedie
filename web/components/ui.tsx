"use client";

import Link from "next/link";
import { cloneElement, isValidElement, useEffect, useId, useRef, useState, type ReactElement, type ReactNode } from "react";
import { errorMessage } from "@/lib/api";
import { ArrowLeftIcon, CloseIcon } from "./icons";

/**
 * The top of a page: its name, a line about it and its actions. `back`
 * names the list a detail page belongs to and links to it.
 */
export function PageHeader({ title, subtitle, children, back }: {
  title: ReactNode; subtitle?: ReactNode; children?: ReactNode; back?: { href: string; label: string };
}) {
  return (
    <div className="page-header">
      <div>
        {back && <Link className="eyebrow" href={back.href}><ArrowLeftIcon size={14} />{back.label}</Link>}
        <h1>{title}</h1>
        {subtitle && <div className="subtitle">{subtitle}</div>}
      </div>
      {children && <div className="actions">{children}</div>}
    </div>
  );
}

type Tone = "ok" | "warn" | "danger" | "info" | "";

export function Badge({ tone = "", children, dotted, live }: { tone?: Tone; children: ReactNode; dotted?: boolean; live?: boolean }) {
  return <span className={`badge ${tone}${dotted ? " dotted" : ""}${live ? " live" : ""}`}>{children}</span>;
}

const STATUS_TONES: Record<string, Tone> = {
  completed: "ok", match: "ok", "count match": "ok", copied: "ok", ok: "ok", active: "ok",
  running: "info", queued: "info", copying: "info", streaming: "info",
  paused: "warn", warn: "warn", draft: "",
  failed: "danger", fail: "danger", aborted: "danger", mismatch: "danger",
};

// Work that is going on right now; its dot breathes.
const LIVE = new Set(["running", "copying", "streaming"]);

/** A badge whose colour follows the conventional meaning of a status word. */
export function StatusBadge({ status }: { status: string }) {
  if (!status) return <span className="faint">—</span>;
  return <Badge tone={STATUS_TONES[status] ?? ""} dotted live={LIVE.has(status)}>{status}</Badge>;
}

export function Banner({ tone = "info", children }: { tone?: "error" | "warn" | "ok" | "info"; children: ReactNode }) {
  if (!children) return null;
  return <div className={`banner ${tone}`} role={tone === "error" ? "alert" : "status"}>{children}</div>;
}

const CONTROLS = new Set(["input", "select", "textarea"]);

/**
 * A labelled form control with an optional line of help. When the child is
 * the control itself, the label and the help are tied to it, so that a
 * click on the label focuses it and a screen reader reads both.
 */
export function Field({ label, hint, children, className = "" }: { label: string; hint?: ReactNode; children: ReactNode; className?: string }) {
  const id = useId();
  let control = children;
  let target: string | undefined;
  if (isValidElement(children) && typeof children.type === "string" && CONTROLS.has(children.type)) {
    const child = children as ReactElement<{ id?: string; "aria-describedby"?: string }>;
    target = child.props.id ?? id;
    control = cloneElement(child, { id: target, "aria-describedby": hint ? `${id}-hint` : child.props["aria-describedby"] });
  }
  return (
    <div className={`field ${className}`}>
      <label htmlFor={target}>{label}</label>
      {control}
      {hint && <span className="hint" id={`${id}-hint`}>{hint}</span>}
    </div>
  );
}

export function Checkbox({ label, hint, checked, onChange, disabled }: {
  label: string; hint?: ReactNode; checked: boolean; onChange: (v: boolean) => void; disabled?: boolean;
}) {
  return (
    <label className="check">
      <input type="checkbox" checked={checked} disabled={disabled} onChange={(e) => onChange(e.target.checked)} />
      <span>
        <span className="label">{label}</span>
        {hint && <span className="hint">{hint}</span>}
      </span>
    </label>
  );
}

// How many dialogs are open; the page scrolls again when the last one closes.
let scrollLocks = 0;

/**
 * Stops the page underneath from scrolling while a dialog covers it, so
 * that the dialog's own scrollbar is the only one. The width the page's
 * scrollbar took is padded back in to keep the page from shifting.
 */
export function useScrollLock() {
  useEffect(() => {
    const root = document.documentElement;
    if (scrollLocks++ === 0) {
      root.style.paddingRight = `${window.innerWidth - root.clientWidth}px`;
      root.style.overflow = "hidden";
      root.classList.add("dialog-open");
    }
    return () => {
      if (--scrollLocks === 0) {
        root.style.overflow = "";
        root.style.paddingRight = "";
        root.classList.remove("dialog-open");
      }
    };
  }, []);
}

// The dialogs that are open, the one on top last. Escape and the Tab key
// belong to that one alone.
const openDialogs: symbol[] = [];

const FOCUSABLE = 'a[href], button:not([disabled]), input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])';

/**
 * Keeps the keyboard inside a dialog while it is open: the focus moves into
 * it, Tab cycles through its controls, Escape closes it, and whatever had
 * the focus before gets it back afterwards.
 */
export function useDialog(onClose: () => void) {
  const ref = useRef<HTMLDivElement>(null);
  const close = useRef(onClose);
  close.current = onClose;
  // Noted while rendering: by the time the effect runs, a field marked
  // autoFocus inside the dialog has already taken the focus.
  const opener = useRef<HTMLElement | null | undefined>(undefined);
  if (opener.current === undefined && typeof document !== "undefined") opener.current = document.activeElement as HTMLElement | null;
  useEffect(() => {
    const me = Symbol("dialog");
    openDialogs.push(me);
    const before = opener.current;
    const box = ref.current;
    // A field marked autoFocus has taken the focus already.
    if (box && !box.contains(document.activeElement)) box.focus({ preventScroll: true });
    const onKey = (e: KeyboardEvent) => {
      if (openDialogs[openDialogs.length - 1] !== me || !box) return;
      if (e.key === "Escape") {
        e.stopPropagation();
        close.current();
        return;
      }
      if (e.key !== "Tab") return;
      const stops = Array.from(box.querySelectorAll<HTMLElement>(FOCUSABLE)).filter((el) => el.offsetParent !== null);
      if (stops.length === 0) {
        e.preventDefault();
        return;
      }
      const first = stops[0], last = stops[stops.length - 1];
      const at = document.activeElement;
      if (!box.contains(at)) {
        e.preventDefault();
        first.focus();
      } else if (e.shiftKey && (at === first || at === box)) {
        e.preventDefault();
        last.focus();
      } else if (!e.shiftKey && at === last) {
        e.preventDefault();
        first.focus();
      }
    };
    window.addEventListener("keydown", onKey);
    return () => {
      window.removeEventListener("keydown", onKey);
      openDialogs.splice(openDialogs.indexOf(me), 1);
      if (before?.isConnected) before.focus();
    };
  }, []);
  return ref;
}

export function Modal({ title, onClose, children, footer, wide }: {
  title: string; onClose: () => void; children: ReactNode; footer?: ReactNode; wide?: boolean;
}) {
  useScrollLock();
  const ref = useDialog(onClose);
  const heading = useId();
  return (
    <div className="modal-backdrop" onMouseDown={(e) => e.target === e.currentTarget && onClose()}>
      <div ref={ref} className={`modal ${wide ? "wide" : ""}`} role="dialog" aria-modal="true" aria-labelledby={heading} tabIndex={-1}>
        <div className="modal-head">
          <h2 id={heading}>{title}</h2>
          <button className="icon-btn" onClick={onClose} aria-label="Close" title="Close (Esc)"><CloseIcon /></button>
        </div>
        <div className="modal-body">{children}</div>
        {footer && <div className="modal-foot">{footer}</div>}
      </div>
    </div>
  );
}

/**
 * The field in which the name of a production connection is typed before
 * something that destroys or disrupts. The dialog around it keeps its
 * button off until `value` is the name.
 */
export function ConfirmNameField({ name, value, onChange, autoFocus }: {
  name: string; value: string; onChange: (value: string) => void; autoFocus?: boolean;
}) {
  const id = useId();
  return (
    <div className="confirm-name">
      <div className="field">
        <label className="label" htmlFor={id}>Type <code>{name}</code> to confirm</label>
        <input id={id} className="input mono" value={value} onChange={(e) => onChange(e.target.value)}
          autoFocus={autoFocus} autoComplete="off" spellCheck={false} aria-describedby={`${id}-hint`} />
        <span className="hint" id={`${id}-hint`}>
          <span className="env production">production</span> Typing the name is recorded in the audit log.
        </span>
      </div>
    </div>
  );
}

/**
 * A confirmation dialog for actions with consequences. The action runs when
 * the user confirms; a failure is shown in place so they can retry or cancel.
 *
 * With `typeName` set, to the name of a production connection the action
 * touches, the name has to be typed first and is handed to the action.
 */
export function ConfirmDialog({ title, children, confirmLabel, danger, typeName, onConfirm, onClose }: {
  title: string; children: ReactNode; confirmLabel: string; danger?: boolean; typeName?: string;
  onConfirm: (typed?: string) => Promise<void>; onClose: () => void;
}) {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [typed, setTyped] = useState("");
  const ready = !typeName || typed.trim() === typeName;
  const run = async () => {
    if (!ready || busy) return;
    setBusy(true);
    setError("");
    try {
      await onConfirm(typeName);
      onClose();
    } catch (err) {
      setError(errorMessage(err));
      setBusy(false);
    }
  };
  const form = useId();
  return (
    <Modal
      title={title}
      onClose={onClose}
      footer={
        <>
          <button className="btn" type="button" onClick={onClose} disabled={busy}>Cancel</button>
          <button className={`btn ${danger || typeName ? "danger" : "primary"}`} type="submit" form={form} disabled={busy || !ready}>
            {busy ? "Working…" : confirmLabel}
          </button>
        </>
      }
    >
      <form id={form} onSubmit={(e) => { e.preventDefault(); void run(); }}>
        {error && <Banner tone="error">{error}</Banner>}
        {children}
        {typeName && <ConfirmNameField name={typeName} value={typed} onChange={setTyped} autoFocus />}
      </form>
    </Modal>
  );
}

export function Progress({ value, total }: { value: number; total: number }) {
  const pct = total > 0 ? Math.min(100, Math.round((value / total) * 100)) : value > 0 ? 100 : 0;
  return (
    <div className={`progress ${pct >= 100 ? "done" : ""}`} role="progressbar" aria-valuemin={0} aria-valuemax={100} aria-valuenow={pct} aria-label={`${pct}%`}>
      <span style={{ width: `${pct}%` }} />
    </div>
  );
}

/**
 * Stands in for content that is on its way, with roughly the shape it will
 * have. `page` is for a whole page that is still loading.
 */
export function Loading({ page, lines = 4 }: { page?: boolean; lines?: number }) {
  return (
    <div className={`loading${page ? " page" : ""}`} role="status" aria-live="polite">
      <span className="sr-only">Loading…</span>
      {Array.from({ length: lines }, (_, i) => <span key={i} className="skeleton" aria-hidden="true" />)}
    </div>
  );
}

/**
 * What a list shows when it has nothing in it: what belongs here, and the
 * action that puts the first thing in.
 */
export function EmptyState({ icon, title, children, action }: {
  icon?: ReactNode; title: string; children?: ReactNode; action?: ReactNode;
}) {
  return (
    <div className="empty-state">
      {icon && <span className="empty-icon">{icon}</span>}
      <h3>{title}</h3>
      {children && <p>{children}</p>}
      {action && <div className="actions">{action}</div>}
    </div>
  );
}
