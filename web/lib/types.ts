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
  | "audit.view"
  | "alerts.manage";

export type Role = "owner" | "admin" | "operator" | "analyst" | "viewer";

export const ROLES: Role[] = ["owner", "admin", "operator", "analyst", "viewer"];

/** One column in the table designer. */
export interface ColumnDef {
  name: string;
  type: string;
  nullable: boolean;
  primary_key: boolean;
  default: string;
}

/** Whether a connection's server answered the last check. */
export interface ConnectionStatus {
  online: boolean;
  version?: string;
  encrypted: boolean;
  latency_ms: number;
  error?: string;
  checked_at: string;
}

export interface User {
  id: string;
  org_id: string;
  email: string;
  name: string;
  role: Role;
  disabled: boolean;
  totp_enabled: boolean;
  created_at: string;
}

export interface Session {
  user: User;
  organization: string;
  permissions: Permission[];
  kinds: string[];
  must_enroll_totp: boolean;
}

export interface SecurityPolicy {
  password_min_length: number;
  password_require_mixed: boolean;
  session_hours: number;
  require_totp: boolean;
}

export interface SessionInfo {
  id: string;
  ip: string;
  user_agent: string;
  created_at: string;
  last_seen_at: string;
  expires_at: string;
  current: boolean;
}

export interface ApiToken {
  id: string;
  user_id: string;
  user_email: string;
  name: string;
  role: Role;
  expires_at: string;
  last_used_at: string;
  created_at: string;
  /** Present only in the response that creates the token. */
  token?: string;
}

export interface ConnectionAccess {
  restricted: boolean;
  user_ids: string[];
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
  restricted: boolean;
  environment: Environment;
  created_by: string;
  created_at: string;
}

/** Where a database sits. Empty when nobody has said. */
export type Environment = "" | "production" | "staging" | "development";

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
  environment: Environment;
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
  owner?: string;
  est_rows: number;
  size_bytes: number;
  columns: TableColumn[];
  indexes: string[];
}

export interface CatalogSchema {
  name: string;
  owner?: string;
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
  /** MongoDB sources: fields lifted into typed columns, by collection. */
  columns?: Record<string, ColumnSpec[]>;
  /** "sync" keeps the target following the source; anything else ends with a cutover. */
  mode?: "migrate" | "sync" | "offline" | "";
  schemas: string[] | null;
  tables?: string[] | null;
  exclude_tables: string[] | null;
  snapshot_workers: number;
  fence_source: boolean;
  validation: Validation | "";
  keep_slot: boolean;
  on_existing?: OnExisting | "";
  /** Between two kinds of database: what is done with stored procedures, functions and triggers. */
  code?: CodeMode | "";
  cutover_timeout_seconds: number;
}

// The state object is empty until the job first runs, so every field is optional.
export interface MigrationState {
  /** Reported by sources that measure lag in time rather than bytes. */
  lag_seconds?: number;
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
  /** Once a job between two kinds of database has finished: what it did with views and stored code. */
  objects?: ObjectSummary;
}

/** Counts of the views and stored code of a finished migration. */
export interface ObjectSummary {
  /** On the target, and nothing to do. */
  created: number;
  /** On the target, but to be checked by a person. */
  review: number;
  /** Not on the target: left for the administrator. */
  left: number;
}

export type JobStatus = "draft" | "queued" | "running" | "paused" | "failed" | "completed" | "aborted";

export interface PreflightCheck {
  name: string;
  status: "ok" | "warn" | "fail";
  detail: string;
}

/** One statement a migration would run on the target. */
export interface PreviewStatement {
  phase: string;
  kind: string;
  name: string;
  sql: string;
  /** What closes the statement in a script when a semicolon does not. */
  end?: string;
}

/** An object of the source that a migration does not carry over. */
export interface Leftover {
  kind: string;
  name: string;
  /** Why it is not migrated. */
  reason: string;
  /** The statement that defines it on the source; empty if the source would not show it. */
  source: string;
  /** What closes the statement in a script when a semicolon does not. */
  end?: string;
  /**
   * For stored code a conversion was tried on: "review", "unconverted",
   * "not created" or "created, needs review".
   */
  status?: string;
  /** What to check, or why the routine could not be converted, by line of the source. */
  notes?: string[];
  /** The converted routine as a script in the target's dialect, where there is one. */
  draft?: string;
}

