package postgres

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"golang.org/x/crypto/pbkdf2"

	"dedie/engine/connector"
)

// Accounts lists roles with their limits and current connections, the
// server's and each database's connection budget, and who holds
// connections now.
func (s *session) Accounts(ctx context.Context) (*connector.Accounts, error) {
	out := &connector.Accounts{Accounts: []connector.Account{}, Roles: []string{}, Databases: []connector.DatabaseLimit{},
		Usage: []connector.ConnectionUsage{}, DatabaseLimits: true}

	// Roles named pg_* are built in and are not managed here.
	rows, err := s.conn.Query(ctx, `SELECT r.rolname, r.rolcanlogin, r.rolsuper, r.rolcreatedb, r.rolcreaterole, r.rolreplication,
			r.rolconnlimit, coalesce(to_char(r.rolvaliduntil, 'YYYY-MM-DD HH24:MI TZ'), ''),
			ARRAY(SELECT b.rolname::text FROM pg_auth_members m JOIN pg_roles b ON b.oid = m.roleid WHERE m.member = r.oid ORDER BY 1),
			coalesce(a.total, 0), coalesce(a.active, 0), coalesce(a.idle, 0), coalesce(a.idle_tx, 0), coalesce(a.oldest, 0)
		FROM pg_roles r LEFT JOIN (
			SELECT usename, count(*) AS total, count(*) FILTER (WHERE state = 'active') AS active,
				count(*) FILTER (WHERE state = 'idle') AS idle,
				count(*) FILTER (WHERE state LIKE 'idle in transaction%') AS idle_tx,
				coalesce(max(extract(epoch FROM now() - state_change)) FILTER (WHERE state LIKE 'idle%'), 0)::bigint AS oldest
			FROM pg_stat_activity WHERE backend_type = 'client backend' GROUP BY usename) a ON a.usename = r.rolname
		WHERE r.rolname !~ '^pg_' ORDER BY r.rolcanlogin DESC, r.rolname`)
	if err != nil {
		return nil, friendly(err)
	}
	for rows.Next() {
		var a connector.Account
		if err := rows.Scan(&a.Name, &a.CanLogin, &a.Superuser, &a.CreateDB, &a.CreateRole, &a.Replication,
			&a.ConnectionLimit, &a.ValidUntil, &a.MemberOf, &a.Connections, &a.Active, &a.Idle, &a.IdleInTransaction, &a.OldestIdleSeconds); err != nil {
			return nil, err
		}
		if a.MemberOf == nil {
			a.MemberOf = []string{}
		}
		out.Accounts = append(out.Accounts, a)
		out.Roles = append(out.Roles, a.Name)
	}
	if err := rows.Err(); err != nil {
		return nil, friendly(err)
	}

	if err := s.conn.QueryRow(ctx, `SELECT current_setting('max_connections')::int, current_setting('superuser_reserved_connections')::int,
			(SELECT count(*) FROM pg_stat_activity WHERE backend_type = 'client backend')`).Scan(&out.MaxConnections, &out.Reserved, &out.Used); err != nil {
		return nil, friendly(err)
	}

	rows, err = s.conn.Query(ctx, `SELECT d.datname, d.datconnlimit, coalesce(s.numbackends, 0)
		FROM pg_database d LEFT JOIN pg_stat_database s ON s.datid = d.oid WHERE NOT d.datistemplate ORDER BY 1`)
	if err != nil {
		return nil, friendly(err)
	}
	for rows.Next() {
		var d connector.DatabaseLimit
		if err := rows.Scan(&d.Name, &d.ConnectionLimit, &d.Connections); err != nil {
			return nil, err
		}
		out.Databases = append(out.Databases, d)
	}
	if err := rows.Err(); err != nil {
		return nil, friendly(err)
	}

	rows, err = s.conn.Query(ctx, `SELECT coalesce(usename, ''), coalesce(application_name, ''), coalesce(host(client_addr), 'local'), coalesce(datname, ''),
			count(*), count(*) FILTER (WHERE state = 'active'), count(*) FILTER (WHERE state = 'idle'),
			count(*) FILTER (WHERE state LIKE 'idle in transaction%'),
			coalesce(max(extract(epoch FROM now() - state_change))::bigint, 0)
		FROM pg_stat_activity WHERE backend_type = 'client backend'
		GROUP BY 1, 2, 3, 4 ORDER BY 5 DESC, 1, 2`)
	if err != nil {
		return nil, friendly(err)
	}
	for rows.Next() {
		var u connector.ConnectionUsage
		if err := rows.Scan(&u.User, &u.Application, &u.Client, &u.Database, &u.Total, &u.Active, &u.Idle, &u.IdleInTransaction, &u.LongestSeconds); err != nil {
			return nil, err
		}
		out.Usage = append(out.Usage, u)
	}
	return out, friendly(rows.Err())
}

