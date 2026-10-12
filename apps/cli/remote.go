package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"dedie/engine/app"
	"dedie/engine/connector"
	"dedie/engine/migrate"
	"dedie/engine/store"
)

// remote runs commands on a Dedie server through its HTTP API. The server
// enforces the role of the signed-in user exactly as it does for the web
// interface.
type remote struct {
	ctx   context.Context
	base  string // server address without a trailing slash
	token string
	http  *http.Client
}

// apiError is an error response from the server.
type apiError struct {
	Status  int
	Message string `json:"error"`
	Code    string `json:"code"`
}

func (e *apiError) Error() string { return e.Message }

// checkServerURL normalizes a server address. Plain HTTP is accepted only
// for this machine: anywhere else the token and every database password
// typed into the CLI would cross the network unencrypted.
func checkServerURL(raw string) (string, error) {
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("%q is not a valid server address", raw)
	}
	switch u.Scheme {
	case "https":
	case "http":
		host := u.Hostname()
		if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return "", errors.New("refusing to use plain http:// for a server on another machine; use https://")
		}
	default:
		return "", fmt.Errorf("unsupported scheme %q; use https://", u.Scheme)
	}
	return u.Scheme + "://" + u.Host, nil
}

// newHTTPClient builds a client that trusts the system's certificate
// authorities plus, when given, a private one.
func newHTTPClient(caFile string) (*http.Client, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, err
		}
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("%s contains no PEM certificate", caFile)
		}
		transport.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	jar, _ := cookiejar.New(nil)
	return &http.Client{Transport: transport, Jar: jar}, nil
}

// send performs one API request. A nil body sends none; out, when not
// nil, receives the decoded JSON response.
func (r *remote) send(method, path string, body, out any) error {
	resp, err := r.raw(method, path, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// raw performs a request and returns the response of a successful one.
func (r *remote) raw(method, path string, body any) (*http.Response, error) {
	var payload io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		payload = bytes.NewReader(b)
	} else if method != http.MethodGet {
		payload = strings.NewReader("{}")
	}
	req, err := http.NewRequestWithContext(r.ctx, method, r.base+"/api/v1"+path, payload)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "dedie-cli")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if r.token != "" {
		req.Header.Set("Authorization", "Bearer "+r.token)
	} else {
		// Signing in uses a short-lived session, which needs the header
		// the server asks of browser requests.
		req.Header.Set("X-Dedie-Request", "1")
	}
	resp, err := r.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("could not reach %s: %w", r.base, err)
	}
	if resp.StatusCode < 300 {
		return resp, nil
	}
	defer resp.Body.Close()
	apiErr := &apiError{Status: resp.StatusCode}
	if json.NewDecoder(resp.Body).Decode(apiErr) != nil || apiErr.Message == "" {
		apiErr.Message = fmt.Sprintf("the server answered %s", resp.Status)
	}
	if resp.StatusCode == http.StatusUnauthorized && r.token != "" {
		apiErr.Message = "the saved sign-in is no longer valid; run `dedie login` again"
	}
	return nil, apiErr
}

func seg(s string) string { return url.PathEscape(s) }

func (r *remote) CreateUser(name, email, password, role string) (u store.User, err error) {
	err = r.send("POST", "/users", map[string]string{"name": name, "email": email, "password": password, "role": role}, &u)
	return
}

func (r *remote) ListConnections() (out []store.Connection, err error) {
	err = r.send("GET", "/connections", nil, &out)
	return
}

func (r *remote) TestInput(in app.ConnectionInput) (info connector.ServerInfo, err error) {
	err = r.send("POST", "/connections/test", in, &info)
	return
}

func (r *remote) SaveConnection(id string, in app.ConnectionInput) (c store.Connection, err error) {
	if id == "" {
		err = r.send("POST", "/connections", in, &c)
	} else {
		err = r.send("PUT", "/connections/"+seg(id), in, &c)
	}
	return
}

func (r *remote) TestConnection(name string) (info connector.ServerInfo, err error) {
	err = r.send("POST", "/connections/"+seg(name)+"/test", nil, &info)
	return
}

func (r *remote) DeleteConnection(name string) error {
	// The API deletes by id; a name is resolved first.
	var c store.Connection
	if err := r.send("GET", "/connections/"+seg(name), nil, &c); err != nil {
		return err
	}
	return r.send("DELETE", "/connections/"+seg(c.ID), nil, nil)
}

func (r *remote) Query(conn string, req app.QueryRequest) (resp app.QueryResponse, err error) {
	err = r.send("POST", "/connections/"+seg(conn)+"/query", req, &resp)
	return
}

func (r *remote) CreateMigration(in app.MigrationInput) (j store.Job, err error) {
	err = r.send("POST", "/migrations", in, &j)
	return
}

func (r *remote) ListMigrations() (out []store.Job, err error) {
	err = r.send("GET", "/migrations", nil, &out)
	return
}

func (r *remote) GetMigration(id string) (d app.MigrationDetail, err error) {
	err = r.send("GET", "/migrations/"+seg(id), nil, &d)
	return
}

func (r *remote) PreflightMigration(id string) (*migrate.Report, error) {
	var report migrate.Report
	err := r.send("POST", "/migrations/"+seg(id)+"/preflight", nil, &report)
	return &report, err
}

func (r *remote) PreviewMigration(id string) (list []migrate.Statement, err error) {
	err = r.send("GET", "/migrations/"+seg(id)+"/preview", nil, &list)
	return
}

