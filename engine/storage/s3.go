package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// S3Config is how to reach a bucket. It works with Amazon S3 and with
// anything that speaks its protocol: MinIO, Ceph, Cloudflare R2,
// Backblaze B2, Wasabi and others.
type S3Config struct {
	// Endpoint is the service address, such as s3.eu-west-1.amazonaws.com
	// or https://minio.example.com:9000. Without a scheme, HTTPS is used.
	Endpoint string `json:"endpoint"`
	Region   string `json:"region"`
	Bucket   string `json:"bucket"`
	// Prefix is put in front of every object name, like a folder.
	Prefix    string `json:"prefix"`
	AccessKey string `json:"access_key"`
	SecretKey string `json:"secret_key"`
}

// Validate checks the settings and tidies them.
func (c *S3Config) Validate() error {
	c.Endpoint, c.Bucket, c.Region = strings.TrimSpace(c.Endpoint), strings.TrimSpace(c.Bucket), strings.TrimSpace(c.Region)
	c.Prefix = strings.Trim(strings.TrimSpace(c.Prefix), "/")
	c.AccessKey = strings.TrimSpace(c.AccessKey)
	if c.Endpoint == "" {
		return errors.New("enter the storage endpoint, for example s3.eu-west-1.amazonaws.com")
	}
	if c.Bucket == "" {
		return errors.New("enter the bucket name")
	}
	if c.AccessKey == "" || c.SecretKey == "" {
		return errors.New("enter the access key and the secret key")
	}
	if _, _, err := c.host(); err != nil {
		return err
	}
	return nil
}

// host splits the endpoint into the host the client wants and whether to
// use TLS.
func (c S3Config) host() (string, bool, error) {
	raw := c.Endpoint
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", false, fmt.Errorf("%q is not a valid endpoint", c.Endpoint)
	}
	return u.Host, u.Scheme != "http", nil
}

// Summary describes the location without its credentials.
func (c S3Config) Summary() string {
	s := "s3://" + c.Bucket
	if c.Prefix != "" {
		s += "/" + c.Prefix
	}
	return s + " at " + c.Endpoint
}

// S3 stores files as objects in a bucket.
type S3 struct {
	cfg    S3Config
	client *minio.Client
}

// Transport, when set, carries every request to object storage. It exists
// for tests, which point it at a local server.
var Transport http.RoundTripper

// NewS3 prepares a client for the bucket. It does not contact the service.
func NewS3(cfg S3Config) (*S3, error) { return newS3(cfg, Transport) }

// newS3 is NewS3 with a transport of the caller's choosing, which tests
// use to trust a local server's certificate.
func newS3(cfg S3Config, transport http.RoundTripper) (*S3, error) {
	host, secure, err := cfg.host()
	if err != nil {
		return nil, err
	}
	client, err := minio.New(host, &minio.Options{
		Creds: credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""), Secure: secure, Region: cfg.Region, Transport: transport,
	})
	if err != nil {
		return nil, err
	}
	return &S3{cfg: cfg, client: client}, nil
}

// partSize is how much is buffered and sent at a time. Object storage
// accepts at most 10,000 parts per object, so this bounds a single backup
// at about 640 GB.
const partSize = 64 << 20

type s3Writer struct {
	pipe *io.PipeWriter
	done chan error
	path string
}

func (w *s3Writer) Write(p []byte) (int, error) { return w.pipe.Write(p) }
func (w *s3Writer) Path() string                { return w.path }

// Close ends the upload and waits for the service to confirm the object.
func (w *s3Writer) Close() error {
	w.pipe.Close()
	return <-w.done
}

// Abort makes the upload fail, which discards the parts already sent.
func (w *s3Writer) Abort() {
	w.pipe.CloseWithError(errors.New("upload abandoned"))
	<-w.done
}

func (s *S3) key(name string) string {
	if s.cfg.Prefix == "" {
		return name
	}
	return s.cfg.Prefix + "/" + name
}

func (s *S3) Create(ctx context.Context, name string) (Writer, error) {
	key := s.key(name)
	pr, pw := io.Pipe()
	w := &s3Writer{pipe: pw, done: make(chan error, 1), path: "s3://" + s.cfg.Bucket + "/" + key}
	go func() {
		// The size is not known in advance, so the object is sent in
		// parts as the backup is produced.
		_, err := s.client.PutObject(ctx, s.cfg.Bucket, key, pr, -1, minio.PutObjectOptions{PartSize: partSize, ContentType: "application/octet-stream"})
		pr.CloseWithError(err)
		w.done <- friendly(err)
	}()
	return w, nil
}

// objectKey extracts the key from an s3:// address in this bucket.
func (s *S3) objectKey(path string) (string, error) {
	key, ok := strings.CutPrefix(path, "s3://"+s.cfg.Bucket+"/")
	if !ok {
		return "", fmt.Errorf("%s is not in bucket %s", path, s.cfg.Bucket)
	}
	return key, nil
}

func (s *S3) Open(ctx context.Context, path string) (io.ReadCloser, error) {
	key, err := s.objectKey(path)
	if err != nil {
		return nil, err
	}
	obj, err := s.client.GetObject(ctx, s.cfg.Bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, friendly(err)
	}
	// The request is only sent on first use; asking for the object's
	// details now surfaces a missing object here rather than mid-read.
	if _, err := obj.Stat(); err != nil {
		obj.Close()
		return nil, friendly(err)
	}
	return obj, nil
}

func (s *S3) Remove(ctx context.Context, path string) error {
	key, err := s.objectKey(path)
	if err != nil {
		return err
	}
	return friendly(s.client.RemoveObject(ctx, s.cfg.Bucket, key, minio.RemoveObjectOptions{}))
}

func (s *S3) Exists(ctx context.Context, path string) bool {
	key, err := s.objectKey(path)
	if err != nil {
		return false
	}
	_, err = s.client.StatObject(ctx, s.cfg.Bucket, key, minio.StatObjectOptions{})
	return err == nil
}

func (s *S3) Check(ctx context.Context) error {
	key := s.key(".dedie-write-test")
	if _, err := s.client.PutObject(ctx, s.cfg.Bucket, key, strings.NewReader("ok"), 2, minio.PutObjectOptions{}); err != nil {
		return friendly(err)
	}
	return friendly(s.client.RemoveObject(ctx, s.cfg.Bucket, key, minio.RemoveObjectOptions{}))
}

// friendly reduces a service error to what it said.
func friendly(err error) error {
	if err == nil {
		return nil
	}
	var resp minio.ErrorResponse
	if errors.As(err, &resp) && resp.Message != "" {
		return fmt.Errorf("%s (%s)", resp.Message, resp.Code)
	}
	return err
}
