"use client";

import { useState, type FormEvent } from "react";
import { api, errorMessage } from "@/lib/api";
import type { SecurityPolicy } from "@/lib/types";
import { Banner, Field } from "@/components/ui";

/** Describes the organization's password rules in one sentence. */
export function passwordRule(policy: SecurityPolicy | null): string {
  if (!policy) return "";
  return `At least ${policy.password_min_length} characters` +
    (policy.password_require_mixed ? ", with upper and lower case letters, a digit and a symbol." : ".");
}

export default function Password({ policy }: { policy: SecurityPolicy | null }) {
  const [current, setCurrent] = useState("");
  const [next, setNext] = useState("");
  const [repeat, setRepeat] = useState("");
  const [busy, setBusy] = useState(false);
  const [notice, setNotice] = useState<{ tone: "ok" | "error"; text: string } | null>(null);

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    if (next !== repeat) {
      setNotice({ tone: "error", text: "The two new passwords do not match." });
      return;
    }
    setBusy(true);
    setNotice(null);
    try {
      await api.changePassword(current, next);
      setCurrent(""); setNext(""); setRepeat("");
      setNotice({ tone: "ok", text: "Password changed. Your other sessions were signed out." });
    } catch (err) {
      setNotice({ tone: "error", text: errorMessage(err) });
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="card">
      <div className="card-head"><h2>Password</h2></div>
      <form className="card-body" onSubmit={submit} style={{ maxWidth: 420 }}>
        {notice && <Banner tone={notice.tone}>{notice.text}</Banner>}
        <Field label="Current password">
          <input className="input" type="password" value={current} onChange={(e) => setCurrent(e.target.value)} required autoComplete="current-password" />
        </Field>
        <Field label="New password" hint={passwordRule(policy)}>
          <input className="input" type="password" value={next} onChange={(e) => setNext(e.target.value)} required autoComplete="new-password" />
        </Field>
        <Field label="Repeat new password">
          <input className="input" type="password" value={repeat} onChange={(e) => setRepeat(e.target.value)} required autoComplete="new-password" />
        </Field>
        <button className="btn primary" type="submit" disabled={busy}>{busy ? "Saving…" : "Change password"}</button>
      </form>
    </div>
  );
}
