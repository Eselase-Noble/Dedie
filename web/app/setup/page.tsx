"use client";

import Link from "next/link";
import { useEffect, useState, type FormEvent } from "react";
import { api, errorMessage } from "@/lib/api";
import { Logo } from "@/components/icons";
import { Banner, Field, Loading } from "@/components/ui";

const MIN_PASSWORD = 12;

/**
 * Creates the organization and its first owner. This works once per
 * installation; afterwards, owners and admins add people from the Users page.
 */
export default function SetupPage() {
  // null until the server says whether an organization already exists.
  const [needsSetup, setNeedsSetup] = useState<boolean | null>(null);
  const [organization, setOrganization] = useState("");
  const [name, setName] = useState("");
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [confirm, setConfirm] = useState("");
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
    if (password.length < MIN_PASSWORD) {
      setError(`Choose a password of at least ${MIN_PASSWORD} characters.`);
      return;
    }
    if (password !== confirm) {
      setError("The two passwords do not match.");
      return;
    }
    setBusy(true);
    try {
      await api.setup({ organization, name, email, password });
      window.location.href = "/";
    } catch (err) {
      setError(errorMessage(err));
      setBusy(false);
    }
  };

  return (
    <div className="auth-page">
      <div className="auth-brand"><Logo size={36} /><span>Dedie</span></div>
      <form className="auth-card" onSubmit={submit}>
        <h1>Create account</h1>
        <p className="muted" style={{ marginBottom: 18 }}>
          Set up your organization and its first owner account.
        </p>
        {error && <Banner tone="error">{error}</Banner>}
        {needsSetup === null && !error && <Loading lines={3} />}

        {needsSetup === false && (
          <Banner tone="info">
            This installation already has an organization. To get an account, ask one of its owners or
            admins to add you from the Dedie users page.
          </Banner>
        )}

        {needsSetup && (
          <>
            <Field label="Organization">
              <input className="input" value={organization} onChange={(e) => setOrganization(e.target.value)} required autoFocus />
            </Field>
            <Field label="Your name">
              <input className="input" value={name} onChange={(e) => setName(e.target.value)} required autoComplete="name" />
            </Field>
            <Field label="Email" hint="You will sign in with this address.">
              <input className="input" type="email" value={email} onChange={(e) => setEmail(e.target.value)} required autoComplete="username" />
            </Field>
            <Field label="Password" hint={`At least ${MIN_PASSWORD} characters.`}>
              <input className="input" type="password" value={password} onChange={(e) => setPassword(e.target.value)} required
                minLength={MIN_PASSWORD} autoComplete="new-password" />
            </Field>
            <Field label="Confirm password">
              <input className="input" type="password" value={confirm} onChange={(e) => setConfirm(e.target.value)} required
                autoComplete="new-password" />
            </Field>
            <button className="btn primary" type="submit" disabled={busy} style={{ width: "100%" }}>
              {busy ? "Creating…" : "Create account"}
            </button>
          </>
        )}

        <div className="auth-alt">
          <span className="muted">Already have an account?</span>
          <Link className="btn" href="/login/">Sign in</Link>
        </div>
      </form>
    </div>
  );
}
