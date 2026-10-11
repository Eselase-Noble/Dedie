package mysql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"dedie/engine/connector"
)

// Accounts lists users with their limits and current connections, the
// server's connection budget and who holds connections now. Reading
// mysql.user needs SELECT on the mysql schema.
func (s *session) Accounts(ctx context.Context) (*connector.Accounts, error) {
	out := &connector.Accounts{Accounts: []connector.Account{}, Roles: []string{}, Databases: []connector.DatabaseLimit{}, Usage: []connector.ConnectionUsage{}}

	type held struct {
		total, active, idle int
		oldest              int64
	}
	byUser := map[string]*held{}
	rows, err := s.conn.QueryContext(ctx, `SELECT user, SUBSTRING_INDEX(host, ':', 1), COALESCE(db, ''), command, time FROM information_schema.processlist`)
	if err != nil {
		return nil, Friendly(err)
	}
	usage := map[[3]string]*connector.ConnectionUsage{}
	var order [][3]string
	for rows.Next() {
		var user, client, db, command string
		var seconds int64
		if err := rows.Scan(&user, &client, &db, &command, &seconds); err != nil {
			rows.Close()
			return nil, err
		}
		h := byUser[user]
		if h == nil {
			h = &held{}
			byUser[user] = h
		}
		h.total++
		if command == "Sleep" {
			h.idle++
			if seconds > h.oldest {
				h.oldest = seconds
			}
		} else {
			h.active++
		}
		out.Used++
		key := [3]string{user, client, db}
		u := usage[key]
		if u == nil {
			u = &connector.ConnectionUsage{User: user, Client: client, Database: db}
			usage[key] = u
			order = append(order, key)
		}
		u.Total++
		if command == "Sleep" {
			u.Idle++
		} else {
			u.Active++
		}
		if seconds > u.LongestSeconds {
			u.LongestSeconds = seconds
		}
	}
	rows.Close()
	for _, key := range order {
		out.Usage = append(out.Usage, *usage[key])
	}

	_ = s.conn.QueryRowContext(ctx, `SELECT @@GLOBAL.max_connections`).Scan(&out.MaxConnections)
	// One connection beyond the limit is kept for an administrator.
	out.Reserved = 1

	// A role is an account that cannot log in by itself: locked, with no
	// password. They are what can be granted to users.
	rows, err = s.conn.QueryContext(ctx, `SELECT user, host, account_locked = 'N', Super_priv = 'Y', Create_priv = 'Y', Create_user_priv = 'Y',
			Repl_slave_priv = 'Y', max_user_connections, account_locked = 'Y' AND authentication_string = ''
		FROM mysql.user WHERE user NOT IN ('mysql.sys', 'mysql.session', 'mysql.infoschema') ORDER BY user, host`)
	if err != nil {
		return nil, fmt.Errorf("listing users needs SELECT on the mysql schema: %w", Friendly(err))
	}
	index := map[string]int{}
	for rows.Next() {
		var a connector.Account
		var isRole bool
		if err := rows.Scan(&a.Name, &a.Host, &a.CanLogin, &a.Superuser, &a.CreateDB, &a.CreateRole, &a.Replication, &a.ConnectionLimit, &isRole); err != nil {
			rows.Close()
			return nil, err
		}
		// MySQL writes "no limit" as 0.
		if a.ConnectionLimit == 0 {
			a.ConnectionLimit = connector.Unlimited
		}
		if h := byUser[a.Name]; h != nil {
			a.Connections, a.Active, a.Idle, a.OldestIdleSeconds = h.total, h.active, h.idle, h.oldest
		}
		a.MemberOf = []string{}
		index[a.Name+"@"+a.Host] = len(out.Accounts)
		out.Accounts = append(out.Accounts, a)
		if isRole {
			out.Roles = append(out.Roles, a.Name)
		}
	}
	rows.Close()

	if rows, err = s.conn.QueryContext(ctx, `SELECT TO_USER, TO_HOST, FROM_USER FROM mysql.role_edges`); err == nil {
		for rows.Next() {
			var user, host, role string
			if rows.Scan(&user, &host, &role) == nil {
				if i, ok := index[user+"@"+host]; ok {
					out.Accounts[i].MemberOf = append(out.Accounts[i].MemberOf, role)
				}
			}
		}
		rows.Close()
	}
	return out, nil
}

