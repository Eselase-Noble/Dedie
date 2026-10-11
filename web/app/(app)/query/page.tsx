"use client";

import { useCallback, useEffect, useMemo, useState } from "react";
import { api, errorMessage } from "@/lib/api";
import { quoteIdent } from "@/lib/format";
import { useLoad } from "@/lib/hooks";
import { beginDrag, useStoredSize } from "@/lib/resize";
import { loadTabs, newId, newQueryTab, saveTabs, type Tab } from "@/lib/tabs";
import type { Catalog, CatalogTable, SavedQuery } from "@/lib/types";
import CreateObject from "@/components/CreateObjects";
import OwnershipDialog from "@/components/query/OwnershipDialog";
import QueryTab from "@/components/query/QueryTab";
import SavedQueries, { SaveQueryDialog } from "@/components/query/SavedQueries";
import TableDetails from "@/components/query/TableDetails";
import TableTab from "@/components/query/TableTab";
import SchemaTree, { type TableAction } from "@/components/SchemaTree";
import { useSession } from "@/components/session";
import { Badge, Banner, Loading } from "@/components/ui";

const LAST_CONNECTION = "dedie.query.connection";

/**
 * The query browser: a schema tree beside any number of tabs. A query tab
 * is an editor with its results; a table tab pages through one table and
 * can edit it. Open tabs are remembered for each connection.
 */
