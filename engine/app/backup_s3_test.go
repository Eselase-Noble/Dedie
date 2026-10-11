package app

import (
	"context"
	"fmt"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"

	"dedie/engine/connector/postgres"
	"dedie/engine/storage"
)

// TestBackupToObjectStorage takes backups into an S3 bucket and restores
// from it. It needs a PostgreSQL server:
//
//	DEDIE_TEST_TARGET=postgres://postgres@127.0.0.1:54318/postgres
func TestBackupToObjectStorage(t *testing.T) {
	raw := os.Getenv("DEDIE_TEST_TARGET")
	if raw == "" {
		t.Skip("DEDIE_TEST_TARGET not set")
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	port, _ := strconv.Atoi(u.Port())
	password, _ := u.User.Password()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	backend := s3mem.New()
	srv := httptest.NewTLSServer(gofakes3.New(backend).Server())
	defer srv.Close()
	backend.CreateBucket("backups")
	storage.Transport = srv.Client().Transport
	defer func() { storage.Transport = nil }()

	a, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	owner, err := a.Setup(ctx, "Acme", "Ada", "ada@example.com", "a-long-enough-password")
	if err != nil {
		t.Fatal(err)
	}
	actor := Actor{User: owner}

	name := fmt.Sprintf("dedie_s3_%d", time.Now().UnixNano()%1e9)
	in := ConnectionInput{Name: "src", Kind: "postgres", Host: u.Hostname(), Port: port, Database: "postgres",
		Username: u.User.Username(), Password: password, SSLMode: "disable"}
	admin, err := a.SaveConnection(ctx, actor, "", in)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.CreateDatabase(ctx, actor, admin.ID, name, false); err != nil {
		t.Fatal(err)
	}
	cfg, _ := a.config(admin)
	defer func() {
		if c, err := postgres.Connect(context.Background(), cfg); err == nil {
			c.Exec(context.Background(), `DROP DATABASE IF EXISTS `+name+` WITH (FORCE)`)
			c.Exec(context.Background(), `DROP DATABASE IF EXISTS `+name+`_back WITH (FORCE)`)
			c.Close(context.Background())
		}
	}()
	in.Name, in.Database = "db", name
	conn, err := a.SaveConnection(ctx, actor, "", in)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Query(ctx, actor, conn.ID, QueryRequest{Write: true, SQL: `CREATE TABLE t (id int PRIMARY KEY, v text); INSERT INTO t SELECT g, md5(g::text) FROM generate_series(1, 5000) g`}); err != nil {
		t.Fatal(err)
	}

	if _, err := a.CreateLocation(ctx, actor, LocationInput{Name: "bad", Kind: "s3", S3: storage.S3Config{Endpoint: srv.URL, Bucket: "missing", AccessKey: "k", SecretKey: "s"}}); err == nil {
		t.Error("a location in a bucket that does not exist was accepted")
	}
	loc, err := a.CreateLocation(ctx, actor, LocationInput{Name: "bucket", Kind: "s3",
		S3: storage.S3Config{Endpoint: srv.URL, Region: "us-east-1", Bucket: "backups", Prefix: "prod", AccessKey: "k", SecretKey: "top-secret"}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(loc.Path, "top-secret") || loc.Kind != "s3" {
		t.Errorf("location %+v", loc)
	}

	for _, plain := range []bool{false, true} {
		b, err := a.CreateBackup(ctx, actor, BackupInput{ConnectionID: conn.ID, LocationID: loc.ID, Plain: plain}, true)
		if err != nil || b.Status != "completed" {
			t.Fatalf("backup (plain=%v): %v %s", plain, err, b.Error)
		}
		if !strings.HasPrefix(b.Path, "s3://backups/prod/"+name+"-") || b.SizeBytes == 0 {
			t.Errorf("backup stored at %q, %d bytes", b.Path, b.SizeBytes)
		}
		key := strings.TrimPrefix(b.Path, "s3://backups/")
		obj, err := backend.GetObject("backups", key, nil)
		if err != nil {
			t.Fatalf("the object is not in the bucket: %v", err)
		}
		head := make([]byte, 9)
		obj.Contents.Read(head)
		obj.Contents.Close()
		// An encrypted backup starts with Dedie's marker; a plain one
		// with pg_dump's.
		if encrypted := string(head) == "DEDIEBK1\n"; encrypted == plain {
			t.Errorf("plain=%v but the object begins %q", plain, head)
		}
		if err := a.VerifyBackup(ctx, actor, b.ID); err != nil {
			t.Errorf("verify: %v", err)
		}
		if !plain {
			if _, err := a.RestoreBackup(ctx, actor, b.ID, RestoreInput{TargetID: conn.ID, NewDatabase: name + "_back", Confirm: name + "_back"}, true); err != nil {
				t.Fatalf("restore: %v", err)
			}
			in.Name, in.Database = "back", name+"_back"
			back, _ := a.SaveConnection(ctx, actor, "", in)
			res, err := a.Query(ctx, actor, back.ID, QueryRequest{SQL: `SELECT count(*), max(v) FROM t`})
			if err != nil || *res.Results[0].Rows[0][0] != "5000" {
				t.Errorf("restored data: %v %v", res, err)
			}
		}
		if err := a.DeleteBackup(ctx, actor, b.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := backend.GetObject("backups", key, nil); err == nil {
			t.Error("the object is still in the bucket after the backup was deleted")
		}
	}
}
