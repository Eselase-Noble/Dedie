"use client";

import Link from "next/link";
import { useState, type FormEvent } from "react";
import { api, errorMessage } from "@/lib/api";
import { Banner, Field } from "@/components/ui";

/** Sign-in. This is the first screen of the application. */
export default function LoginPage() {
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setError("");
    setBusy(true);
    try {
      await api.login(email, password);
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
        <p className="muted" style={{ marginBottom: 18 }}>Sign in to your organization.</p>
        {error && <Banner tone="error">{error}</Banner>}
        <Field label="Email">
          <input className="input" type="email" value={email} onChange={(e) => setEmail(e.target.value)} required
            autoComplete="username" autoFocus />
        </Field>
        <Field label="Password">
          <input className="input" type="password" value={password} onChange={(e) => setPassword(e.target.value)} required
            autoComplete="current-password" />
        </Field>
        <button className="btn primary" type="submit" disabled={busy} style={{ width: "100%" }}>
          {busy ? "Signing in…" : "Sign in"}
        </button>
        <div className="auth-alt">
          <span className="muted">New installation?</span>
          <Link className="btn" href="/setup/">Create account</Link>
        </div>
      </form>
    </div>
  );
}
