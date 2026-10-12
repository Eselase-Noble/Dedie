package store

import (
	"context"
	"errors"
	"strings"
	"time"
)

type Connection struct {
	ID          string `json:"id"`
	OrgID       string `json:"-"`
	Name        string `json:"name"`
	Kind        string `json:"kind"`
	Host        string `json:"host"`
	Port        int    `json:"port"`
	Database    string `json:"database"`
	Username    string `json:"username"`
	SecretEnc   []byte `json:"-"`
	SSLMode     string `json:"sslmode"`
	SSLRootCert string `json:"ssl_root_cert"`
	ReadOnly    bool   `json:"read_only"`
	// Restricted limits the connection to administrators and the users
	// it has been granted to.
	Restricted bool `json:"restricted"`
	// Environment says where the database sits: "production", "staging",
	// "development", or empty when nobody has said.
	Environment string    `json:"environment"`
	CreatedBy   string    `json:"created_by"`
	CreatedAt   time.Time `json:"created_at"`
}

const connCols = `id,org_id,name,kind,host,port,database,username,secret_enc,sslmode,ssl_root_cert,read_only,created_by,created_at,restricted,environment`

func scanConn(row interface{ Scan(...any) error }) (Connection, error) {
	var c Connection
	var created string
	err := row.Scan(&c.ID, &c.OrgID, &c.Name, &c.Kind, &c.Host, &c.Port, &c.Database, &c.Username, &c.SecretEnc, &c.SSLMode, &c.SSLRootCert, &c.ReadOnly, &c.CreatedBy, &created, &c.Restricted, &c.Environment)
	c.CreatedAt = parseTime(created)
	return c, one(err)
}

func (s *Store) SaveConnection(ctx context.Context, c Connection) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO connections(`+connCols+`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET name=excluded.name,host=excluded.host,port=excluded.port,database=excluded.database,
		username=excluded.username,secret_enc=excluded.secret_enc,sslmode=excluded.sslmode,ssl_root_cert=excluded.ssl_root_cert,read_only=excluded.read_only,environment=excluded.environment`,
		c.ID, c.OrgID, c.Name, c.Kind, c.Host, c.Port, c.Database, c.Username, c.SecretEnc, c.SSLMode, c.SSLRootCert, c.ReadOnly, c.CreatedBy, now(), c.Restricted, c.Environment)
	if err != nil && strings.Contains(err.Error(), "UNIQUE") {
		return errors.New("a connection with that name already exists")
	}
	return err
}

func (s *Store) GetConnection(ctx context.Context, orgID, id string) (Connection, error) {
	return scanConn(s.db.QueryRowContext(ctx, `SELECT `+connCols+` FROM connections WHERE org_id=? AND (id=? OR name=?)`, orgID, id, id))
}

func (s *Store) ListConnections(ctx context.Context, orgID string) ([]Connection, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+connCols+` FROM connections WHERE org_id=? ORDER BY name`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Connection{}
	for rows.Next() {
		c, err := scanConn(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) DeleteConnection(ctx context.Context, orgID, id string) error {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM jobs WHERE org_id=? AND (source_id=? OR target_id=?) AND status NOT IN ('completed','aborted')`, orgID, id, id).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return errors.New("connection is used by an active migration")
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM connections WHERE org_id=? AND id=?`, orgID, id)
	return err
}

// ConnectionGrants lists the users a restricted connection is granted to.
func (s *Store) ConnectionGrants(ctx context.Context, connID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT user_id FROM connection_grants WHERE connection_id=? ORDER BY user_id`, connID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// UserGrants lists the connections granted to a user.
func (s *Store) UserGrants(ctx context.Context, userID string) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT connection_id FROM connection_grants WHERE user_id=?`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// SetConnectionAccess replaces a connection's restriction and its grants.
func (s *Store) SetConnectionAccess(ctx context.Context, connID string, restricted bool, userIDs []string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE connections SET restricted=? WHERE id=?`, restricted, connID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM connection_grants WHERE connection_id=?`, connID); err != nil {
		return err
	}
	for _, id := range userIDs {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO connection_grants(connection_id,user_id) VALUES(?,?)`, connID, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}