func (r *remote) MigrationLeftovers(id string) (list []migrate.Leftover, err error) {
	err = r.send("GET", "/migrations/"+seg(id)+"/leftovers", nil, &list)
	return
}

func (r *remote) SetOnExisting(id, policy string) error {
	return r.send("POST", "/migrations/"+seg(id)+"/on-existing", map[string]string{"policy": policy}, nil)
}

func (r *remote) MigrationAction(id, action string) error {
	return r.send("POST", "/migrations/"+seg(id)+"/actions/"+seg(action), nil, nil)
}

// RunJob waits while the server runs the job. Interrupting the wait does
// not stop the migration; it carries on at the server.
func (r *remote) RunJob(id string) error {
	for {
		d, err := r.GetMigration(id)
		if err != nil {
			return err
		}
		if d.Status != app.StatusQueued && d.Status != app.StatusRunning {
			return nil
		}
		select {
		case <-r.ctx.Done():
			fmt.Fprintln(os.Stderr, "\nStopped watching. The migration continues on the server; see `dedie migrate status "+id+"`.")
			return r.ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

func (r *remote) CreateBackup(in app.BackupInput, wait bool) (b store.Backup, err error) {
	if err = r.send("POST", "/backups", in, &b); err != nil || !wait {
		return
	}
	for b.Status == "running" {
		select {
		case <-r.ctx.Done():
			return b, r.ctx.Err()
		case <-time.After(time.Second):
		}
		list, err := r.ListBackups()
		if err != nil {
			return b, err
		}
		for _, x := range list {
			if x.ID == b.ID {
				b = x
			}
		}
	}
	return b, nil
}

func (r *remote) ListBackups() (out []store.Backup, err error) {
	err = r.send("GET", "/backups", nil, &out)
	return
}

func (r *remote) VerifyBackup(id string) error {
	return r.send("POST", "/backups/"+seg(id)+"/verify", nil, nil)
}

func (r *remote) DeleteBackup(id string) error {
	return r.send("DELETE", "/backups/"+seg(id), nil, nil)
}

func (r *remote) RestoreBackup(id string, in app.RestoreInput, wait bool) (res store.Restore, err error) {
	if err = r.send("POST", "/backups/"+seg(id)+"/restore", in, &res); err != nil || !wait {
		return
	}
	for res.Status == "running" {
		select {
		case <-r.ctx.Done():
			return res, r.ctx.Err()
		case <-time.After(time.Second):
		}
		var list []store.Restore
		if err := r.send("GET", "/restores", nil, &list); err != nil {
			return res, err
		}
		for _, x := range list {
			if x.ID == res.ID {
				res = x
			}
		}
	}
	if res.Status != "completed" {
		return res, errors.New(res.Error)
	}
	return res, nil
}

func (r *remote) ExportBackup(id string) (io.ReadCloser, string, error) {
	resp, err := r.raw("GET", "/backups/"+seg(id)+"/download", nil)
	if err != nil {
		return nil, "", err
	}
	name := id
	if _, after, ok := strings.Cut(resp.Header.Get("Content-Disposition"), `filename="`); ok {
		name = strings.TrimSuffix(after, `"`)
	}
	return resp.Body, name, nil
}

// ExtractBackup downloads a physical backup and unpacks it here: the data
// directory is wanted on the machine where the engineer is recovering.
func (r *remote) ExtractBackup(id, dir string) (int, error) {
	src, _, err := r.ExportBackup(id)
	if err != nil {
		return 0, err
	}
	return extractDownloaded(src, dir)
}

func (r *remote) ListLocations() (string, []store.Location, error) {
	var out struct {
		Default   string           `json:"default_path"`
		Locations []store.Location `json:"locations"`
	}
	err := r.send("GET", "/backup-locations", nil, &out)
	return out.Default, out.Locations, err
}

func (r *remote) CreateLocation(in app.LocationInput) (l store.Location, err error) {
	err = r.send("POST", "/backup-locations", in, &l)
	return
}

func (r *remote) DeleteLocation(name string) error {
	return r.send("DELETE", "/backup-locations/"+seg(name), nil, nil)
}

func (r *remote) SaveSchedule(id string, in app.ScheduleInput) (sc store.Schedule, err error) {
	if id == "" {
		err = r.send("POST", "/schedules", in, &sc)
	} else {
		err = r.send("PUT", "/schedules/"+seg(id), in, &sc)
	}
	return
}

func (r *remote) ListSchedules() (out []store.Schedule, err error) {
	err = r.send("GET", "/schedules", nil, &out)
	return
}

func (r *remote) DeleteSchedule(id string) error {
	return r.send("DELETE", "/schedules/"+seg(id), nil, nil)
}

func (r *remote) ListAudit(before int64, limit int) (out []store.AuditEntry, err error) {
	err = r.send("GET", "/audit?before="+strconv.FormatInt(before, 10)+"&limit="+strconv.Itoa(limit), nil, &out)
	return
}

func (r *remote) VerifyAudit() (v app.AuditVerification, err error) {
	err = r.send("GET", "/audit/verify", nil, &v)
	return
}

func (r *remote) ListNotifications(unreadOnly bool, limit int) (int, []store.Notification, error) {
	path := "/notifications?limit=" + strconv.Itoa(limit)
	if unreadOnly {
		path += "&unread=1"
	}
	var out struct {
		Unread int                  `json:"unread"`
		Items  []store.Notification `json:"items"`
	}
	err := r.send("GET", path, nil, &out)
	return out.Unread, out.Items, err
}

func (r *remote) ReadAllNotifications() error {
	return r.send("POST", "/notifications/read-all", nil, nil)
}
