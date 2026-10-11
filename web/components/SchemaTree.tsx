"use client";

import { useMemo, useState } from "react";
import { formatNumber } from "@/lib/format";
import type { Catalog, CatalogTable } from "@/lib/types";

/**
 * The object tree of a database. Clicking a table or view name calls
 * onPick; the caret expands its columns.
 */
export default function SchemaTree({ catalog, onPick }: {
  catalog: Catalog; onPick: (schema: string, table: string) => void;
}) {
  const [filter, setFilter] = useState("");
  const [open, setOpen] = useState<Record<string, boolean>>(() => {
    // Start with the first schema expanded so a small database is usable at once.
    const first = catalog.schemas.find((s) => s.name === "public") ?? catalog.schemas[0];
    return first ? { [`s:${first.name}`]: true } : {};
  });
  const toggle = (key: string) => setOpen((o) => ({ ...o, [key]: !o[key] }));
  const needle = filter.trim().toLowerCase();

  const schemas = useMemo(() => {
    if (!needle) return catalog.schemas;
    const match = (name: string) => name.toLowerCase().includes(needle);
    return catalog.schemas
      .map((s) => ({
        ...s,
        tables: s.tables.filter((t) => match(t.name)),
        views: s.views.filter((t) => match(t.name)),
        functions: s.functions.filter(match),
        sequences: s.sequences.filter(match),
      }))
      .filter((s) => s.tables.length + s.views.length + s.functions.length + s.sequences.length > 0);
  }, [catalog, needle]);

  const relation = (schema: string, t: CatalogTable, isView: boolean) => {
    const key = `t:${schema}.${t.name}`;
    return (
      <div key={key}>
        <div className="tree-row" style={{ paddingLeft: 22 }}>
          <button className="link-btn caret" onClick={() => toggle(key)} aria-label={open[key] ? "Collapse" : "Expand"}>
            {open[key] ? "▾" : "▸"}
          </button>
          <button className="link-btn tname" style={{ color: "inherit" }} onClick={() => onPick(schema, t.name)}
            title={`Insert a SELECT for ${schema}.${t.name}`}>
            {t.name}
          </button>
          {!isView && <span className="tmeta" title="Estimated rows">~{formatNumber(t.est_rows)}</span>}
        </div>
        {open[key] && t.columns.map((c) => (
          <div className="tree-col" key={c.name} title={c.default ? `default ${c.default}` : undefined}>
            <span>{c.name}</span>
            {c.primary_key && <span className="pk">PK</span>}
            <span className="ctype">{c.type}{c.nullable ? "" : " not null"}</span>
          </div>
        ))}
      </div>
    );
  };

  return (
    <div className="tree">
      <div className="tree-filter">
        <input className="input" placeholder="Filter objects" value={filter} onChange={(e) => setFilter(e.target.value)} />
      </div>
      {schemas.length === 0 && <div className="empty">Nothing matches.</div>}
      {schemas.map((s) => {
        const key = `s:${s.name}`;
        const expanded = !!needle || open[key];
        return (
          <div key={s.name}>
            <button className="tree-row" onClick={() => toggle(key)}>
              <span className="caret">{expanded ? "▾" : "▸"}</span>
              <strong className="tname">{s.name}</strong>
              <span className="tmeta">{s.tables.length + s.views.length}</span>
            </button>
            {expanded && (
              <>
                {s.tables.length > 0 && <div className="tree-group">Tables</div>}
                {s.tables.map((t) => relation(s.name, t, false))}
                {s.views.length > 0 && <div className="tree-group">Views</div>}
                {s.views.map((t) => relation(s.name, t, true))}
                {s.functions.length > 0 && <div className="tree-group">Functions</div>}
                {s.functions.map((f) => <div className="tree-leaf" key={f} title={f}>{f}</div>)}
                {s.sequences.length > 0 && <div className="tree-group">Sequences</div>}
                {s.sequences.map((q) => <div className="tree-leaf" key={q} title={q}>{q}</div>)}
              </>
            )}
          </div>
        );
      })}
    </div>
  );
}
