// Package mysql is the MySQL driver: query execution, catalog introspection
// and server administration. The MySQL migration pipeline reuses its
// connection and TLS helpers.
package mysql

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	driver "github.com/go-sql-driver/mysql"

	"dedie/engine/connector"
)

const Kind = "mysql"

func init() { connector.Register(mysqlDriver{}) }

type mysqlDriver struct{}

func (mysqlDriver) Kind() string     { return Kind }
func (mysqlDriver) DefaultPort() int { return 3306 }

func (mysqlDriver) Open(ctx context.Context, cfg connector.Config) (connector.Session, error) {
	db, err := Open(ctx, cfg, nil, FoundRows)
	if err != nil {
		return nil, err
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		db.Close()
		return nil, Friendly(err)
	}
	return &session{db: db, conn: conn}, nil
}

// session pins one connection so that session settings made for a query
// (read-only mode, timeouts) apply to the statements that follow.
type session struct {
	db   *sql.DB
	conn *sql.Conn
}

func (s *session) Close(ctx context.Context) error {
	s.conn.Close()
	return s.db.Close()
}

// Alive reports whether the server still answers on this connection.
func (s *session) Alive(ctx context.Context) bool { return s.conn.PingContext(ctx) == nil }

// Reset reports false: nothing puts a session back the way it was opened
// once a statement has changed it, so such a session is closed instead
// of being used again.
func (s *session) Reset(context.Context) bool { return false }

// TLSConfig maps the connection's SSL mode onto a TLS configuration. It
// returns nil when the mode does not demand TLS ("disable", "allow",
// "prefer"); the SQL driver negotiates opportunistic TLS for those itself.
func TLSConfig(cfg connector.Config) (*tls.Config, error) {
	var roots *x509.CertPool
	if cfg.SSLRootCert != "" {
		roots = x509.NewCertPool()
		if !roots.AppendCertsFromPEM([]byte(cfg.SSLRootCert)) {
			return nil, errors.New("SSL root certificate is not valid PEM")
		}
	}
	switch cfg.SSLMode {
	case "require":
		// Encrypt without checking who the server is.
		return &tls.Config{InsecureSkipVerify: true}, nil
	case "verify-ca":
		// Check the certificate chain but not the host name.
		return &tls.Config{
			InsecureSkipVerify: true,
			VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
				certs := make([]*x509.Certificate, len(raw))
				for i, der := range raw {
					c, err := x509.ParseCertificate(der)
					if err != nil {
						return err
					}
					certs[i] = c
				}
				if len(certs) == 0 {
					return errors.New("server presented no certificate")
				}
				opts := x509.VerifyOptions{Roots: roots, Intermediates: x509.NewCertPool()}
				for _, c := range certs[1:] {
					opts.Intermediates.AddCert(c)
				}
				_, err := certs[0].Verify(opts)
				return err
			},
		}, nil
	case "verify-full":
		return &tls.Config{ServerName: cfg.Host, RootCAs: roots}, nil
	}
	return nil, nil
}

// Open returns a connection pool for the configured database. params are
// extra session variables set on every connection, for example
// {"foreign_key_checks": "0"}; adjust may change driver options.
func Open(ctx context.Context, cfg connector.Config, params map[string]string, adjust ...func(*driver.Config)) (*sql.DB, error) {
	port := cfg.Port
	if port == 0 {
		port = 3306
	}
	dc := driver.NewConfig()
	dc.User, dc.Passwd, dc.DBName = cfg.Username, cfg.Password, cfg.Database
	dc.Net, dc.Addr = "tcp", net.JoinHostPort(cfg.Host, strconv.Itoa(port))
	dc.Timeout = 15 * time.Second
	// Placeholders are filled in client-side, so a statement costs one round
	// trip and values of any type can be passed for any column.
	dc.InterpolateParams = true
	dc.Collation = "utf8mb4_general_ci"
	dc.MaxAllowedPacket = 0 // use the server's limit
	dc.Loc = time.UTC
	dc.Params = map[string]string{"time_zone": "'+00:00'"}
	for k, v := range params {
		dc.Params[k] = v
	}
	for _, fn := range adjust {
		fn(dc)
	}
	tlsCfg, err := TLSConfig(cfg)
	if err != nil {
		return nil, err
	}
	switch {
	case tlsCfg != nil:
		dc.TLS = tlsCfg
	case cfg.SSLMode == "allow" || cfg.SSLMode == "prefer" || cfg.SSLMode == "":
		dc.TLSConfig = "preferred"
	}
	connector, err := driver.NewConnector(dc)
	if err != nil {
		return nil, fmt.Errorf("invalid connection settings: %w", err)
	}
	db := sql.OpenDB(connector)
	pingCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		db.Close()
		return nil, Friendly(err)
	}
	return db, nil
}

// FoundRows makes UPDATE report the rows it matched rather than the rows it
// changed, so a statement that finds its row but leaves it as it was still
// counts as having hit it.
func FoundRows(dc *driver.Config) { dc.ClientFoundRows = true }

// Friendly reduces a driver error to the server's message and code.
func Friendly(err error) error {
	var myErr *driver.MySQLError
	if errors.As(err, &myErr) {
		return fmt.Errorf("%s (error %d)", myErr.Message, myErr.Number)
	}
	return err
}

// VersionNum turns "8.0.46-0ubuntu" into 80046.
func VersionNum(version string) int {
	parts := strings.SplitN(version, ".", 3)
	num := 0
	for i := 0; i < 3; i++ {
		n := 0
		if i < len(parts) {
			digits := parts[i]
			for j, c := range digits {
				if c < '0' || c > '9' {
					digits = digits[:j]
					break
				}
			}
			n, _ = strconv.Atoi(digits)
		}
		num = num*100 + n
	}
	return num
}

// Quote quotes an identifier.
func Quote(name string) string { return "`" + strings.ReplaceAll(name, "`", "``") + "`" }

// Encrypted reports whether a connection is using TLS.
func Encrypted(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}) bool {
	var name, cipher string
	_ = q.QueryRowContext(ctx, `SHOW SESSION STATUS LIKE 'Ssl_cipher'`).Scan(&name, &cipher)
	return cipher != ""
}

func (s *session) Info(ctx context.Context) (connector.ServerInfo, error) {
	info := connector.ServerInfo{Kind: Kind}
	var db sql.NullString
	var super int
	err := s.conn.QueryRowContext(ctx, `SELECT VERSION(), DATABASE(), CURRENT_USER(),
		(SELECT COUNT(*) FROM information_schema.user_privileges
		 WHERE privilege_type IN ('SUPER','SYSTEM_USER')
		   AND grantee = CONCAT("'", SUBSTRING_INDEX(CURRENT_USER(),'@',1), "'@'", SUBSTRING_INDEX(CURRENT_USER(),'@',-1), "'"))`).
		Scan(&info.Version, &db, &info.User, &super)
	if err != nil {
		return info, Friendly(err)
	}
	info.Database, info.Superuser = db.String, super > 0
	info.VersionNum = VersionNum(info.Version)
	info.Encrypted = Encrypted(ctx, s.conn)
	return info, nil
}