// scramVerifier computes the SCRAM-SHA-256 verifier PostgreSQL stores for
// a password. Sending the verifier instead of the password means the
// password itself never reaches the server, its logs or its statement
// statistics.
func scramVerifier(password string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	const iterations = 4096
	salted := pbkdf2.Key([]byte(password), salt, iterations, sha256.Size, sha256.New)
	mac := func(key []byte, msg string) []byte {
		h := hmac.New(sha256.New, key)
		h.Write([]byte(msg))
		return h.Sum(nil)
	}
	stored := sha256.Sum256(mac(salted, "Client Key"))
	b64 := base64.StdEncoding.EncodeToString
	return fmt.Sprintf("SCRAM-SHA-256$%d:%s$%s:%s", iterations, b64(salt), b64(stored[:]), b64(mac(salted, "Server Key"))), nil
}

// passwordClause renders PASSWORD '...' for CREATE or ALTER ROLE.
func passwordClause(password string) (string, error) {
	secret := password
	// The verifier is defined over the password after Unicode
	// normalization. Plain ASCII needs none, so only then is it computed
	// here; anything else is left for the server to hash.
	ascii := true
	for _, r := range password {
		if r < 0x20 || r > 0x7e {
			ascii = false
		}
	}
	if ascii {
		v, err := scramVerifier(password)
		if err != nil {
			return "", err
		}
		secret = v
	}
	return " PASSWORD " + literal(secret), nil
}

