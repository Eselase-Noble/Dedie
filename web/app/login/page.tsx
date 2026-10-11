"use client";

import Link from "next/link";
import { useState, type FormEvent } from "react";
import { ApiError, api, errorMessage } from "@/lib/api";
import { Banner, Field } from "@/components/ui";

/** Sign-in. This is the first screen of the application. */
export default function LoginPage() {
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [showPassword, setShowPassword] = useState(false);
  // The second step, shown for accounts with two-factor sign-in.
  const [needCode, setNeedCode] = useState(false);
  const [code, setCode] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setError("");
    setBusy(true);
    try {
      const session = await api.login(email.trim(), password, needCode ? code : "");
      window.location.href = session.must_enroll_totp ? "/account/" : "/";
    } catch (err) {
      if (err instanceof ApiError && err.code === "totp_required") {
        setNeedCode(true);
      } else {
        setError(errorMessage(err));
      }
      setBusy(false);
    }
  };

  return (
    <div className="auth-page">
      <form className="auth-card" onSubmit={submit}>
        <h1>Dedie</h1>
        <p className="muted" style={{ marginBottom: 18 }}>
          {needCode ? "Enter the 6-digit code from your authenticator app." : "Sign in to your organization."}
        </p>
        {error && <Banner tone="error">{error}</Banner>}
        {!needCode ? (
          <>
            <Field label="Email">
              <input className="input" type="email" value={email} onChange={(e) => setEmail(e.target.value)} required
                autoComplete="username" autoFocus />
            </Field>
            <Field label="Password">
              <div className="input-group">
                <input className="input" type={showPassword ? "text" : "password"} value={password}
                  onChange={(e) => setPassword(e.target.value)} required autoComplete="current-password" />
                <button className="btn" type="button" onClick={() => setShowPassword((v) => !v)} aria-pressed={showPassword}>
                  {showPassword ? "Hide" : "Show"}
                </button>
              </div>
            </Field>
          </>
        ) : (
          <Field label="Authentication code" hint="Lost your phone? Enter one of your recovery codes instead.">
            <input className="input mono" value={code} onChange={(e) => setCode(e.target.value)} required autoFocus
              autoComplete="one-time-code" inputMode="text" spellCheck={false} placeholder="123456" />
          </Field>
        )}
        <button className="btn primary" type="submit" disabled={busy} style={{ width: "100%" }}>
          {busy ? "Signing in…" : needCode ? "Verify" : "Sign in"}
        </button>
        {needCode ? (
          <div className="auth-alt">
            <button className="btn" type="button" onClick={() => { setNeedCode(false); setCode(""); setError(""); }}>
              Back
            </button>
          </div>
        ) : (
          <div className="auth-alt">
            <span className="muted">New installation?</span>
            <Link className="btn" href="/setup/">Create account</Link>
          </div>
        )}
      </form>
    </div>
  );
}
