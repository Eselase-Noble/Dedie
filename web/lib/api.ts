import type {
  AccountChange,
  AlertChannel,
  ApiToken,
  AuditEntry,
  AuditVerification,
  Backup,
  BackupInput,
  Catalog,
  ChannelInput,
  ColumnDef,
  Connection,
  ConnectionAccess,
  ConnectionInput,
  ConnectionStatus,
  DbAccounts,
  Migration,
  MigrationAction,
  MigrationDetail,
  MigrationInput,
  Monitoring,
  Overview,
  OwnerChange,
  PreflightReport,
  QueryResponse,
  Restore,
  RestoreInput,
  RowChange,
  SavedQuery,
  Schedule,
  ScheduleInput,
  SecurityPolicy,
  ServerInfo,
  ServerObjects,
  Session,
  SessionFilter,
  SessionInfo,
  User,
  UserUpdate,
} from "./types";

const BASE = "/api/v1";

/** An error response from the API, carrying the HTTP status. */
export class ApiError extends Error {
  status: number;
  /** A machine-readable reason, set for conditions the interface reacts to. */
  code: string;
  constructor(status: number, message: string, code = "") {
    super(message);
    this.status = status;
    this.code = code;
  }
}

interface RequestOptions {
  body?: unknown;
  // The sign-in page handles 401 itself; everywhere else an expired session
  // sends the user back to it.
  noRedirect?: boolean;
  // Aborting the signal abandons the request; the server then cancels the
  // statement it was running for it.
  signal?: AbortSignal;
}

async function request<T>(method: string, path: string, opts: RequestOptions = {}): Promise<T> {
  const headers: Record<string, string> = { Accept: "application/json" };
  if (method !== "GET") {
    // The server rejects state-changing requests without this header; a
    // cross-site form or script cannot add it.
    headers["X-Dedie-Request"] = "1";
    headers["Content-Type"] = "application/json";
  }
  let res: Response;
  try {
    res = await fetch(BASE + path, {
      method,
      headers,
      credentials: "same-origin",
      body: method === "GET" ? undefined : JSON.stringify(opts.body ?? {}),
      signal: opts.signal,
    });
  } catch (err) {
    if (err instanceof DOMException && err.name === "AbortError") throw new ApiError(0, "Cancelled.", "cancelled");
    throw new ApiError(0, "The server could not be reached.");
  }
  let data: unknown = null;
  try {
    data = await res.json();
  } catch {
    // Non-JSON bodies only occur on unexpected failures.
  }
  if (!res.ok) {
    if (res.status === 401 && !opts.noRedirect && typeof window !== "undefined") {
      window.location.href = "/login/";
    }
    const body = data as { error?: string; code?: string } | null;
    // An organization that requires two-factor sign-in confines a user
    // who has not set it up to the account page.
    if (body?.code === "totp_enrollment_required" && typeof window !== "undefined" && !window.location.pathname.startsWith("/account")) {
      window.location.href = "/account/";
    }
    throw new ApiError(res.status, body?.error ?? `Request failed (${res.status})`, body?.code ?? "");
  }
  return data as T;
}

const get = <T>(path: string, opts?: RequestOptions) => request<T>("GET", path, opts);
const post = <T>(path: string, body?: unknown, opts?: RequestOptions) => request<T>("POST", path, { ...opts, body });

const id = encodeURIComponent;
const inDatabase = (database: string) => (database ? `?database=${encodeURIComponent(database)}` : "");