/** Something on the target with the name of an object being migrated. */
export interface ExistingObject {
  kind: string;
  name: string;
}

/** What a migration does about objects the target already has. */
export type OnExisting = "fail" | "skip" | "replace";

/** What a migration between two kinds of database does with stored code. */
export type CodeMode = "converted" | "review" | "off";

export interface PreflightReport {
  ok: boolean;
  checks: PreflightCheck[];
  tables: number;
  existing?: ExistingObject[];
}

/** A job's initial copy summed over its tables. Listed jobs carry it while they copy. */
export interface CopyProgress {
  tables: number;
  tables_copied: number;
  rows_copied: number;
  /** The source's own estimate, which can be zero or out of date. */
  est_rows: number;
  /** Actual counts for the tables already copied, estimates for the rest. */
  rows_expected: number;
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
  copy?: CopyProgress;
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

export type BackupType = "full" | "schema" | "data" | "tables" | "physical";

export interface BackupInput {
  connection_id: string;
  note: string;
  type: BackupType;
  tables: string[];
  level: BackupLevel;
  location_id: string;
  /** Write the database's own format without encryption. */
  plain: boolean;
}

export interface Schedule {
  id: string;
  name: string;
  connection_id: string;
  type: BackupType;
  tables: string[];
  level: BackupLevel;
  location_id: string;
  encrypted: boolean;
  cron: string;
  keep_last: number;
  keep_days: number;
  verify: boolean;
  enabled: boolean;
  last_run_at: string;
  last_status: string;
  last_error: string;
  next_run_at: string;
  created_by: string;
  created_at: string;
}

export type ScheduleInput = Pick<Schedule, "name" | "connection_id" | "type" | "tables" | "level" | "location_id" | "cron" | "keep_last" | "keep_days" | "verify" | "enabled"> & { plain: boolean };

export interface Backup {
  id: string;
  connection_id: string;
  connection_name: string;
  database: string;
  kind: string;
  type: BackupType;
  format: string;
  tables: string[];
  level: BackupLevel;
  parent_id: string;
  encrypted: boolean;
  location_id: string;
  /** The file on the machine that runs Dedie. */
  path: string;
  schedule_id: string;
  verified_at: string;
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
  database: string;
  status: "running" | "completed" | "failed";
  error: string;
  log: string;
  created_by: string;
  created_at: string;
  finished_at: string;
}

export interface RestoreInput {
  target_id: string;
  new_database: string;
  tables: string[];
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
  reset_totp?: boolean;
}

export interface HealthSample {
  time: string;
  online: boolean;
  latency_ms: number;
  error?: string;
}

export interface ConnectionHealth {
  connection_id: string;
  name: string;
  kind: string;
  environment: Environment;
  checks: number;
  uptime_percent: number;
  avg_latency_ms: number;
  last: HealthSample | null;
  samples: HealthSample[];
}

export interface MigrationHealth {
  id: string;
  name: string;
  status: string;
  phase: string;
  lag_bytes: number;
  rows_applied: number;
  last_commit_time: string;
}

export interface AlertEvent {
  id: number;
  time: string;
  type: string;
  subject: string;
  message: string;
}

export interface Monitoring {
  connections: ConnectionHealth[];
  migrations: MigrationHealth[];
  events: AlertEvent[];
}

export type ChannelKind = "webhook" | "slack" | "email";

export interface AlertChannel {
  id: string;
  name: string;
  kind: ChannelKind;
  summary: string;
  events: string[];
  enabled: boolean;
  last_sent_at: string;
  last_error: string;
  created_by: string;
  created_at: string;
}

export interface ChannelConfig {
  url?: string;
  secret?: string;
  smtp_host?: string;
  smtp_port?: number;
  username?: string;
  password?: string;
  from?: string;
  to?: string[];
}

export interface ChannelInput {
  name: string;
  kind: ChannelKind;
  config: ChannelConfig;
  events: string[];
  enabled: boolean;
}

export interface RowChange {
  schema: string;
  table: string;
  action: "insert" | "update" | "delete";
  key?: Record<string, string | null>;
  values?: Record<string, string | null>;
}

export interface SavedQuery {
  id: string;
  user_id: string;
  owner: string;
  name: string;
  sql: string;
  kind: string;
  shared: boolean;
  created_at: string;
  updated_at: string;
}

export interface ServerObjects {
  databases: { name: string; owner?: string }[];
  roles: string[];
  /** Whether objects have owners on this kind of server. */
  ownership: boolean;
}

export interface OwnerChange {
  kind: "database" | "schema" | "table";
  schema: string;
  name: string;
  owner: string;
}

export interface DbAccount {
  name: string;
  /** Client host part of a MySQL account. */
  host?: string;
  can_login: boolean;
  superuser: boolean;
  create_db: boolean;
  create_role: boolean;
  replication: boolean;
  /** -1 means no limit. */
  connection_limit: number;
  connections: number;
  active: number;
  idle: number;
  idle_in_transaction: number;
  oldest_idle_seconds: number;
  member_of: string[];
  valid_until?: string;
}

export interface ConnectionUsage {
  user: string;
  application: string;
  client: string;
  database: string;
  total: number;
  active: number;
  idle: number;
  idle_in_transaction: number;
  longest_seconds: number;
}

export interface DbAccounts {
  accounts: DbAccount[];
  roles: string[];
  max_connections: number;
  used_connections: number;
  reserved_connections: number;
  databases: { name: string; connection_limit: number; connections: number }[];
  usage: ConnectionUsage[];
  database_limits: boolean;
}

export interface AccountChange {
  create: boolean;
  name: string;
  host: string;
  password?: string | null;
  can_login?: boolean | null;
  connection_limit?: number | null;
  grant: string[];
  revoke: string[];
  access: "" | "readonly" | "readwrite";
}

export interface SessionFilter {
  /** Only sessions that are not working: "idle" or "idle in transaction". */
  state?: "" | "idle" | "idle in transaction";
  /** With state: only sessions in that state at least this long. */
  idle_seconds?: number;
  user?: string;
  application?: string;
  database?: string;
  client?: string;
}

export interface IdlePolicy {
  connection_id: string;
  enabled: boolean;
  idle_minutes: number;
  tx_minutes: number;
  min_usage_percent: number;
  exclude_users: string[];
  last_run_at: string;
  last_closed: number;
  total_closed: number;
  last_error: string;
  updated_by: string;
  updated_at: string;
}

export type BackupLevel = "full" | "differential" | "incremental";

export interface BackupLocation {
  id: string;
  name: string;
  kind: "dir" | "s3";
  /** The directory, or a description of the bucket without credentials. */
  path: string;
  created_by: string;
  created_at: string;
  backups: number;
}

export interface BackupLocations {
  default_path: string;
  locations: BackupLocation[];
}

export interface LocationInput {
  name: string;
  kind: "dir" | "s3";
  path: string;
  s3: { endpoint: string; region: string; bucket: string; prefix: string; access_key: string; secret_key: string };
}

export interface WalArchive {
  connection_id: string;
  connection_name: string;
  enabled: boolean;
  dir: string;
  slot: string;
  retention_days: number;
  started_at: string;
  last_error: string;
  receiving: boolean;
  segments: number;
  size_bytes: number;
  newest_at: string;
  /** Zero until a physical backup has been taken since archiving began. */
  earliest_recovery: string;
}

export interface ColumnSpec {
  /** Path of the field, with dots for nesting. */
  field: string;
  name?: string;
  type: string;
}

export type NotificationLevel = "info" | "success" | "warning" | "error";
export type NotificationTopic = "migration" | "backup" | "monitoring" | "security";

/** Something that happened which the signed-in user should know about. */
export interface AppNotification {
  id: number;
  topic: NotificationTopic;
  /** What happened, for example "migration.completed". */
  kind: string;
  level: NotificationLevel;
  title: string;
  body: string;
  /** The page inside the application where the user can act. */
  link: string;
  created_at: string;
  /** Whether this user has read it; others have their own state. */
  read: boolean;
}

export interface NotificationList {
  unread: number;
  items: AppNotification[];
}

export interface NotificationQuery {
  unread?: boolean;
  topic?: NotificationTopic | "";
  /** Returns notifications older than this id. */
  before?: number;
  limit?: number;
}
