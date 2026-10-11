"use client";

import { formatBytes, formatNumber } from "@/lib/format";
import type { CatalogTable } from "@/lib/types";
import { Badge, Modal } from "@/components/ui";

/** The structure of a table: its columns, keys and indexes. */
export default function TableDetails({ schema, table, onClose }: { schema: string; table: CatalogTable; onClose: () => void }) {
  return (
    <Modal wide title={`${schema}.${table.name}`} onClose={onClose} footer={<button className="btn primary" onClick={onClose}>Close</button>}>
      <p className="muted" style={{ marginBottom: 12 }}>
        About {formatNumber(table.est_rows)} rows · {formatBytes(table.size_bytes)} · {table.columns.length} columns · {table.indexes.length} indexes
      </p>
      <div className="table-wrap" style={{ marginBottom: 16 }}>
        <table className="data">
          <thead><tr><th>Column</th><th>Type</th><th>Null</th><th>Default</th></tr></thead>
          <tbody>
            {table.columns.map((c) => (
              <tr key={c.name}>
                <td><strong className="mono">{c.name}</strong> {c.primary_key && <Badge tone="info">primary key</Badge>}</td>
                <td className="mono">{c.type}</td>
                <td>{c.nullable ? "yes" : "no"}</td>
                <td className="mono">{c.default || <span className="faint">—</span>}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      <div className="label" style={{ marginBottom: 6 }}>Indexes</div>
      {table.indexes.length === 0 ? <div className="hint">None.</div> : (
        <pre className="cell-value">{table.indexes.join("\n")}</pre>
      )}
    </Modal>
  );
}
