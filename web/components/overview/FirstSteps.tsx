"use client";

import Link from "next/link";
import { useEffect, useState } from "react";
import type { Summary } from "@/lib/types";

const HIDDEN_KEY = "dedie.firststeps.hidden";

/**
 * The first things to do with a new installation: save a connection, take
 * a backup of it, and move something. Each step is ticked off by what the
 * installation actually holds, so it is the same for everyone and survives
 * a change of browser. The card goes away when all three are done, or when
 * it is hidden, which is remembered in this browser.
 */
export default function FirstSteps({ setup }: { setup: NonNullable<Summary["setup"]> }) {
  // Unknown until the browser's storage has been read, so that a card
  // somebody has hidden does not flash up first.
  const [hidden, setHidden] = useState<boolean | null>(null);
  useEffect(() => {
    try {
      setHidden(window.localStorage.getItem(HIDDEN_KEY) === "1");
    } catch {
      setHidden(false);
    }
  }, []);
  const hide = () => {
    setHidden(true);
    try {
      window.localStorage.setItem(HIDDEN_KEY, "1");
    } catch {
      // Remembering the choice is optional.
    }
  };

  const steps = [
    {
      done: setup.connection, title: "Save a connection",
      text: "Tell Dedie where a database is and which account to use. The password is stored encrypted. Label it production, staging or development while you are there.",
      href: "/connections/?new=1", action: "New connection", ready: true,
    },
    {
      done: setup.backup, title: "Take a backup",
      text: "A backup of a PostgreSQL or MySQL connection, encrypted and checked. Add a schedule afterwards and they take themselves.",
      href: "/backups/?new=1", action: "Create backup", ready: setup.connection,
    },
    {
      done: setup.migration, title: "Try a migration",
      text: "With a second connection, copy a database to it offline. Creating a migration changes nothing; its preflight check says what would happen.",
      href: "/migrations/?new=offline", action: "New migration", ready: setup.connection,
    },
  ];
  const left = steps.filter((s) => !s.done).length;
  if (hidden !== false || left === 0) return null;
  // The step to take now is the first one not done yet.
  const next = steps.findIndex((s) => !s.done);

  return (
    <section className="card" aria-labelledby="first-steps">
      <div className="card-head">
        <h2 id="first-steps">Getting started</h2>
        <div className="actions">
          <span className="muted">{steps.length - left} of {steps.length} done</span>
          <button className="btn small ghost" onClick={hide} title="Hide this card in this browser">Hide</button>
        </div>
      </div>
      <ol className="checklist">
        {steps.map((step, i) => (
          <li key={step.title} className={step.done ? "done" : ""}>
            <span className="mark" aria-hidden="true" />
            <span className="note-text">
              <span className="note-title">{step.title}{step.done && <span className="sr-only"> (done)</span>}</span>
              <span className="note-body">{step.text}</span>
            </span>
            {!step.done && step.ready && (
              <Link className={`btn small ${i === next ? "primary" : ""}`} href={step.href}>{step.action}</Link>
            )}
          </li>
        ))}
      </ol>
    </section>
  );
}
