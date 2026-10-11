"use client";

import { useMemo, useState } from "react";
import { api } from "@/lib/api";
import { useLoad } from "@/lib/hooks";
import type { Connection } from "@/lib/types";

/**
 * A searchable checklist of a connection's tables. Names are reported the
 * way the backup tools expect them: schema.table on PostgreSQL, the bare
 * table name on MySQL, where the connection already names the database.
 */
export default function TablePicker({ connection, selected, onChange }: {
  connection: Connection; selected: string[]; onChange: (tables: string[]) => void;
}) {
  const { data, error, loading } = useLoad(() => api.catalog(connection.id), [connection.id]);
  const [filter, setFilter] = useState("");

  const names = useMemo(() => {
    if (!data) return [];
    return data.schemas.flatMap((s) =>
      s.tables.map((t) => (connection.kind === "mysql" ? t.name : `${s.name}.${t.name}`)),
    );
  }, [data, connection.kind]);
  const shown = names.filter((n) => n.toLowerCase().includes(filter.toLowerCase()));

  const toggle = (name: string) =>
    onChange(selected.includes(name) ? selected.filter((n) => n !== name) : [...selected, name]);

  if (loading) return <div className="hint">Loading tables…</div>;
  if (error) return <div className="hint" style={{ color: "var(--danger)" }}>{error}</div>;
  if (names.length === 0) return <div className="hint">This database has no tables.</div>;

  return (
    <div className="picker">
      <div className="picker-head">
        <input className="input" placeholder="Filter tables" value={filter} onChange={(e) => setFilter(e.target.value)} />
        <span className="hint nowrap">{selected.length} of {names.length} selected</span>
      </div>
      <div className="picker-list">
        {shown.map((name) => (
          <label key={name} className="picker-item">
            <input type="checkbox" checked={selected.includes(name)} onChange={() => toggle(name)} />
            <span className="mono truncate" title={name}>{name}</span>
          </label>
        ))}
        {shown.length === 0 && <div className="hint" style={{ padding: 8 }}>No table matches the filter.</div>}
      </div>
    </div>
  );
}
