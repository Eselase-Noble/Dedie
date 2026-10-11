"use client";

import { useEffect, useState, type FormEvent } from "react";
import { api, errorMessage } from "@/lib/api";
import { Banner, Field } from "@/components/ui";

const MIN_PASSWORD = 12;

export default function LoginPage() {
  // null until the server says whether first-run setup is still needed.
  const [needsSetup, setNeedsSetup] = useState<boolean | null>(null);
  const [organization, setOrganization] = useState("");
  const [name, setName] = useState("");
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    api.setupStatus()
      .then((s) => setNeedsSetup(s.needs_setup))
      .catch((err) => setError(errorMessage(err)));
  }, []);

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setError("");
    if (needsSetup && password.length < MIN_PASSWORD) {
      setError(`Choose a password of at least ${MIN_PASSWORD} characters.`);
      return;
    }
    setBusy(true);
    try {
      if (needsSetup) {
        await api.setup({ organization, name, email, password });
      } else {
        await api.login(email, password);
      }
      window.location.href = "/";
    } catch (err) {
      setError(errorMessage(err));
      setBusy(false);
    }
  };

  return (
    <div className="auth-page">
      <form className="auth-card" onSubmit={submit}>
        <h1>Dedie</h1>
        <p className="muted" style={{ marginBottom: 18 }}>
          {needsSetup
            ? "Create your organization and the first owner account."
            : "Sign in to your organization."}
        </p>
        {error && <Banner tone="error">{error}</Banner>}
        {needsSetup === null && !error && <div className="loading">Loading…</div>}
        {needsSetup !== null && (
          <>
            {needsSetup && (
              <>
                <Field label="Organization">
                  <input className="input" value={organization} onChange={(e) => setOrganization(e.target.value)} required autoFocus />
                </Field>
                <Field label="Your name">
                  <input className="input" value={name} onChange={(e) => setName(e.target.value)} required autoComplete="name" />
                </Field>
              </>
            )}
            <Field label="Email">
              <input className="input" type="email" value={email} onChange={(e) => setEmail(e.target.value)} required
                autoComplete="username" autoFocus={!needsSetup} />
            </Field>
            <Field label="Password" hint={needsSetup ? `At least ${MIN_PASSWORD} characters.` : undefined}>
              <input className="input" type="password" value={password} onChange={(e) => setPassword(e.target.value)} required
                minLength={needsSetup ? MIN_PASSWORD : undefined}
                autoComplete={needsSetup ? "new-password" : "current-password"} />
            </Field>
            <button className="btn primary" type="submit" disabled={busy}>
              {busy ? "Please wait…" : needsSetup ? "Create organization" : "Sign in"}
            </button>
          </>
        )}
      </form>
    </div>
  );
}