// text quotes a string literal. SaveAccount clears the session's SQL mode
// first, so the backslash is an escape character here whatever the
// server's default is.
func text(v string) string {
	return "'" + strings.NewReplacer(`\`, `\\`, `'`, `''`).Replace(v) + "'"
}

func account(name, host string) string {
	if host == "" {
		host = "%"
	}
	return text(name) + "@" + text(host)
}

// SaveAccount creates a user or changes one. MySQL commits each of these
// statements as it runs, so a failure part-way leaves the earlier ones
// in effect; the error says which statement failed.
func (s *session) SaveAccount(ctx context.Context, c connector.AccountChange) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if _, err := s.conn.ExecContext(ctx, "SET SESSION sql_mode = ''"); err != nil {
		return Friendly(err)
	}
	who := account(c.Name, c.Host)
	var options string
	if c.Password != nil && *c.Password != "" {
		options += " IDENTIFIED BY " + text(*c.Password)
	}
	if c.ConnectionLimit != nil {
		limit := *c.ConnectionLimit
		if limit == connector.Unlimited {
			limit = 0
		}
		options += fmt.Sprintf(" WITH MAX_USER_CONNECTIONS %d", limit)
	}
	if c.CanLogin != nil {
		options += map[bool]string{true: " ACCOUNT UNLOCK", false: " ACCOUNT LOCK"}[*c.CanLogin]
	}
	var stmts []string
	switch {
	case c.Create:
		stmts = append(stmts, "CREATE USER "+who+options)
	case options != "":
		stmts = append(stmts, "ALTER USER "+who+options)
	}
	for _, r := range c.Grant {
		stmts = append(stmts, "GRANT "+Quote(r)+" TO "+who, "SET DEFAULT ROLE ALL TO "+who)
	}
	for _, r := range c.Revoke {
		stmts = append(stmts, "REVOKE "+Quote(r)+" FROM "+who)
	}
	if c.Access != "" {
		var database sql.NullString
		if err := s.conn.QueryRowContext(ctx, "SELECT DATABASE()").Scan(&database); err != nil || database.String == "" {
			return errors.New("the connection has no database to grant access to")
		}
		privileges := "SELECT, SHOW VIEW"
		if c.Access == connector.AccessReadWrite {
			privileges = "SELECT, SHOW VIEW, INSERT, UPDATE, DELETE, EXECUTE"
		}
		stmts = append(stmts, "GRANT "+privileges+" ON "+Quote(database.String)+".* TO "+who)
	}
	for i, stmt := range stmts {
		if _, err := s.conn.ExecContext(ctx, stmt); err != nil {
			if i > 0 {
				return fmt.Errorf("%w; the earlier changes to this user were already applied", Friendly(err))
			}
			return Friendly(err)
		}
	}
	return nil
}

func (s *session) DropAccount(ctx context.Context, name, host string, handOver bool) error {
	if err := connector.CheckName("user", name); err != nil {
		return err
	}
	if _, err := s.conn.ExecContext(ctx, "SET SESSION sql_mode = ''"); err != nil {
		return Friendly(err)
	}
	_, err := s.conn.ExecContext(ctx, "DROP USER "+account(name, host))
	return Friendly(err)
}

func (s *session) SetConnectionLimit(ctx context.Context, l connector.LimitChange) error {
	if l.Scope != "account" {
		return errors.New("MySQL limits connections per user, not per database")
	}
	limit := l.Limit
	return s.SaveAccount(ctx, connector.AccountChange{Name: l.Name, Host: l.Host, ConnectionLimit: &limit})
}

// TerminateSessions disconnects the sessions that match the filter, never
// this one. MySQL does not record an application name, so that part of
// the filter is ignored.
func (s *session) TerminateSessions(ctx context.Context, f connector.SessionFilter) (int, error) {
	if f.User == "" && f.Database == "" && f.Client == "" && f.State == "" {
		return 0, errors.New("say whose sessions to disconnect")
	}
	if err := f.Validate(); err != nil {
		return 0, err
	}
	if f.State == connector.StateIdleInTransaction {
		return 0, errors.New("MySQL does not report sessions idle inside a transaction here; close idle sessions instead")
	}
	// An idle MySQL session is one whose command is Sleep; its time is
	// how long it has been asleep. The server's own threads are never
	// touched.
	rows, err := s.conn.QueryContext(ctx, `SELECT id FROM information_schema.processlist
		WHERE id <> CONNECTION_ID() AND user NOT IN ('event_scheduler', 'system user')
		  AND (? = '' OR user = ?) AND (? = '' OR db = ?) AND (? = '' OR SUBSTRING_INDEX(host, ':', 1) = ?)
		  AND (? = '' OR (command = 'Sleep' AND time >= ?))`,
		f.User, f.User, f.Database, f.Database, f.Client, f.Client, f.State, f.IdleSeconds)
	if err != nil {
		return 0, Friendly(err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	n := 0
	for _, id := range ids {
		// A session that ended in the meantime is not an error.
		if _, err := s.conn.ExecContext(ctx, fmt.Sprintf("KILL CONNECTION %d", id)); err == nil {
			n++
		}
	}
	return n, nil
}