// literal quotes a string for a statement that cannot take parameters.
// The E” form has the same meaning whatever standard_conforming_strings
// is set to.
func literal(v string) string {
	return "E'" + strings.NewReplacer(`\`, `\\`, `'`, `''`).Replace(v) + "'"
}

func ident(name string) string { return pgx.Identifier{name}.Sanitize() }

// SaveAccount creates a role or changes one, all in one transaction.
func (s *session) SaveAccount(ctx context.Context, c connector.AccountChange) error {
	if err := c.Validate(); err != nil {
		return err
	}
	name := ident(c.Name)
	var options string
	if c.CanLogin != nil {
		options += map[bool]string{true: " LOGIN", false: " NOLOGIN"}[*c.CanLogin]
	}
	if c.ConnectionLimit != nil {
		options += fmt.Sprintf(" CONNECTION LIMIT %d", *c.ConnectionLimit)
	}
	if c.Password != nil && *c.Password != "" {
		clause, err := passwordClause(*c.Password)
		if err != nil {
			return err
		}
		options += clause
	}
	var stmts []string
	switch {
	case c.Create:
		stmts = append(stmts, "CREATE ROLE "+name+options)
	case options != "":
		stmts = append(stmts, "ALTER ROLE "+name+options)
	}
	for _, r := range c.Grant {
		stmts = append(stmts, "GRANT "+ident(r)+" TO "+name)
	}
	for _, r := range c.Revoke {
		stmts = append(stmts, "REVOKE "+ident(r)+" FROM "+name)
	}

	tx, err := s.conn.Begin(ctx)
	if err != nil {
		return friendly(err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	for _, stmt := range stmts {
		if _, err := tx.Exec(ctx, stmt); err != nil {
			return friendly(err)
		}
	}
	if c.Access != "" {
		if err := grantAccess(ctx, tx, c.Name, c.Access); err != nil {
			return friendly(err)
		}
	}
	return friendly(tx.Commit(ctx))
}

// grantAccess gives a role a ready-made level of access to the current
// database: it may connect, and read (or read and change) every table in
// every user schema. Tables the granting role creates later are covered
// too; tables created by other roles need their own grant.
func grantAccess(ctx context.Context, tx pgx.Tx, role, level string) error {
	name := ident(role)
	var database string
	if err := tx.QueryRow(ctx, "SELECT current_database()").Scan(&database); err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `SELECT n.nspname FROM pg_namespace n WHERE `+userSchemas+` ORDER BY 1`)
	if err != nil {
		return err
	}
	schemas, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	tables, sequences := "SELECT", "SELECT"
	if level == connector.AccessReadWrite {
		tables, sequences = "SELECT, INSERT, UPDATE, DELETE", "USAGE, SELECT"
	}
	stmts := []string{"GRANT CONNECT ON DATABASE " + ident(database) + " TO " + name}
	for _, schema := range schemas {
		sc := ident(schema)
		stmts = append(stmts,
			"GRANT USAGE ON SCHEMA "+sc+" TO "+name,
			"GRANT "+tables+" ON ALL TABLES IN SCHEMA "+sc+" TO "+name,
			"GRANT "+sequences+" ON ALL SEQUENCES IN SCHEMA "+sc+" TO "+name,
			"ALTER DEFAULT PRIVILEGES IN SCHEMA "+sc+" GRANT "+tables+" ON TABLES TO "+name,
			"ALTER DEFAULT PRIVILEGES IN SCHEMA "+sc+" GRANT "+sequences+" ON SEQUENCES TO "+name)
	}
	for _, stmt := range stmts {
		if _, err := tx.Exec(ctx, stmt); err != nil {
			return err
		}
	}
	return nil
}

// DropAccount removes a role. The server refuses while the role owns
// objects or holds privileges in any database. With handOver, what it has
// in the connected database is dealt with first: objects it owns go to the
// connecting user, and its privileges are revoked. Nothing is dropped
// except the role.
func (s *session) DropAccount(ctx context.Context, name, host string, handOver bool) error {
	if err := connector.CheckName("user", name); err != nil {
		return err
	}
	tx, err := s.conn.Begin(ctx)
	if err != nil {
		return friendly(err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	if handOver {
		// After the reassignment the role owns nothing here, so DROP
		// OWNED is left with only its privileges to remove.
		for _, stmt := range []string{"REASSIGN OWNED BY " + ident(name) + " TO CURRENT_USER", "DROP OWNED BY " + ident(name)} {
			if _, err := tx.Exec(ctx, stmt); err != nil {
				return friendly(err)
			}
		}
	}
	if _, err := tx.Exec(ctx, "DROP ROLE "+ident(name)); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "2BP01" {
			detail := strings.ReplaceAll(pgErr.Detail, "\n", "; ")
			if handOver {
				return fmt.Errorf("%s still depends on the role in another database: %s. Connect to that database and drop the user there", name, detail)
			}
			return fmt.Errorf("the role still owns objects or holds privileges: %s. Choose to hand them over, or revoke them first", detail)
		}
		return friendly(err)
	}
	return friendly(tx.Commit(ctx))
}

func (s *session) SetConnectionLimit(ctx context.Context, l connector.LimitChange) error {
	if err := connector.CheckName(l.Scope, l.Name); err != nil {
		return err
	}
	if l.Limit < connector.Unlimited {
		return errors.New("the connection limit cannot be negative")
	}
	var stmt string
	switch l.Scope {
	case "account":
		stmt = fmt.Sprintf("ALTER ROLE %s CONNECTION LIMIT %d", ident(l.Name), l.Limit)
	case "database":
		stmt = fmt.Sprintf("ALTER DATABASE %s CONNECTION LIMIT %d", ident(l.Name), l.Limit)
	default:
		return fmt.Errorf("unknown limit scope %q", l.Scope)
	}
	_, err := s.conn.Exec(ctx, stmt)
	return friendly(err)
}

// TerminateSessions disconnects the client sessions that match the
// filter, never this one.
func (s *session) TerminateSessions(ctx context.Context, f connector.SessionFilter) (int, error) {
	if f.Empty() {
		return 0, errors.New("say whose sessions to disconnect")
	}
	if err := f.Validate(); err != nil {
		return 0, err
	}
	// "idle in transaction (aborted)" is the same condition for this
	// purpose, so the state is matched by prefix.
	var n int
	err := s.conn.QueryRow(ctx, `SELECT count(*) FROM (
			SELECT pg_terminate_backend(pid) FROM pg_stat_activity
			WHERE pid <> pg_backend_pid() AND backend_type = 'client backend'
			  AND ($1 = '' OR usename = $1) AND ($2 = '' OR application_name = $2)
			  AND ($3 = '' OR datname = $3) AND ($4 = '' OR coalesce(host(client_addr), 'local') = $4)
			  AND ($5 = '' OR state = $5 OR ($5 = 'idle in transaction' AND state LIKE 'idle in transaction%'))
			  AND ($6 = 0 OR now() - state_change >= make_interval(secs => $6))
			  AND NOT (coalesce(usename, '') = ANY($7::text[])) AND NOT (coalesce(application_name, '') = ANY($8::text[]))) t`,
		f.User, f.Application, f.Database, f.Client, f.State, f.IdleSeconds,
		append([]string{}, f.ExcludeUsers...), append([]string{}, f.ExcludeApplications...)).Scan(&n)
	return n, friendly(err)
}