export const api = {
  // Setup and session
  setupStatus: () => get<{ needs_setup: boolean }>("/setup", { noRedirect: true }),
  setup: (body: { organization: string; name: string; email: string; password: string }) =>
    post<Session>("/setup", body, { noRedirect: true }),
  login: (email: string, password: string, code = "") =>
    post<Session>("/auth/login", { email, password, code }, { noRedirect: true }),
  logout: () => post<{ ok: boolean }>("/auth/logout"),
  me: (noRedirect = false) => get<Session>("/auth/me", { noRedirect }),

  // The signed-in user's own account
  changePassword: (current: string, next: string) => post<{ ok: boolean }>("/auth/password", { current, new: next }),
  sessions: () => get<SessionInfo[]>("/auth/sessions"),
  revokeSession: (sessionId: string) => request<{ ok: boolean }>("DELETE", `/auth/sessions/${id(sessionId)}`),
  beginTotp: () => post<{ secret: string; uri: string }>("/auth/totp/begin"),
  confirmTotp: (code: string) => post<{ recovery_codes: string[] }>("/auth/totp/confirm", { code }),
  disableTotp: (password: string) => post<{ ok: boolean }>("/auth/totp/disable", { password }),
  tokens: () => get<ApiToken[]>("/auth/tokens"),
  createToken: (body: { name: string; role: string; days: number }) => post<ApiToken>("/auth/tokens", body),
  revokeToken: (tokenId: string) => request<{ ok: boolean }>("DELETE", `/auth/tokens/${id(tokenId)}`),
  policy: () => get<SecurityPolicy>("/auth/policy"),
  setPolicy: (body: SecurityPolicy) => request<SecurityPolicy>("PUT", "/auth/policy", { body }),

  // Users
  users: () => get<User[]>("/users"),
  createUser: (body: { name: string; email: string; password: string; role: string }) => post<User>("/users", body),
  updateUser: (userId: string, body: UserUpdate) => request<User>("PATCH", `/users/${id(userId)}`, { body }),

  // Connections
  connections: () => get<Connection[]>("/connections"),
  createConnection: (body: ConnectionInput) => post<Connection>("/connections", body),
  updateConnection: (connId: string, body: ConnectionInput) =>
    request<Connection>("PUT", `/connections/${id(connId)}`, { body }),
  deleteConnection: (connId: string) => request<{ ok: boolean }>("DELETE", `/connections/${id(connId)}`),
  testConnection: (connId: string) => post<ServerInfo>(`/connections/${id(connId)}/test`),
  testConnectionInput: (body: ConnectionInput) => post<ServerInfo>("/connections/test", body),
  connectionAccess: (connId: string) => get<ConnectionAccess>(`/connections/${id(connId)}/access`),
  setConnectionAccess: (connId: string, body: ConnectionAccess) =>
    request<{ ok: boolean }>("PUT", `/connections/${id(connId)}/access`, { body }),
  connectionStatuses: () => get<Record<string, ConnectionStatus>>("/connections/status"),
  createDatabase: (connId: string, name: string, addConnection: boolean) =>
    post<{ ok: boolean; connection: Connection | null }>(`/connections/${id(connId)}/databases`, { name, add_connection: addConnection }),
  // `database` names another database on the same server; empty means the
  // connection's own.
  createSchema: (connId: string, name: string, database = "") =>
    post<{ ok: boolean }>(`/connections/${id(connId)}/schemas${inDatabase(database)}`, { name }),
  createTable: (connId: string, body: { schema: string; name: string; columns: ColumnDef[] }, database = "") =>
    post<{ ok: boolean }>(`/connections/${id(connId)}/tables${inDatabase(database)}`, body),
  serverObjects: (connId: string) => get<ServerObjects>(`/connections/${id(connId)}/server-objects`),
  setOwner: (connId: string, body: OwnerChange, database = "") =>
    post<{ ok: boolean }>(`/connections/${id(connId)}/owner${inDatabase(database)}`, body),
  catalog: (connId: string) => get<Catalog>(`/connections/${id(connId)}/catalog`),
  query: (connId: string, body: { sql: string; write: boolean; max_rows: number }, signal?: AbortSignal) =>
    post<QueryResponse>(`/connections/${id(connId)}/query`, body, { signal }),
  changeRow: (connId: string, body: RowChange) => post<{ ok: boolean }>(`/connections/${id(connId)}/rows`, body),

  // Users and connections of a database server
  dbAccounts: (connId: string) => get<DbAccounts>(`/connections/${id(connId)}/accounts`),
  saveDbAccount: (connId: string, body: AccountChange) => post<{ ok: boolean }>(`/connections/${id(connId)}/accounts`, body),
  dropDbAccount: (connId: string, user: string, host: string, handOver: boolean) =>
    request<{ ok: boolean }>("DELETE", `/connections/${id(connId)}/accounts/${id(user)}?host=${id(host)}${handOver ? "&hand_over=1" : ""}`),
  setConnectionLimit: (connId: string, body: { scope: "account" | "database"; name: string; host: string; limit: number }) =>
    post<{ ok: boolean }>(`/connections/${id(connId)}/connection-limit`, body),
  terminateSessions: (connId: string, filter: SessionFilter) =>
    post<{ sessions: number }>(`/connections/${id(connId)}/sessions/terminate`, filter),

  // Saved queries
  savedQueries: () => get<SavedQuery[]>("/queries"),
  saveQuery: (queryId: string, body: { name: string; sql: string; kind: string; shared: boolean }) =>
    queryId ? request<SavedQuery>("PUT", `/queries/${id(queryId)}`, { body }) : post<SavedQuery>("/queries", body),
  deleteSavedQuery: (queryId: string) => request<{ ok: boolean }>("DELETE", `/queries/${id(queryId)}`),
  overview: (connId: string) => get<Overview>(`/connections/${id(connId)}/overview`),
  cancelBackend: (connId: string, pid: number, terminate: boolean) =>
    post<{ ok: boolean }>(`/connections/${id(connId)}/backends/${pid}/cancel${terminate ? "?terminate=1" : ""}`),

  // Migrations
  migrations: () => get<Migration[]>("/migrations"),
  createMigration: (body: MigrationInput) => post<Migration>("/migrations", body),
  migration: (jobId: string) => get<MigrationDetail>(`/migrations/${id(jobId)}`),
  preflight: (jobId: string) => post<PreflightReport>(`/migrations/${id(jobId)}/preflight`),
  migrationAction: (jobId: string, action: MigrationAction) =>
    post<{ ok: boolean }>(`/migrations/${id(jobId)}/actions/${action}`),

  // Backups
  backups: () => get<Backup[]>("/backups"),
  createBackup: (body: BackupInput) => post<Backup>("/backups", body),
  // A plain link: the browser downloads the decrypted archive as a file.
  backupDownloadUrl: (backupId: string) => `${BASE}/backups/${id(backupId)}/download`,
  schedules: () => get<Schedule[]>("/schedules"),
  saveSchedule: (scheduleId: string, body: ScheduleInput) =>
    scheduleId ? request<Schedule>("PUT", `/schedules/${id(scheduleId)}`, { body }) : post<Schedule>("/schedules", body),
  deleteSchedule: (scheduleId: string) => request<{ ok: boolean }>("DELETE", `/schedules/${id(scheduleId)}`),
  runSchedule: (scheduleId: string) => post<{ ok: boolean }>(`/schedules/${id(scheduleId)}/run`),
  deleteBackup: (backupId: string) => request<{ ok: boolean }>("DELETE", `/backups/${id(backupId)}`),
  verifyBackup: (backupId: string) => post<{ ok: boolean }>(`/backups/${id(backupId)}/verify`),
  restoreBackup: (backupId: string, body: RestoreInput) => post<Restore>(`/backups/${id(backupId)}/restore`, body),
  restores: () => get<Restore[]>("/restores"),

  // Monitoring and alerts
  monitoring: () => get<Monitoring>("/monitoring"),
  alertChannels: () => get<AlertChannel[]>("/alerts/channels"),
  saveAlertChannel: (channelId: string, body: ChannelInput) =>
    channelId ? request<AlertChannel>("PUT", `/alerts/channels/${id(channelId)}`, { body }) : post<AlertChannel>("/alerts/channels", body),
  deleteAlertChannel: (channelId: string) => request<{ ok: boolean }>("DELETE", `/alerts/channels/${id(channelId)}`),
  testAlertChannel: (channelId: string) => post<{ ok: boolean }>(`/alerts/channels/${id(channelId)}/test`),

  // Audit
  audit: (before: number, limit: number) => get<AuditEntry[]>(`/audit?before=${before}&limit=${limit}`),
  verifyAudit: () => get<AuditVerification>("/audit/verify"),
};

/** Extracts a displayable message from anything thrown. */
export function errorMessage(err: unknown): string {
  if (err instanceof Error) return err.message;
  return "Something went wrong.";
}
