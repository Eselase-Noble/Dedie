"use client";

import Link from "next/link";
import { formatAgo } from "@/lib/format";
import type { Attention } from "@/lib/types";
import { CheckIcon } from "@/components/icons";
import { LevelIcon } from "@/components/notifications/icons";

/**
 * What is waiting for somebody, most pressing first: connections that do
 * not answer, migrations that failed, stopped or need a decision, backups
 * that failed, and production databases nothing backs up.
 */
export default function NeedsAttention({ items }: { items: Attention[] }) {
  return (
    <section className="card" aria-labelledby="needs-attention">
      <div className="card-head">
        <h2 id="needs-attention">Needs attention</h2>
        {items.length > 0 && <span className={`badge ${items.some((i) => i.level === "error") ? "danger" : "warn"}`}>{items.length}</span>}
      </div>
      {items.length === 0 ? (
        <div className="attention-clear">
          <span className="note-icon success"><CheckIcon size={14} /></span>
          <span>Nothing is waiting for you. Failed or paused migrations, databases that stop answering and backups that fail are listed here.</span>
        </div>
      ) : (
        <ul className="attention">
          {items.map((item) => (
            <li key={item.title + item.link}>
              <LevelIcon level={item.level} />
              <span className="note-text">
                <span className="note-title"><span className="sr-only">{item.level === "error" ? "Problem" : "Warning"}: </span>{item.title}</span>
                {item.detail && <span className="note-body">{item.detail}</span>}
                {formatAgo(item.at) && <span className="note-time">{formatAgo(item.at)}</span>}
              </span>
              <Link className="btn small" href={item.link} aria-label={`Open: ${item.title}`}>Open</Link>
            </li>
          ))}
        </ul>
      )}
    </section>
  );
}
