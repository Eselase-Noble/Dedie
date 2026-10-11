// Package postgres is the PostgreSQL driver: query execution, catalog
// introspection and server administration. The migration pipeline reuses its
// connection helpers for regular and replication connections.
package postgres

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"dedie/engine/connector"
)

const Kind = "postgres"

func init() { connector.Register(driver{}) }

type driver struct{}

func (driver) Kind() string     { return Kind }
func (driver) DefaultPort() int { return 5432 }

func (driver) Open(ctx context.Context, cfg connector.Config) (connector.Session, error) {
	conn, err := Connect(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &session{conn: conn}, nil
}

type session struct{ conn *pgx.Conn }

func (s *session) Close(ctx context.Context) error { return s.conn.Close(ctx) }

func quoteDSN(v string) string {
	return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(v) + "'"
}

// ConnConfig translates a connector.Config into a pgx configuration.
func ConnConfig(cfg connector.Config) (*pgx.ConnConfig, error) {
	sslmode := cfg.SSLMode
	if sslmode == "" {
		sslmode = "prefer"
	}
	port := cfg.Port
	if port == 0 {
		port = 5432
	}
	dsn := fmt.Sprintf("host=%s port=%d dbname=%s user=%s password=%s sslmode=%s connect_timeout=10 application_name=dedie",
		quoteDSN(cfg.Host), port, quoteDSN(cfg.Database), quoteDSN(cfg.Username), quoteDSN(cfg.Password), quoteDSN(sslmode))
	pc, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("invalid connection settings: %w", err)
	}
	if cfg.SSLRootCert != "" && pc.TLSConfig != nil {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM([]byte(cfg.SSLRootCert)) {
			return nil, errors.New("SSL root certificate is not valid PEM")
		}
		pc.TLSConfig.RootCAs = pool
	}
	return pc, nil
}

// Connect opens a regular connection.
func Connect(ctx context.Context, cfg connector.Config) (*pgx.Conn, error) {
	pc, err := ConnConfig(cfg)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	conn, err := pgx.ConnectConfig(ctx, pc)
	if err != nil {
		return nil, friendly(err)
	}
	return conn, nil
}

// ConnectReplication opens a logical replication connection to the database.
func ConnectReplication(ctx context.Context, cfg connector.Config) (*pgconn.PgConn, error) {
	pc, err := ConnConfig(cfg)
	if err != nil {
		return nil, err
	}
	pc.RuntimeParams["replication"] = "database"
	// The stream carries values as text, rendered with this session's
	// settings. Fixing them makes the text the same on every server:
	// ISO dates, timestamps in UTC and floats at full precision.
	pc.RuntimeParams["datestyle"] = "ISO"
	pc.RuntimeParams["intervalstyle"] = "postgres"
	pc.RuntimeParams["extra_float_digits"] = "3"
	pc.RuntimeParams["timezone"] = "UTC"
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	conn, err := pgconn.ConnectConfig(ctx, &pc.Config)
	if err != nil {
		return nil, friendly(err)
	}
	return conn, nil
}

// friendly strips driver noise from connection errors without leaking the
// password (pgx already redacts it from its own messages).
func friendly(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return fmt.Errorf("%s (SQLSTATE %s)", pgErr.Message, pgErr.Code)
	}
	return err
}

func (s *session) Info(ctx context.Context) (connector.ServerInfo, error) {
	info := connector.ServerInfo{Kind: Kind}
	err := s.conn.QueryRow(ctx, `SELECT current_setting('server_version'), current_setting('server_version_num')::int,
		current_database(), current_user, (SELECT rolsuper FROM pg_roles WHERE rolname = current_user)`).
		Scan(&info.Version, &info.VersionNum, &info.Database, &info.User, &info.Superuser)
	if err != nil {
		return info, friendly(err)
	}
	// Report whether this very connection is encrypted.
	_ = s.conn.QueryRow(ctx, `SELECT coalesce((SELECT ssl FROM pg_stat_ssl WHERE pid = pg_backend_pid()), false)`).Scan(&info.Encrypted)
	return info, nil
}
