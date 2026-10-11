"use client";

import { useEffect, useState, type ReactNode } from "react";
import { errorMessage } from "@/lib/api";

export function PageHeader({ title, subtitle, children }: { title: string; subtitle?: ReactNode; children?: ReactNode }) {
  return (
    <div className="page-header">
      <div>
        <h1>{title}</h1>
        {subtitle && <div className="subtitle">{subtitle}</div>}
      </div>
      {children && <div className="actions">{children}</div>}
    </div>
  );
}

type Tone = "ok" | "warn" | "danger" | "info" | "";

export function Badge({ tone = "", children }: { tone?: Tone; children: ReactNode }) {
  return <span className={`badge ${tone}`}>{children}</span>;
}

const STATUS_TONES: Record<string, Tone> = {
  completed: "ok", match: "ok", "count match": "ok", copied: "ok", ok: "ok", active: "ok",
  running: "info", queued: "info", copying: "info", streaming: "info",
  paused: "warn", warn: "warn", draft: "",
  failed: "danger", fail: "danger", aborted: "danger", mismatch: "danger",
};

/** A badge whose colour follows the conventional meaning of a status word. */
export function StatusBadge({ status }: { status: string }) {
  if (!status) return <span className="faint">—</span>;
  return <Badge tone={STATUS_TONES[status] ?? ""}>{status}</Badge>;
}

export function Banner({ tone = "info", children }: { tone?: "error" | "warn" | "ok" | "info"; children: ReactNode }) {
  if (!children) return null;
  return <div className={`banner ${tone}`} role={tone === "error" ? "alert" : "status"}>{children}</div>;
}

export function Field({ label, hint, children, className = "" }: { label: string; hint?: ReactNode; children: ReactNode; className?: string }) {
  return (
    <div className={`field ${className}`}>
      <label>{label}</label>
      {children}
      {hint && <span className="hint">{hint}</span>}
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

export function Modal({ title, onClose, children, footer, wide }: {
  title: string; onClose: () => void; children: ReactNode; footer?: ReactNode; wide?: boolean;
}) {
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => e.key === "Escape" && onClose();
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [onClose]);
  return (
    <div className="modal-backdrop" onMouseDown={(e) => e.target === e.currentTarget && onClose()}>
      <div className={`modal ${wide ? "wide" : ""}`} role="dialog" aria-modal="true" aria-label={title}>
        <div className="modal-head">
          <h2>{title}</h2>
          <button className="btn small ghost" onClick={onClose} aria-label="Close">Close</button>
        </div>
        <div className="modal-body">{children}</div>
        {footer && <div className="modal-foot">{footer}</div>}
      </div>
    </div>
  );
}

/**
 * A confirmation dialog for actions with consequences. The action runs when
 * the user confirms; a failure is shown in place so they can retry or cancel.
 */
export function ConfirmDialog({ title, children, confirmLabel, danger, onConfirm, onClose }: {
  title: string; children: ReactNode; confirmLabel: string; danger?: boolean;
  onConfirm: () => Promise<void>; onClose: () => void;
}) {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const run = async () => {
    setBusy(true);
    setError("");
    try {
      await onConfirm();
      onClose();
    } catch (err) {
      setError(errorMessage(err));
      setBusy(false);
    }
  };
  return (
    <Modal
      title={title}
      onClose={onClose}
      footer={
        <>
          <button className="btn" onClick={onClose} disabled={busy}>Cancel</button>
          <button className={`btn ${danger ? "danger" : "primary"}`} onClick={run} disabled={busy}>
            {busy ? "Working…" : confirmLabel}
          </button>
        </>
      }
    >
      {error && <Banner tone="error">{error}</Banner>}
      {children}
    </Modal>
  );
}

export function Progress({ value, total }: { value: number; total: number }) {
  const pct = total > 0 ? Math.min(100, Math.round((value / total) * 100)) : value > 0 ? 100 : 0;
  return (
    <div className={`progress ${pct >= 100 ? "done" : ""}`} aria-label={`${pct}%`}>
      <span style={{ width: `${pct}%` }} />
    </div>
  );
}

export function Loading() {
  return <div className="loading">Loading…</div>;
}
