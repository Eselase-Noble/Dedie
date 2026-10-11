import type {
  AuditEntry, AuditVerification, Backup, Catalog, Connection, ConnectionInput, Migration, MigrationAction,
  MigrationDetail, MigrationInput, Overview, PreflightReport, QueryResponse, Restore, RestoreInput, ServerInfo,
  Session, User, UserUpdate,
} from "./types";

const BASE = "/api/v1";

/** An error response from the API, carrying the HTTP status. */
export class ApiError extends Error {
  status: number;
  constructor(status: number, message: string) {
    super(message);
    this.status = status;
  }
}

interface RequestOptions {
  body?: unknown;
  // The sign-in page handles 401 itself; everywhere else an expired session
  // sends the user back to it.
  noRedirect?: boolean;
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
    });
  } catch {
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
    const message = (data as { error?: string } | null)?.error ?? `Request failed (${res.status})`;
    throw new ApiError(res.status, message);
  }
  return data as T;
}

const get = <T>(path: string, opts?: RequestOptions) => request<T>("GET", path, opts);
const post = <T>(path: string, body?: unknown, opts?: RequestOptions) => request<T>("POST", path, { ...opts, body });

const id = encodeURIComponent;

export const api = {
  // Setup and session
  setupStatus: () => get<{ needs_setup: boolean }>("/setup", { noRedirect: true }),
  setup: (body: { organization: string; name: string; email: string; password: string }) =>
    post<Session>("/setup", body, { noRedirect: true }),
  login: (email: string, password: string) => post<Session>("/auth/login", { email, password }, { noRedirect: true }),
  logout: () => post<{ ok: boolean }>("/auth/logout"),
  me: (noRedirect = false) => get<Session>("/auth/me", { noRedirect }),

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
  catalog: (connId: string) => get<Catalog>(`/connections/${id(connId)}/catalog`),
  query: (connId: string, body: { sql: string; write: boolean; max_rows: number }) =>
    post<QueryResponse>(`/connections/${id(connId)}/query`, body),
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
  createBackup: (connection_id: string, note: string) => post<Backup>("/backups", { connection_id, note }),
  deleteBackup: (backupId: string) => request<{ ok: boolean }>("DELETE", `/backups/${id(backupId)}`),
  verifyBackup: (backupId: string) => post<{ ok: boolean }>(`/backups/${id(backupId)}/verify`),
  restoreBackup: (backupId: string, body: RestoreInput) => post<Restore>(`/backups/${id(backupId)}/restore`, body),
  restores: () => get<Restore[]>("/restores"),

  // Audit
  audit: (before: number, limit: number) => get<AuditEntry[]>(`/audit?before=${before}&limit=${limit}`),
  verifyAudit: () => get<AuditVerification>("/audit/verify"),
};

/** Extracts a displayable message from anything thrown. */
export function errorMessage(err: unknown): string {
  if (err instanceof Error) return err.message;
  return "Something went wrong.";
}
