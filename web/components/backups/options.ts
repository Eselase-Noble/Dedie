import type { BackupType } from "@/lib/types";

export interface TypeOption {
  value: BackupType;
  label: string;
  description: string;
}

const COMMON: TypeOption[] = [
  { value: "full", label: "Full", description: "Structure and data of the whole database." },
  { value: "schema", label: "Structure only", description: "Tables, indexes, views and routines without any rows." },
  { value: "data", label: "Data only", description: "Rows only, for loading into a database that already has the structure." },
  { value: "tables", label: "Selected tables", description: "Structure and data of the tables you choose." },
];

const PHYSICAL: TypeOption = {
  value: "physical",
  label: "Physical (whole server)",
  description:
    "A copy of the server's data files covering every database, taken online with pg_basebackup. Needs an account with the REPLICATION attribute. Recovered by extracting it into an empty data directory.",
};

/** The backup types a database kind supports. */
export function typeOptions(kind: string): TypeOption[] {
  return kind === "postgres" ? [...COMMON, PHYSICAL] : COMMON;
}

export function typeLabel(type: string): string {
  return [...COMMON, PHYSICAL].find((t) => t.value === type)?.label ?? type;
}
