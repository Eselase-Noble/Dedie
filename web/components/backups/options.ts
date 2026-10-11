import type { BackupLevel, BackupType } from "@/lib/types";

export interface TypeOption {
  value: BackupType;
  label: string;
  description: string;
}

/** What a logical backup can contain. */
export const CONTENTS: TypeOption[] = [
  { value: "full", label: "Whole database", description: "Structure and data of the whole database." },
  { value: "schema", label: "Structure only", description: "Tables, indexes, views and routines without any rows." },
  { value: "data", label: "Data only", description: "Rows only, for loading into a database that already has the structure." },
  { value: "tables", label: "Selected tables", description: "Structure and data of the tables you choose." },
];

export const METHODS = {
  logical: {
    label: "Logical",
    description: "A description of the data that any server of the same kind can load: restore it into another database, another server or a newer version, whole or one table at a time. Slower to restore for very large databases.",
  },
  physical: {
    label: "Physical",
    description: "A copy of the server's own data files, covering every database on it. Fast to take and to restore for large servers, and the basis for differential and incremental backups. It is restored as a whole server of the same major version.",
  },
} as const;

export type Method = keyof typeof METHODS;

export const LEVELS: { value: BackupLevel; label: string; description: string }[] = [
  { value: "full", label: "Full", description: "Everything. It stands on its own and is what the other levels build on." },
  { value: "differential", label: "Differential", description: "What changed since the last full backup. Restoring needs that full backup and this one. Each differential is larger than the last until the next full backup." },
  { value: "incremental", label: "Incremental", description: "What changed since the last backup of any level. Smallest and quickest to take; restoring needs the full backup and every backup after it, in order." },
];

/** Whether a kind of database has physical backups. */
export function hasPhysical(kind: string): boolean {
  return kind === "postgres" || kind === "mysql";
}

/** What a physical backup needs on this kind of database. */
export function physicalNote(kind: string): string {
  if (kind === "mysql") {
    return "MySQL makes the copy itself with its clone plugin (8.0.17 or newer), so Dedie must run on the same machine as MySQL and be able to read the server's files. The account needs BACKUP_ADMIN.";
  }
  return "It needs an account with the REPLICATION attribute.";
}

/**
 * Whether differential and incremental backups exist for this choice:
 * physical backups on PostgreSQL, whole-database logical backups on MySQL.
 */
export function levelsAvailable(kind: string, type: BackupType): boolean {
  return (kind === "postgres" && type === "physical") || (kind === "mysql" && type === "full");
}

/** Why the levels are not available, in words for the form. */
export function levelHint(kind: string, type: BackupType): string {
  if (kind === "postgres" && type !== "physical") {
    return "A logical PostgreSQL backup is always full. Choose a physical backup for differential and incremental ones.";
  }
  if (kind === "mysql" && type !== "full") {
    return "MySQL takes differential and incremental backups of the whole database only.";
  }
  if (kind === "postgres") return "Needs PostgreSQL 17 or newer with summarize_wal = on, set before the full backup was taken.";
  if (kind === "mysql") return "Taken from the binary log, so binary logging must be on and the account needs RELOAD, REPLICATION CLIENT and REPLICATION SLAVE.";
  return "";
}

export function methodOf(type: string): Method {
  return type === "physical" ? "physical" : "logical";
}

export function typeLabel(type: string): string {
  if (type === "physical") return "Whole server";
  return CONTENTS.find((t) => t.value === type)?.label ?? type;
}

export function levelLabel(level: string): string {
  return LEVELS.find((l) => l.value === level)?.label ?? "Full";
}