export default function QueryPage() {
  const { can } = useSession();
  const { data: connections, error: loadError, reload: reloadConnections } = useLoad(() => api.connections(), []);
  const { data: saved, reload: reloadSaved } = useLoad(() => api.savedQueries(), []);
  const [connId, setConnId] = useState("");
  const [catalog, setCatalog] = useState<Catalog | null>(null);
  const [catalogError, setCatalogError] = useState("");
  const [catalogLoading, setCatalogLoading] = useState(false);
  const [tabs, setTabs] = useState<Tab[]>([]);
  const [active, setActive] = useState("");
  const [showTree, setShowTree] = useState(true);
  // The object being created from the Create menu, if any.
  const [creating, setCreating] = useState<"database" | "schema" | "table" | null>(null);
  const [showCreate, setShowCreate] = useState(false);
  const [showSaved, setShowSaved] = useState(false);
  const [showOwner, setShowOwner] = useState(false);
  const [saving, setSaving] = useState<Tab | null>(null);
  const [details, setDetails] = useState<{ schema: string; table: CatalogTable } | null>(null);
  const [notice, setNotice] = useState("");

  // The tree's width is draggable and remembered.
  const [treeWidth, setTreeWidth] = useStoredSize("dedie.query.tree", 270, 180, () => Math.max(220, window.innerWidth * 0.45));

  const connection = connections?.find((c) => c.id === connId) ?? null;
  const mayWrite = can("query.write") && !!connection && !connection.read_only;

  // Reopen the connection used last time, if it still exists.
  useEffect(() => {
    if (!connections || connId) return;
    let remembered = "";
    try {
      remembered = window.localStorage.getItem(LAST_CONNECTION) ?? "";
    } catch {
      // Storage unavailable: fall back to the first connection.
    }
    const pick = connections.find((c) => c.id === remembered) ?? connections[0];
    if (pick) setConnId(pick.id);
  }, [connections, connId]);

  const loadCatalog = useCallback(async (id: string) => {
    setCatalogLoading(true);
    setCatalogError("");
    try {
      setCatalog(await api.catalog(id));
    } catch (err) {
      setCatalog(null);
      setCatalogError(errorMessage(err));
    } finally {
      setCatalogLoading(false);
    }
  }, []);

  useEffect(() => {
    if (!connId) return;
    const stored = loadTabs(connId);
    setTabs(stored.tabs);
    setActive(stored.tabs.some((t) => t.id === stored.active) ? stored.active : stored.tabs[0].id);
    try {
      window.localStorage.setItem(LAST_CONNECTION, connId);
    } catch {
      // Remembering the connection is optional.
    }
    void loadCatalog(connId);
  }, [connId, loadCatalog]);

  useEffect(() => {
    if (connId && tabs.length > 0) saveTabs(connId, tabs, active);
  }, [connId, tabs, active]);

  // Table and column names for the editor's completions. A table is offered
  // both by its bare name and qualified with its schema.
  const completions = useMemo(() => {
    const out: Record<string, string[]> = {};
    for (const schema of catalog?.schemas ?? []) {
      for (const t of [...schema.tables, ...schema.views]) {
        const columns = t.columns.map((c) => c.name);
        out[t.name] = columns;
        out[`${schema.name}.${t.name}`] = columns;
      }
    }
    return out;
  }, [catalog]);

  const findTable = useCallback((schema: string, table: string): CatalogTable | null => {
    const s = catalog?.schemas.find((x) => x.name === schema);
    return s?.tables.find((t) => t.name === table) ?? s?.views.find((t) => t.name === table) ?? null;
  }, [catalog]);

  const openTab = (tab: Tab) => {
    setTabs((list) => [...list, tab]);
    setActive(tab.id);
  };
  const addQuery = (sql = "", title = "", savedId?: string) => {
    const n = tabs.filter((t) => t.kind === "query").length + 1;
    openTab({ ...newQueryTab(n, sql, title), savedId });
  };
  const closeTab = (id: string) => {
    setTabs((list) => {
      const index = list.findIndex((t) => t.id === id);
      const next = list.filter((t) => t.id !== id);
      // The last tab is replaced rather than removed, so there is always
      // somewhere to type.
      const kept = next.length > 0 ? next : [newQueryTab(1)];
      if (id === active) setActive(kept[Math.min(index, kept.length - 1)].id);
      return kept;
    });
  };
  const patchTab = (id: string, change: Partial<Tab>) => setTabs((list) => list.map((t) => (t.id === id ? { ...t, ...change } : t)));

  const mongo = connection?.kind === "mongodb";

  const onTree = (action: TableAction, schema: string, table: string) => {
    // A collection is read with a find command; there is no SQL to write
    // and no table grid to page through.
    if (mongo && action !== "details") {
      addQuery(JSON.stringify({ find: table, filter: {}, limit: 100 }, null, 2), table);
      return;
    }
    if (action === "details") {
      const t = findTable(schema, table);
      if (t) setDetails({ schema, table: t });
      return;
    }
    if (action === "select") {
      addQuery(`SELECT * FROM ${quoteIdent(schema, connection?.kind)}.${quoteIdent(table, connection?.kind)} LIMIT 100`, table);
      return;
    }
    const existing = tabs.find((t) => t.kind === "table" && t.schema === schema && t.table === table);
    if (existing) setActive(existing.id);
    else openTab({ id: newId(), kind: "table", title: table, sql: "", schema, table });
  };

  return (
    <div className="workspace">
      <div className="workspace-bar">
        <h1>Query browser</h1>
        <select className="select" value={connId} onChange={(e) => setConnId(e.target.value)} aria-label="Connection">
          {(!connections || connections.length === 0) && <option value="">No connections</option>}
          {connections?.map((c) => <option key={c.id} value={c.id}>{c.name} ({c.kind}, {c.database})</option>)}
        </select>
        {connection?.read_only && <Badge>read-only connection</Badge>}
        <span className="spacer" />
        <button className="btn small" onClick={() => setShowSaved(true)}>Saved queries{saved && saved.length > 0 ? ` (${saved.length})` : ""}</button>
        {mayWrite && !mongo && (
          <div className="popover-anchor">
            <button className="btn small" onClick={() => setShowCreate((v) => !v)} aria-expanded={showCreate}>Create…</button>
            {showCreate && (
              <div className="popover menu">
                <button onClick={() => { setCreating("database"); setShowCreate(false); }}>New database</button>
                {connection?.kind !== "mysql" && (
                  <button onClick={() => { setCreating("schema"); setShowCreate(false); }}>New schema</button>
                )}
                <button onClick={() => { setCreating("table"); setShowCreate(false); }}>New table</button>
              </div>
            )}
          </div>
        )}
        {/* MySQL objects have no owner, so the option is PostgreSQL only. */}
        {mayWrite && can("server.admin") && connection?.kind === "postgres" && (
          <button className="btn small" onClick={() => setShowOwner(true)}>Change owner…</button>
        )}
        <button className="btn small" onClick={() => setShowTree((v) => !v)} aria-pressed={showTree}>
          {showTree ? "Hide schema" : "Show schema"}
        </button>
        <button className="btn small" onClick={() => connId && loadCatalog(connId)} disabled={!connId || catalogLoading}>
          Refresh schema
        </button>
      </div>
      {loadError && <Banner tone="error">{loadError}</Banner>}
      {notice && <Banner tone="ok">{notice}</Banner>}
      {creating && connection && (
        <CreateObject
          what={creating}
          connection={connection}
          catalog={catalog}
          canAddConnection={can("connections.manage")}
          onClose={() => setCreating(null)}
          onCreated={(message) => {
            setCreating(null);
            setNotice(message);
            void loadCatalog(connId);
            void reloadConnections();
          }}
        />
      )}

      <div className="workspace-body">
        {showTree && (
          <>
            <div className="pane tree-pane" style={{ width: treeWidth }}>
              {catalogLoading && <Loading />}
              {catalogError && <Banner tone="error">{catalogError}</Banner>}
              {catalog && !catalogLoading && <SchemaTree key={connId} catalog={catalog} onPick={onTree} />}
            </div>
            <div className="splitter x" role="separator" aria-orientation="vertical" title="Drag to resize"
              onPointerDown={(e) => beginDrag(e, "x", treeWidth, setTreeWidth)} />
          </>
        )}

        <div className="tab-area">
          <div className="doc-tabs" role="tablist">
            {tabs.map((t) => (
              <div key={t.id} role="tab" aria-selected={t.id === active} className={`doc-tab ${t.id === active ? "active" : ""}`}
                onClick={() => setActive(t.id)} title={t.kind === "table" ? `${t.schema}.${t.table}` : t.title}>
                <span className="doc-kind">{t.kind === "table" ? "▦" : "›_"}</span>
                <span className="doc-title">{t.title}</span>
                <button className="doc-close" aria-label={`Close ${t.title}`} onClick={(e) => { e.stopPropagation(); closeTab(t.id); }}>×</button>
              </div>
            ))}
            <button className="doc-new" onClick={() => addQuery()} title="New query tab">+</button>
          </div>
          {!connection && <div className="empty" style={{ margin: "auto" }}>Add a connection to start querying.</div>}
          {connection && tabs.map((t) => t.kind === "table" ? (
            <TableTab key={`${connId}:${t.id}`} connection={connection} schema={t.schema ?? ""} table={t.table ?? ""}
              info={findTable(t.schema ?? "", t.table ?? "")} mayWrite={mayWrite} active={t.id === active} />
          ) : (
            <QueryTab key={`${connId}:${t.id}`} connection={connection} mayWrite={mayWrite} completions={completions}
              sql={t.sql} onSql={(sql) => patchTab(t.id, { sql })} onSave={() => setSaving(t)} active={t.id === active} />
          ))}
        </div>
      </div>

      {showOwner && connection && (
        <OwnershipDialog connection={connection} catalog={catalog} onClose={() => setShowOwner(false)}
          onChanged={(message) => { setNotice(message); void loadCatalog(connId); }} />
      )}
      {details && <TableDetails schema={details.schema} table={details.table} onClose={() => setDetails(null)} />}
      {showSaved && (
        <SavedQueries queries={saved ?? []} onClose={() => setShowSaved(false)} onDeleted={() => void reloadSaved()}
          onOpen={(q: SavedQuery) => addQuery(q.sql, q.name, q.id)} />
      )}
      {saving && connection && (
        <SaveQueryDialog sql={saving.sql} kind={connection.kind} existing={saved?.find((q) => q.id === saving.savedId) ?? null}
          onClose={() => setSaving(null)}
          onSaved={(q) => {
            patchTab(saving.id, { title: q.name, savedId: q.id });
            void reloadSaved();
          }} />
      )}
    </div>
  );
}
