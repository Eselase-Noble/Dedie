"use client";

import { useEffect, useState, type FormEvent } from "react";
import { api, errorMessage } from "@/lib/api";
import type { SecurityPolicy } from "@/lib/types";
import { Banner, Checkbox, Field } from "@/components/ui";

/** The organization's sign-in rules, editable by administrators. */
export default function SecurityPolicyCard() {
  const [policy, setPolicy] = useState<SecurityPolicy | null>(null);
  const [busy, setBusy] = useState(false);
  const [notice, setNotice] = useState<{ tone: "ok" | "error"; text: string } | null>(null);

  useEffect(() => {
    api.policy().then(setPolicy).catch((err) => setNotice({ tone: "error", text: errorMessage(err) }));
  }, []);

  if (!policy) return notice ? <Banner tone={notice.tone}>{notice.text}</Banner> : null;
  const set = (change: Partial<SecurityPolicy>) => setPolicy({ ...policy, ...change });

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setNotice(null);
    try {
      setPolicy(await api.setPolicy(policy));
      setNotice({ tone: "ok", text: "Security policy saved." });
    } catch (err) {
      setNotice({ tone: "error", text: errorMessage(err) });
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="card">
      <div className="card-head"><h2>Security policy</h2></div>
      <form className="card-body" onSubmit={submit} style={{ maxWidth: 560 }}>
        {notice && <Banner tone={notice.tone}>{notice.text}</Banner>}
        <div className="form-row">
          <Field label="Minimum password length">
            <input className="input" type="number" min={8} max={128} value={policy.password_min_length}
              onChange={(e) => set({ password_min_length: Number(e.target.value) })} />
          </Field>
          <Field label="Session length (hours)" hint="How long a sign-in lasts.">
            <input className="input" type="number" min={1} max={720} value={policy.session_hours}
              onChange={(e) => set({ session_hours: Number(e.target.value) })} />
          </Field>
        </div>
        <Checkbox label="Require complex passwords" checked={policy.password_require_mixed}
          onChange={(v) => set({ password_require_mixed: v })}
          hint="Upper and lower case letters, a digit and a symbol. Applies to passwords set from now on." />
        <Checkbox label="Require two-factor sign-in for everyone" checked={policy.require_totp}
          onChange={(v) => set({ require_totp: v })}
          hint="Users who have not set it up are taken to their Account page and cannot use anything else until they do. Set up your own first." />
        <button className="btn primary" type="submit" disabled={busy}>{busy ? "Saving…" : "Save policy"}</button>
      </form>
    </div>
  );
}
