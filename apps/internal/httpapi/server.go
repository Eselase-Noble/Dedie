// Package httpapi exposes the engine over HTTP and serves the web interface.
// It is shared by the server and the desktop launcher. Handlers are thin:
// they decode a request, call the application layer and encode the result;
// authorization and auditing live in the engine.
package httpapi

import (
	"io/fs"
	"net/http"
	"strings"

	"dedie/engine/app"
)

// Options configure how the API treats its transport.
type Options struct {
	// Secure marks session cookies Secure and enables HSTS. Set it when the
	// listener uses TLS, or when a TLS-terminating proxy sits in front.
	Secure bool
	// TrustProxy takes the client address from X-Forwarded-For. Enable it
	// only behind a reverse proxy you control.
	TrustProxy bool
	// UI is the built web interface; nil serves the API alone.
	UI fs.FS
}

type Server struct {
	app    *app.App
	opts   Options
	logins *rateLimiter
}

// New builds the HTTP handler.
func New(a *app.App, opts Options) http.Handler {
	s := &Server{app: a, opts: opts, logins: newRateLimiter(10, 60)}
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/v1/setup", s.setupStatus)
	mux.HandleFunc("POST /api/v1/setup", s.setup)
	mux.HandleFunc("POST /api/v1/auth/login", s.login)
	mux.HandleFunc("POST /api/v1/auth/logout", s.authed(s.logout))
	mux.HandleFunc("GET /api/v1/auth/me", s.authed(s.me))
	mux.HandleFunc("POST /api/v1/auth/password", s.authed(s.changePassword))
	mux.HandleFunc("GET /api/v1/auth/sessions", s.authed(s.listSessions))
	mux.HandleFunc("DELETE /api/v1/auth/sessions/{id}", s.authed(s.revokeSession))
	mux.HandleFunc("POST /api/v1/auth/totp/begin", s.authed(s.beginTOTP))
	mux.HandleFunc("POST /api/v1/auth/totp/confirm", s.authed(s.confirmTOTP))
	mux.HandleFunc("POST /api/v1/auth/totp/disable", s.authed(s.disableTOTP))
	mux.HandleFunc("GET /api/v1/auth/tokens", s.authed(s.listTokens))
	mux.HandleFunc("POST /api/v1/auth/tokens", s.authed(s.createToken))
	mux.HandleFunc("DELETE /api/v1/auth/tokens/{id}", s.authed(s.revokeToken))
	mux.HandleFunc("GET /api/v1/auth/policy", s.authed(s.getPolicy))
	mux.HandleFunc("PUT /api/v1/auth/policy", s.authed(s.setPolicy))

	mux.HandleFunc("GET /api/v1/users", s.authed(s.listUsers))
	mux.HandleFunc("POST /api/v1/users", s.authed(s.createUser))
	mux.HandleFunc("PATCH /api/v1/users/{id}", s.authed(s.updateUser))

	mux.HandleFunc("GET /api/v1/connections", s.authed(s.listConnections))
	mux.HandleFunc("POST /api/v1/connections", s.authed(s.createConnection))
	mux.HandleFunc("POST /api/v1/connections/test", s.authed(s.testConnectionInput))
	mux.HandleFunc("GET /api/v1/connections/status", s.authed(s.connectionStatuses))
	mux.HandleFunc("GET /api/v1/connections/{id}", s.authed(s.getConnection))
	mux.HandleFunc("PUT /api/v1/connections/{id}", s.authed(s.updateConnection))
	mux.HandleFunc("DELETE /api/v1/connections/{id}", s.authed(s.deleteConnection))
	mux.HandleFunc("POST /api/v1/connections/{id}/test", s.authed(s.testConnection))
	mux.HandleFunc("GET /api/v1/connections/{id}/access", s.authed(s.getConnectionAccess))
	mux.HandleFunc("PUT /api/v1/connections/{id}/access", s.authed(s.setConnectionAccess))
	mux.HandleFunc("GET /api/v1/connections/{id}/catalog", s.authed(s.catalog))
	mux.HandleFunc("POST /api/v1/connections/{id}/query", s.authed(s.query))
	mux.HandleFunc("GET /api/v1/connections/{id}/overview", s.authed(s.overview))
	mux.HandleFunc("POST /api/v1/connections/{id}/backends/{pid}/cancel", s.authed(s.cancelBackend))
	mux.HandleFunc("POST /api/v1/connections/{id}/databases", s.authed(s.createDatabase))
	mux.HandleFunc("POST /api/v1/connections/{id}/schemas", s.authed(s.createSchema))
	mux.HandleFunc("POST /api/v1/connections/{id}/tables", s.authed(s.createTable))
	mux.HandleFunc("POST /api/v1/connections/{id}/rows", s.authed(s.changeRow))
	mux.HandleFunc("GET /api/v1/connections/{id}/server-objects", s.authed(s.serverObjects))
	mux.HandleFunc("POST /api/v1/connections/{id}/owner", s.authed(s.setOwner))

	mux.HandleFunc("GET /api/v1/connections/{id}/accounts", s.authed(s.databaseAccounts))
	mux.HandleFunc("POST /api/v1/connections/{id}/accounts", s.authed(s.saveDatabaseAccount))
	mux.HandleFunc("DELETE /api/v1/connections/{id}/accounts/{user}", s.authed(s.dropDatabaseAccount))
	mux.HandleFunc("POST /api/v1/connections/{id}/connection-limit", s.authed(s.setConnectionLimit))
	mux.HandleFunc("POST /api/v1/connections/{id}/sessions/terminate", s.authed(s.terminateSessions))
	mux.HandleFunc("GET /api/v1/connections/{id}/idle-policy", s.authed(s.getIdlePolicy))
	mux.HandleFunc("PUT /api/v1/connections/{id}/idle-policy", s.authed(s.setIdlePolicy))

	mux.HandleFunc("GET /api/v1/queries", s.authed(s.listSavedQueries))
	mux.HandleFunc("POST /api/v1/queries", s.authed(s.saveQuery))
	mux.HandleFunc("PUT /api/v1/queries/{id}", s.authed(s.saveQuery))
	mux.HandleFunc("DELETE /api/v1/queries/{id}", s.authed(s.deleteSavedQuery))

	mux.HandleFunc("GET /api/v1/migrations", s.authed(s.listMigrations))
	mux.HandleFunc("POST /api/v1/migrations", s.authed(s.createMigration))
	mux.HandleFunc("GET /api/v1/migrations/{id}", s.authed(s.getMigration))
	mux.HandleFunc("POST /api/v1/migrations/{id}/preflight", s.authed(s.preflightMigration))
	mux.HandleFunc("POST /api/v1/migrations/{id}/actions/{action}", s.authed(s.migrationAction))

	mux.HandleFunc("GET /api/v1/backups", s.authed(s.listBackups))
	mux.HandleFunc("POST /api/v1/backups", s.authed(s.createBackup))
	mux.HandleFunc("DELETE /api/v1/backups/{id}", s.authed(s.deleteBackup))
	mux.HandleFunc("POST /api/v1/backups/{id}/verify", s.authed(s.verifyBackup))
	mux.HandleFunc("POST /api/v1/backups/{id}/restore", s.authed(s.restoreBackup))
	mux.HandleFunc("GET /api/v1/backups/{id}/download", s.authed(s.downloadBackup))
	mux.HandleFunc("GET /api/v1/restores", s.authed(s.listRestores))
	mux.HandleFunc("GET /api/v1/wal-archives", s.authed(s.listWALArchives))
	mux.HandleFunc("PUT /api/v1/connections/{id}/wal-archive", s.authed(s.setWALArchive))
	mux.HandleFunc("GET /api/v1/backup-locations", s.authed(s.listLocations))
	mux.HandleFunc("POST /api/v1/backup-locations", s.authed(s.createLocation))
	mux.HandleFunc("DELETE /api/v1/backup-locations/{id}", s.authed(s.deleteLocation))
	mux.HandleFunc("GET /api/v1/schedules", s.authed(s.listSchedules))
	mux.HandleFunc("POST /api/v1/schedules", s.authed(s.saveSchedule))
	mux.HandleFunc("PUT /api/v1/schedules/{id}", s.authed(s.saveSchedule))
	mux.HandleFunc("DELETE /api/v1/schedules/{id}", s.authed(s.deleteSchedule))
	mux.HandleFunc("POST /api/v1/schedules/{id}/run", s.authed(s.runSchedule))

	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("GET /metrics", s.authed(s.metrics))
	mux.HandleFunc("GET /api/v1/monitoring", s.authed(s.monitoring))
	mux.HandleFunc("GET /api/v1/alerts/events", s.authed(s.listAlertEvents))
	mux.HandleFunc("GET /api/v1/alerts/channels", s.authed(s.listAlertChannels))
	mux.HandleFunc("POST /api/v1/alerts/channels", s.authed(s.saveAlertChannel))
	mux.HandleFunc("PUT /api/v1/alerts/channels/{id}", s.authed(s.saveAlertChannel))
	mux.HandleFunc("DELETE /api/v1/alerts/channels/{id}", s.authed(s.deleteAlertChannel))
	mux.HandleFunc("POST /api/v1/alerts/channels/{id}/test", s.authed(s.testAlertChannel))

	mux.HandleFunc("GET /api/v1/audit", s.authed(s.listAudit))
	mux.HandleFunc("GET /api/v1/audit/verify", s.authed(s.verifyAudit))

	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "no such endpoint")
	})
	if opts.UI != nil {
		mux.Handle("/", uiHandler(opts.UI))
	}
	return s.guard(mux)
}

// uiHandler serves the exported web interface. Unknown paths get the
// exporter's 404 page rather than a bare error.
func uiHandler(ui fs.FS) http.Handler {
	files := http.FileServer(http.FS(ui))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if path == "/" {
			path = "/index.html"
		}
		name := strings.TrimSuffix(path[1:], "/")
		if _, err := fs.Stat(ui, name); err != nil {
			if _, err := fs.Stat(ui, name+"/index.html"); err != nil {
				if page, err := fs.ReadFile(ui, "404.html"); err == nil {
					w.Header().Set("Content-Type", "text/html; charset=utf-8")
					w.WriteHeader(http.StatusNotFound)
					w.Write(page)
					return
				}
				http.NotFound(w, r)
				return
			}
		}
		files.ServeHTTP(w, r)
	})
}
