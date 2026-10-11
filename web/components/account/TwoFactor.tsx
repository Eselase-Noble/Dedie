"use client";

import QRCode from "qrcode";
import { useEffect, useState, type FormEvent } from "react";
import { api, errorMessage } from "@/lib/api";
import { Badge, Banner, Field, Modal } from "@/components/ui";

/** Shows recovery codes once, with a way to keep them. */
function RecoveryCodes({ codes, onClose }: { codes: string[]; onClose: () => void }) {
  const text = codes.join("\n");
  return (
    <Modal title="Save your recovery codes" onClose={onClose} footer={
      <>
        <button className="btn" onClick={() => void navigator.clipboard?.writeText(text)}>Copy</button>
        <button className="btn primary" onClick={onClose}>I have saved them</button>
      </>
    }>
      <p style={{ marginBottom: 12 }}>
        Each code signs you in once if you lose your phone. They are shown only now: store them in a password
        manager or print them.
      </p>
      <pre className="codes">{text}</pre>
    </Modal>
  );
}

export default function TwoFactor({ enabled, required, onChange }: {
  enabled: boolean; required: boolean; onChange: () => void;
}) {
  const [setup, setSetup] = useState<{ secret: string; uri: string } | null>(null);
  const [qr, setQr] = useState("");
  const [code, setCode] = useState("");
  const [password, setPassword] = useState("");
  const [disabling, setDisabling] = useState(false);
  const [recovery, setRecovery] = useState<string[] | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  useEffect(() => {
    if (!setup) return;
    // Drawn in the browser: the secret never leaves this page.
    QRCode.toDataURL(setup.uri, { margin: 1, width: 200 }).then(setQr).catch(() => setQr(""));
  }, [setup]);

  const run = async (action: () => Promise<void>) => {
    setBusy(true);
    setError("");
    try {
      await action();
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  const begin = () => run(async () => setSetup(await api.beginTotp()));
  const confirm = (e: FormEvent) => {
    e.preventDefault();
    void run(async () => {
      const res = await api.confirmTotp(code);
      setSetup(null); setCode("");
      setRecovery(res.recovery_codes);
    });
  };
  const disable = (e: FormEvent) => {
    e.preventDefault();
    void run(async () => {
      await api.disableTotp(password);
      setDisabling(false); setPassword("");
      onChange();
    });
  };

  return (
    <div className="card">
      <div className="card-head">
        <h2>Two-factor sign-in</h2>
        <Badge tone={enabled ? "ok" : required ? "danger" : "warn"}>{enabled ? "on" : "off"}</Badge>
      </div>
      <div className="card-body" style={{ maxWidth: 560 }}>
        {error && <Banner tone="error">{error}</Banner>}
        {required && !enabled && (
          <Banner tone="warn">Your organization requires two-factor sign-in. Set it up to continue using Dedie.</Banner>
        )}
        {enabled ? (
          <>
            <p style={{ marginBottom: 12 }}>
              Signing in asks for your password and a code from your authenticator app.
            </p>
            {!disabling ? (
              <button className="btn" onClick={() => setDisabling(true)} disabled={required}
                title={required ? "Required by your organization" : ""}>Turn off</button>
            ) : (
              <form onSubmit={disable}>
                <Field label="Confirm with your password">
                  <input className="input" type="password" value={password} onChange={(e) => setPassword(e.target.value)} required autoFocus autoComplete="current-password" />
                </Field>
                <div className="row-actions" style={{ justifyContent: "flex-start" }}>
                  <button className="btn" type="button" onClick={() => setDisabling(false)}>Cancel</button>
                  <button className="btn danger" type="submit" disabled={busy}>Turn off two-factor sign-in</button>
                </div>
              </form>
            )}
          </>
        ) : !setup ? (
          <>
            <p style={{ marginBottom: 12 }}>
              Add a second step to signing in: a 6-digit code from an authenticator app such as Google Authenticator,
              Microsoft Authenticator, 1Password or Authy. A stolen password alone is then not enough.
            </p>
            <button className="btn primary" onClick={begin} disabled={busy}>Set up</button>
          </>
        ) : (
          <form onSubmit={confirm}>
            <p style={{ marginBottom: 12 }}>1. Scan this code with your authenticator app, or enter the key by hand.</p>
            <div className="totp-setup">
              {qr && <img src={qr} alt="QR code for the authenticator app" width={200} height={200} />}
              <div>
                <div className="label">Setup key</div>
                <code className="mono" style={{ wordBreak: "break-all" }}>{setup.secret.match(/.{1,4}/g)?.join(" ")}</code>
              </div>
            </div>
            <p style={{ margin: "12px 0" }}>2. Enter the code the app shows.</p>
            <Field label="Code">
              <input className="input mono" style={{ maxWidth: 160 }} value={code} onChange={(e) => setCode(e.target.value)}
                required inputMode="numeric" autoComplete="one-time-code" placeholder="123456" autoFocus />
            </Field>
            <div className="row-actions" style={{ justifyContent: "flex-start" }}>
              <button className="btn" type="button" onClick={() => setSetup(null)}>Cancel</button>
              <button className="btn primary" type="submit" disabled={busy}>Turn on</button>
            </div>
          </form>
        )}
      </div>
      {recovery && <RecoveryCodes codes={recovery} onClose={() => { setRecovery(null); onChange(); }} />}
    </div>
  );
}
