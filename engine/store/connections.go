package store

import (
	"context"
	"errors"
	"strings"
	"time"
)

type Connection struct {
	ID          string    `json:"id"`
	OrgID       string    `json:"-"`
	Name        string    `json:"name"`
	Kind        string    `json:"kind"`
	Host        string    `json:"host"`
	Port        int       `json:"port"`
	Database    string    `json:"database"`
	Username    string    `json:"username"`
	SecretEnc   []byte    `json:"-"`
	SSLMode     string    `json:"sslmode"`
	SSLRootCert string    `json:"ssl_root_cert"`
	ReadOnly    bool      `json:"read_only"`
	CreatedBy   string    `json:"created_by"`
	CreatedAt   time.Time `json:"created_at"`
}

const connCols = `id,org_id,name,kind,host,port,database,username,secret_enc,sslmode,ssl_root_cert,read_only,created_by,created_at`

func scanConn(row interface{ Scan(...any) error }) (Connection, error) {
	var c Connection
	var created string
	err := row.Scan(&c.ID, &c.OrgID, &c.Name, &c.Kind, &c.Host, &c.Port, &c.Database, &c.Username, &c.SecretEnc, &c.SSLMode, &c.SSLRootCert, &c.ReadOnly, &c.CreatedBy, &created)
	c.CreatedAt = parseTime(created)
	return c, one(err)
}

func (s *Store) SaveConnection(ctx context.Context, c Connection) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO connections(`+connCols+`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET name=excluded.name,host=excluded.host,port=excluded.port,database=excluded.database,
		username=excluded.username,secret_enc=excluded.secret_enc,sslmode=excluded.sslmode,ssl_root_cert=excluded.ssl_root_cert,read_only=excluded.read_only`,
		c.ID, c.OrgID, c.Name, c.Kind, c.Host, c.Port, c.Database, c.Username, c.SecretEnc, c.SSLMode, c.SSLRootCert, c.ReadOnly, c.CreatedBy, now())
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
