// Package oracle is the Oracle Database driver: query execution, catalog
// introspection and the parts of server administration that have a direct
// equivalent. The Oracle migration pipeline reuses its connection helpers.
//
// A connection names a service, which on a multitenant server is usually a
// pluggable database. The driver speaks the wire protocol itself, so no
// Oracle client libraries have to be installed.
package oracle

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	go_ora "github.com/sijms/go-ora/v2"
	"github.com/sijms/go-ora/v2/network"

	"dedie/engine/connector"
)

const Kind = "oracle"

func init() { connector.Register(oracleDriver{}) }

type oracleDriver struct{}

func (oracleDriver) Kind() string     { return Kind }
func (oracleDriver) DefaultPort() int { return 1521 }

func (oracleDriver) Open(ctx context.Context, cfg connector.Config) (connector.Session, error) {
	db, err := Open(ctx, cfg)
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

// session pins one connection, so a read-only transaction opened for a
// query covers the statement that follows it.
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

// tlsConfig maps the connection's SSL mode onto a TLS configuration, or
// nil when the mode does not ask for TLS. Oracle listens for TLS on its
// own port (TCPS), so there is no opportunistic upgrade to fall back on.
func tlsConfig(cfg connector.Config) (*tls.Config, error) {
	var roots *x509.CertPool
	if cfg.SSLRootCert != "" {
		roots = x509.NewCertPool()
		if !roots.AppendCertsFromPEM([]byte(cfg.SSLRootCert)) {
			return nil, errors.New("SSL root certificate is not valid PEM")
		}
	}
	switch cfg.SSLMode {
	case "require":
		return &tls.Config{InsecureSkipVerify: true}, nil
	case "verify-ca", "verify-full":
		return &tls.Config{ServerName: cfg.Host, RootCAs: roots}, nil
	}
	return nil, nil
}

// Open returns a connection pool for the configured service.
func Open(ctx context.Context, cfg connector.Config) (*sql.DB, error) {
	port := cfg.Port
	if port == 0 {
		port = 1521
	}
	if strings.TrimSpace(cfg.Database) == "" {
		return nil, errors.New("enter the Oracle service name in the database field, for example FREEPDB1 or ORCLPDB1")
	}
	opts := map[string]string{"CONNECTION TIMEOUT": "15", "PREFETCH_ROWS": "500", "PROGRAM": "Dedie"}
	tlsCfg, err := tlsConfig(cfg)
	if err != nil {
		return nil, err
	}
	if tlsCfg != nil {
		opts["SSL"] = "true"
		opts["SSL VERIFY"] = strconv.FormatBool(!tlsCfg.InsecureSkipVerify)
	}
	c := go_ora.NewConnector(go_ora.BuildUrl(cfg.Host, port, cfg.Database, cfg.Username, cfg.Password, opts))
	if oc, ok := c.(*go_ora.OracleConnector); ok && tlsCfg != nil {
		oc.WithTLSConfig(tlsCfg)
	}
	db := sql.OpenDB(c)
	pingCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		db.Close()
		return nil, Friendly(err)
	}
	return db, nil
}

var positionRe = regexp.MustCompile(`\s*error occur at position: -?\d+\s*$`)

// Friendly reduces a driver error to the server's message, with advice
// for the mistakes that are easy to make when setting up a connection.
func Friendly(err error) error {
	if err == nil {
		return nil
	}
	var oe *network.OracleError
	if !errors.As(err, &oe) {
		return err
	}
	msg := strings.TrimSpace(positionRe.ReplaceAllString(oe.Error(), ""))
	switch oe.ErrCode {
	case 12514:
		msg += ". Check the service name: it is the name of the pluggable database's service, not the instance SID"
	case 1017:
		msg = "ORA-01017: the username or password is not correct"
	case 28000:
		msg = "ORA-28000: the account is locked"
	case 28001:
		msg = "ORA-28001: the account's password has expired"
	}
	return errors.New(msg)
}

// Quote quotes an identifier.
func Quote(name string) string { return `"` + strings.ReplaceAll(name, `"`, "") + `"` }

// QName quotes a table name with its owner.
func QName(owner, table string) string { return Quote(owner) + "." + Quote(table) }

var versionRe = regexp.MustCompile(`(\d+)\.(\d+)\.(\d+)`)

// VersionNum turns "23.26.1.0.0" into 232601.
func VersionNum(version string) int {
	m := versionRe.FindStringSubmatch(version)
	if m == nil {
		return 0
	}
	num := 0
	for _, part := range m[1:] {
		n, _ := strconv.Atoi(part)
		num = num*100 + n%100
	}
	return num
}

// Version returns the server's banner and its dotted version number.
func Version(ctx context.Context, q Queryer) (banner, version string, err error) {
	// VERSION_FULL carries the release update; it exists from 18c on.
	err = q.QueryRowContext(ctx, `SELECT product, version_full FROM product_component_version WHERE product LIKE 'Oracle%' AND ROWNUM = 1`).Scan(&banner, &version)
	if err != nil {
		err = q.QueryRowContext(ctx, `SELECT product, version FROM product_component_version WHERE product LIKE 'Oracle%' AND ROWNUM = 1`).Scan(&banner, &version)
	}
	return strings.TrimSpace(banner), version, Friendly(err)
}

// Queryer is what a pool and a pinned connection have in common.
type Queryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func (s *session) Info(ctx context.Context) (connector.ServerInfo, error) {
	info := connector.ServerInfo{Kind: Kind}
	banner, version, err := Version(ctx, s.conn)
	if err != nil {
		return info, err
	}
	info.Version, info.VersionNum = fmt.Sprintf("%s %s", banner, version), VersionNum(version)
	var proto sql.NullString
	var dba int
	err = s.conn.QueryRowContext(ctx, `SELECT SYS_CONTEXT('USERENV','CON_NAME'), SYS_CONTEXT('USERENV','SESSION_USER'),
			SYS_CONTEXT('USERENV','NETWORK_PROTOCOL'), (SELECT COUNT(*) FROM session_roles WHERE role = 'DBA') FROM dual`).
		Scan(&info.Database, &info.User, &proto, &dba)
	if err != nil {
		return info, Friendly(err)
	}
	info.Encrypted, info.Superuser = strings.EqualFold(proto.String, "tcps"), dba > 0
	return info, nil
}
