package storage

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"
)

func roundTrip(t *testing.T, s Store, size int) {
	t.Helper()
	ctx := context.Background()
	if err := s.Check(ctx); err != nil {
		t.Fatalf("check: %v", err)
	}
	data := make([]byte, size)
	rand.Read(data)
	w, err := s.Create(ctx, "db-full.dediebak")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(w, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	path := w.Path()
	if !s.Exists(ctx, path) {
		t.Fatalf("%s does not exist after writing", path)
	}
	r, err := s.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(r)
	r.Close()
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("read back %d bytes (%v), want %d", len(got), err, len(data))
	}
	if err := s.Remove(ctx, path); err != nil {
		t.Fatal(err)
	}
	if s.Exists(ctx, path) {
		t.Error("the file still exists after removal")
	}
	if _, err := s.Open(ctx, path); err == nil {
		t.Error("opening a removed file succeeded")
	}

	// An abandoned write leaves nothing behind.
	w, err = s.Create(ctx, "abandoned.dediebak")
	if err != nil {
		t.Fatal(err)
	}
	w.Write([]byte("partial"))
	w.Abort()
	if s.Exists(ctx, w.Path()) {
		t.Error("an aborted file was kept")
	}
}

func TestDir(t *testing.T) { roundTrip(t, Dir{Path: t.TempDir() + "/nested/backups"}, 1<<20) }

func TestS3(t *testing.T) {
	backend := s3mem.New()
	// Over TLS the client sends each part whole, as it does to a real
	// service; over plain HTTP it wraps them in a signed encoding this
	// fake does not decode.
	srv := httptest.NewTLSServer(gofakes3.New(backend).Server())
	defer srv.Close()
	if err := backend.CreateBucket("backups"); err != nil {
		t.Fatal(err)
	}
	cfg := S3Config{Endpoint: srv.URL, Region: "us-east-1", Bucket: "backups", Prefix: "/dedie/prod/", AccessKey: "key", SecretKey: "secret"}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	s, err := newS3(cfg, srv.Client().Transport)
	if err != nil {
		t.Fatal(err)
	}
	// Larger than one part, so the multipart path is exercised.
	roundTrip(t, s, partSize+3<<20)

	w, _ := s.Create(context.Background(), "x")
	w.Write([]byte("x"))
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if want := "s3://backups/dedie/prod/x"; w.Path() != want {
		t.Errorf("path %q, want %q", w.Path(), want)
	}
	if !strings.Contains(cfg.Summary(), "s3://backups/dedie/prod") || strings.Contains(cfg.Summary(), "secret") {
		t.Errorf("summary %q", cfg.Summary())
	}
	for _, bad := range []S3Config{{}, {Endpoint: "x"}, {Endpoint: "x", Bucket: "b"}} {
		if bad.Validate() == nil {
			t.Errorf("%+v was accepted", bad)
		}
	}
}
