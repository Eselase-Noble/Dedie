// Types mirroring the JSON produced by the Go API (apps/internal/httpapi).

export type Permission =
  | "connections.view"
  | "connections.manage"
  | "query.read"
  | "query.write"
  | "migrations.view"
  | "migrations.manage"
  | "server.admin"
  | "backups.create"
  | "backups.restore"
  | "users.manage"
  | "audit.view";

export type Role = "owner" | "admin" | "operator" | "analyst" | "viewer";

export const ROLES: Role[] = ["owner", "admin", "operator", "analyst", "viewer"];

export interface User {
  id: string;
  org_id: string;
  email: string;
  name: string;
  role: Role;
  disabled: boolean;
  created_at: string;
}

export interface Session {
  user: User;
  organization: string;
  permissions: Permission[];
  kinds: string[];
}

export interface Connection {
  id: string;
  name: string;
  kind: string;
  host: string;
  port: number;
  database: string;
  username: string;
  sslmode: string;
  ssl_root_cert: string;
  read_only: boolean;
  created_by: string;
  created_at: string;
}

export interface ConnectionInput {
  name: string;
  kind: string;
  host: string;
  port: number;
  database: string;
  username: string;
  password: string;
  sslmode: string;
  ssl_root_cert: string;
  read_only: boolean;
}

export interface ServerInfo {
  kind: string;
  version: string;
  version_num: number;
  database: string;
  user: string;
  encrypted: boolean;
  superuser: boolean;
}

export interface TableColumn {
  name: string;
  type: string;
  nullable: boolean;
  primary_key: boolean;
  default?: string;
}

export interface CatalogTable {
  name: string;
  est_rows: number;
  size_bytes: number;
  columns: TableColumn[];
  indexes: string[];
}

export interface CatalogSchema {
  name: string;
  tables: CatalogTable[];
  views: CatalogTable[];
  functions: string[];
  sequences: string[];
}

export interface Catalog {
  schemas: CatalogSchema[];
}

export interface ResultSet {
  columns: { name: string; type: string }[];
  rows: (string | null)[][];
  truncated: boolean;
  command: string;
}

export interface QueryResponse {
  results: ResultSet[];
  read_only: boolean;
  duration_ms: number;
}

export interface Overview {
  databases: { name: string; size_bytes: number }[];
  activity: Backend[];
  replication_slots: ReplicationSlot[];
  settings: Record<string, string>;
}

export interface Backend {
  pid: number;
  user: string;
  database: string;
  application: string;
  client: string;
  state: string;
  wait_event: string;
  seconds: number;
  query: string;
}

export interface ReplicationSlot {
  name: string;
  plugin: string;
  database: string;
  active: boolean;
  retained_bytes: number;
}

export type Validation = "checksum" | "count" | "none";

export interface MigrationConfig {
  schemas: string[] | null;
  tables?: string[] | null;
  exclude_tables: string[] | null;
  snapshot_workers: number;
  fence_source: boolean;
  validation: Validation | "";
  keep_slot: boolean;
  cutover_timeout_seconds: number;
}

// The state object is empty until the job first runs, so every field is optional.
export interface MigrationState {
  slot?: string;
  publication?: string;
  schema_done?: boolean;
  snapshot_done?: boolean;
  indexing_done?: boolean;
  fenced?: boolean;
  replica_role?: boolean;
  consistent_lsn?: string;
  applied_lsn?: string;
  source_lsn?: string;
  lag_bytes?: number;
  tx_applied?: number;
  rows_applied?: number;
  anomalies?: number;
  last_commit_time?: string;
  source_version?: string;
  target_version?: string;
}

export type JobStatus = "draft" | "queued" | "running" | "paused" | "failed" | "completed" | "aborted";

export interface PreflightCheck {
  name: string;
  status: "ok" | "warn" | "fail";
  detail: string;
}

export interface PreflightReport {
  ok: boolean;
  checks: PreflightCheck[];
  tables: number;
}

export interface Migration {
  id: string;
  name: string;
  kind: string;
  source_id: string;
  target_id: string;
  config: Partial<MigrationConfig>;
  state: MigrationState;
  preflight?: PreflightReport;
  phase: string;
  status: JobStatus;
  desired: string;
  error: string;
  created_by: string;
  created_at: string;
  updated_at: string;
}

export interface JobTable {
  schema: string;
  table: string;
  status: string;
  est_rows: number;
  rows_copied: number;
  src_count: number;
  dst_count: number;
  validation: string;
  detail: string;
}

export interface JobEvent {
  id: number;
  time: string;
  level: string;
  message: string;
}

export interface MigrationDetail extends Migration {
  tables: JobTable[];
  events: JobEvent[];
}

export interface MigrationInput {
  name: string;
  source_id: string;
  target_id: string;
  config: MigrationConfig;
}

export type MigrationAction = "start" | "pause" | "resume" | "cutover" | "abort" | "delete";

export interface Backup {
  id: string;
  connection_id: string;
  connection_name: string;
  database: string;
  kind: string;
  server_version: string;
  status: "running" | "completed" | "failed";
  error: string;
  size_bytes: number;
  sha256: string;
  note: string;
  created_by: string;
  created_at: string;
  finished_at: string;
}

export interface Restore {
  id: string;
  backup_id: string;
  target_id: string;
  target_name: string;
  status: "running" | "completed" | "failed";
  error: string;
  log: string;
  created_by: string;
  created_at: string;
  finished_at: string;
}

export interface RestoreInput {
  target_id: string;
  clean: boolean;
  keep_owners: boolean;
  confirm: string;
}

export interface AuditEntry {
  id: number;
  time: string;
  actor: string;
  action: string;
  target: string;
  detail: string;
  ip: string;
  hash: string;
}

export interface AuditVerification {
  intact: boolean;
  checked: number;
  broken_at?: number;
}

export interface UserUpdate {
  name?: string;
  role?: Role;
  disabled?: boolean;
  password?: string;
}
