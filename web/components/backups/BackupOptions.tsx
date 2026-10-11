"use client";

import type { BackupLevel, BackupLocations, BackupType, Connection } from "@/lib/types";
import { Checkbox, Field } from "@/components/ui";
import TablePicker from "./TablePicker";
import { CONTENTS, LEVELS, METHODS, hasPhysical, levelHint, levelsAvailable, methodOf, physicalNote, type Method } from "./options";

export interface BackupChoice {
  type: BackupType;
  level: BackupLevel;
  tables: string[];
  locationId: string;
  plain: boolean;
}

export const DEFAULT_CHOICE: BackupChoice = { type: "full", level: "full", tables: [], locationId: "", plain: false };

/**
 * The choices that define a backup: how it is taken (logical or physical),
 * what it contains, how much history it holds, and where the file goes.
 * Shared by the backup dialog and the schedule form.
 */
export default function BackupOptions({ connection, locations, value, onChange }: {
  connection: Connection | null; locations: BackupLocations | null; value: BackupChoice; onChange: (v: BackupChoice) => void;
}) {
  const kind = connection?.kind ?? "";
  const method = methodOf(value.type);
  const leveled = levelsAvailable(kind, value.type);
  const level = LEVELS.find((l) => l.value === value.level) ?? LEVELS[0];
  const set = (change: Partial<BackupChoice>) => {
    const next = { ...value, ...change };
    // A level that the new choice does not support falls back to full.
    if (!levelsAvailable(kind, next.type)) next.level = "full";
    onChange(next);
  };
  const directory = value.locationId
    ? locations?.locations.find((l) => l.id === value.locationId)?.path ?? ""
    : locations?.default_path ?? "";

  return (
    <>
      <Field label="Method" hint={<>{METHODS[method].description}{method === "physical" && ` ${physicalNote(kind)}`}</>}>
        <div className="segmented">
          {(Object.keys(METHODS) as Method[]).map((m) => (
            <button key={m} type="button" className={method === m ? "active" : ""}
              disabled={m === "physical" && !hasPhysical(kind)}
              title={m === "physical" && !hasPhysical(kind) ? "Physical backups are available for PostgreSQL and MySQL" : undefined}
              onClick={() => set({ type: m === "physical" ? "physical" : "full", tables: [] })}>
              {METHODS[m].label}
            </button>
          ))}
        </div>
      </Field>
      {method === "logical" && (
        <Field label="Contents" hint={CONTENTS.find((c) => c.value === value.type)?.description}>
          <select className="select" value={value.type} onChange={(e) => set({ type: e.target.value as BackupType })}>
            {CONTENTS.map((o) => <option key={o.value} value={o.value}>{o.label}</option>)}
          </select>
        </Field>
      )}
      {value.type === "tables" && connection && (
        <Field label="Tables">
          <TablePicker connection={connection} selected={value.tables} onChange={(tables) => set({ tables })} />
        </Field>
      )}
      <Field label="Level" hint={<>{level.description} {levelHint(kind, value.type)}</>}>
        <div className="segmented">
          {LEVELS.map((l) => (
            <button key={l.value} type="button" className={value.level === l.value ? "active" : ""}
              disabled={l.value !== "full" && !leveled} onClick={() => set({ level: l.value })}>
              {l.label}
            </button>
          ))}
        </div>
      </Field>
      <Field label="Store in" hint={!directory ? undefined : directory.startsWith("s3://")
        ? <>Sent to <code>{directory}</code> as it is taken.</>
        : <>Written to <code>{directory}</code> on the machine that runs Dedie.</>}>
        <select className="select" value={value.locationId} onChange={(e) => set({ locationId: e.target.value })}>
          <option value="">Dedie&apos;s data directory (default)</option>
          {locations?.locations.map((l) => <option key={l.id} value={l.id}>{l.name}</option>)}
        </select>
      </Field>
      <Checkbox label="Write a plain file, without encryption" checked={value.plain} onChange={(plain) => set({ plain })}
        hint={value.plain
          ? "The file is written in the database's own format, so its tools can read it straight from that path. Anyone who can read the directory can read your data: protect it accordingly."
          : "Off: the file is encrypted and can only be read through Dedie (restore, download or export). Turn on to use the file directly with the database's own tools."} />
    </>
  );
}
