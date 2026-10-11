// Package mongodb connects the engine to MongoDB: the query browser runs
// database commands written as JSON, and the migration pipeline reads
// collections and their change stream.
package mongodb

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"dedie/engine/connector"
)

const Kind = "mongodb"

func init() { connector.Register(driver{}) }

type driver struct{}

func (driver) Kind() string     { return Kind }
func (driver) DefaultPort() int { return 27017 }

func (driver) Open(ctx context.Context, cfg connector.Config) (connector.Session, error) {
	client, err := Connect(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &session{client: client, db: client.Database(cfg.Database), cfg: cfg}, nil
}

type session struct {
	client *mongo.Client
	db     *mongo.Database
	cfg    connector.Config
}

func (s *session) Close(ctx context.Context) error { return s.client.Disconnect(ctx) }

// Connect opens a client to the server named in cfg and checks that it
// answers.
//
// The connection is made directly to that host rather than through
// replica-set discovery: discovery follows the host names the members
// advertise to each other, which are often not reachable from outside
// their network. Change streams work on a direct connection all the same.
//
// The account is looked up in the admin database, where MongoDB keeps
// users by default.
func Connect(ctx context.Context, cfg connector.Config) (*mongo.Client, error) {
	port := cfg.Port
	if port == 0 {
		port = 27017
	}
	opts := options.Client().
		SetHosts([]string{net.JoinHostPort(cfg.Host, strconv.Itoa(port))}).
		SetDirect(true).
		SetAppName("dedie").
		SetServerSelectionTimeout(10 * time.Second).
		SetConnectTimeout(10 * time.Second)
	if cfg.Username != "" {
		opts.SetAuth(options.Credential{Username: cfg.Username, Password: cfg.Password, AuthSource: "admin"})
	}
	tlsConfig, err := TLSConfig(cfg)
	if err != nil {
		return nil, err
	}
	if tlsConfig != nil {
		opts.SetTLSConfig(tlsConfig)
	}
	client, err := mongo.Connect(opts)
	if err != nil {
		return nil, Friendly(err)
	}
	if err := client.Ping(ctx, nil); err != nil {
		client.Disconnect(context.WithoutCancel(ctx))
		return nil, Friendly(err)
	}
	return client, nil
}

// TLSConfig maps the connection's SSL mode onto the driver, which either
// uses TLS or does not: "disable", "allow" and "prefer" connect in clear
// text, "require" encrypts without checking the certificate, and the
// verify modes check it against the system authorities or the given one.
func TLSConfig(cfg connector.Config) (*tls.Config, error) {
	switch cfg.SSLMode {
	case "", "disable", "allow", "prefer":
		return nil, nil
	case "require":
		return &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}, nil
	}
	c := &tls.Config{ServerName: cfg.Host, MinVersion: tls.VersionTLS12}
	if cfg.SSLRootCert != "" {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM([]byte(cfg.SSLRootCert)) {
			return nil, errors.New("the root certificate is not valid PEM")
		}
		c.RootCAs = pool
	}
	return c, nil
}

// Friendly reduces a driver error to what the server said.
func Friendly(err error) error {
	if err == nil {
		return nil
	}
	var cmdErr mongo.CommandError
	if errors.As(err, &cmdErr) {
		return fmt.Errorf("%s (%s)", cmdErr.Message, cmdErr.Name)
	}
	var sel interface{ Unwrap() error }
	if mongo.IsTimeout(err) && errors.As(err, &sel) {
		return fmt.Errorf("the server did not answer in time: %v", err)
	}
	return err
}

// Hello is the part of the server's handshake answer the engine uses.
type Hello struct {
	// SetName is the replica set's name; empty on a standalone server,
	// which has no change streams.
	SetName  string `bson:"setName"`
	Primary  bool   `bson:"isWritablePrimary"`
	ReadOnly bool   `bson:"readOnly"`
}

// ServerHello asks the server what it is.
func ServerHello(ctx context.Context, client *mongo.Client) (Hello, error) {
	var h Hello
	err := client.Database("admin").RunCommand(ctx, bson.D{{Key: "hello", Value: 1}}).Decode(&h)
	return h, Friendly(err)
}

// Version returns the server's version string.
func Version(ctx context.Context, client *mongo.Client) (string, error) {
	var info struct {
		Version string `bson:"version"`
	}
	err := client.Database("admin").RunCommand(ctx, bson.D{{Key: "buildInfo", Value: 1}}).Decode(&info)
	return info.Version, Friendly(err)
}

// VersionNum turns "8.0.4" into 80004, the form the engine compares.
func VersionNum(version string) int {
	var major, minor, patch int
	fmt.Sscanf(version, "%d.%d.%d", &major, &minor, &patch)
	return major*10000 + minor*100 + patch
}
