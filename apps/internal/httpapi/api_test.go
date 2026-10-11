package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"testing"

	"dedie/engine/app"
)

type client struct {
	t    *testing.T
	base string
	http *http.Client
}

func newClient(t *testing.T, base string) *client {
	jar, _ := cookiejar.New(nil)
	return &client{t: t, base: base, http: &http.Client{Jar: jar}}
}

// do sends a JSON request and returns the status and decoded body.
func (c *client) do(method, path string, body any, headers map[string]string) (int, map[string]any) {
	c.t.Helper()
	var buf bytes.Buffer
	if body != nil {
		json.NewEncoder(&buf).Encode(body)
	}
	req, _ := http.NewRequest(method, c.base+path, &buf)
	if method != http.MethodGet {
		req.Header.Set(csrfHeader, "1")
	}
	for k, v := range headers {
		if v == "" {
			req.Header.Del(k)
		} else {
			req.Header.Set(k, v)
		}
	}
	resp, err := c.http.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestAccessControl(t *testing.T) {
	a, err := app.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	srv := httptest.NewServer(New(a, Options{}))
	defer srv.Close()

	owner := newClient(t, srv.URL)
	expect := func(what string, got, want int) {
		t.Helper()
		if got != want {
			t.Errorf("%s: status %d, want %d", what, got, want)
		}
	}

	status, _ := owner.do("GET", "/api/v1/connections", nil, nil)
	expect("unauthenticated request", status, 401)

	setup := map[string]string{"organization": "Acme", "name": "Ada", "email": "ada@example.com", "password": "a-long-enough-password"}
	status, _ = owner.do("POST", "/api/v1/setup", setup, map[string]string{csrfHeader: ""})
	expect("request without the CSRF header", status, 403)
	status, _ = owner.do("POST", "/api/v1/setup", setup, map[string]string{"Origin": "https://evil.example"})
	expect("request from another origin", status, 403)
	status, _ = owner.do("POST", "/api/v1/setup", map[string]string{"organization": "Acme", "name": "Ada", "email": "ada@example.com", "password": "short"}, nil)
	expect("weak password", status, 400)
	status, body := owner.do("POST", "/api/v1/setup", setup, nil)
	expect("setup", status, 200)
	if user, _ := body["user"].(map[string]any); user["role"] != "owner" || user["password_hash"] != nil {
		t.Errorf("setup returned unexpected user: %v", body["user"])
	}
	status, _ = owner.do("POST", "/api/v1/setup", setup, nil)
	expect("second setup", status, 409)

	status, _ = owner.do("POST", "/api/v1/users", map[string]string{"name": "Vic", "email": "vic@example.com", "password": "another-long-password", "role": "viewer"}, nil)
	expect("create viewer", status, 201)
	status, _ = owner.do("POST", "/api/v1/users", map[string]string{"name": "Ann", "email": "ann@example.com", "password": "another-long-password", "role": "admin"}, nil)
	expect("create admin", status, 201)

	viewer := newClient(t, srv.URL)
	status, _ = viewer.do("POST", "/api/v1/auth/login", map[string]string{"email": "vic@example.com", "password": "wrong"}, nil)
	expect("wrong password", status, 401)
	status, _ = viewer.do("POST", "/api/v1/auth/login", map[string]string{"email": "vic@example.com", "password": "another-long-password"}, nil)
	expect("viewer login", status, 200)
	status, _ = viewer.do("GET", "/api/v1/connections", nil, nil)
	expect("viewer lists connections", status, 200)
	conn := map[string]any{"name": "x", "kind": "postgres", "host": "127.0.0.1", "database": "d", "username": "u", "password": "p"}
	status, _ = viewer.do("POST", "/api/v1/connections", conn, nil)
	expect("viewer creates a connection", status, 403)
	status, _ = viewer.do("GET", "/api/v1/users", nil, nil)
	expect("viewer lists users", status, 403)
	status, _ = viewer.do("GET", "/api/v1/audit", nil, nil)
	expect("viewer reads the audit log", status, 403)

	// An admin must not be able to promote anyone to owner.
	admin := newClient(t, srv.URL)
	admin.do("POST", "/api/v1/auth/login", map[string]string{"email": "ann@example.com", "password": "another-long-password"}, nil)
	status, _ = admin.do("POST", "/api/v1/users", map[string]string{"name": "Eve", "email": "eve@example.com", "password": "another-long-password", "role": "owner"}, nil)
	expect("admin creates an owner", status, 403)

	status, body = owner.do("POST", "/api/v1/connections", conn, nil)
	expect("owner creates a connection", status, 201)
	if _, leaked := body["password"]; leaked || body["secret_enc"] != nil {
		t.Errorf("connection response exposes the secret: %v", body)
	}

	// The last owner cannot be demoted or locked out.
	status, info := owner.do("GET", "/api/v1/auth/me", nil, nil)
	expect("me", status, 200)
	id := info["user"].(map[string]any)["id"].(string)
	status, _ = owner.do("PATCH", "/api/v1/users/"+id, map[string]string{"role": "viewer"}, nil)
	expect("demoting the last owner", status, 409)

	status, body = owner.do("GET", "/api/v1/audit/verify", nil, nil)
	expect("audit verify", status, 200)
	if body["intact"] != true {
		t.Errorf("audit chain not intact: %v", body)
	}

	// Disabling a user ends their session immediately.
	resp, _ := owner.http.Get(srv.URL + "/api/v1/users")
	var list []map[string]any
	json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()
	for _, u := range list {
		if u["email"] == "vic@example.com" {
			status, _ = owner.do("PATCH", "/api/v1/users/"+u["id"].(string), map[string]bool{"disabled": true}, nil)
			expect("disable viewer", status, 200)
		}
	}
	status, _ = viewer.do("GET", "/api/v1/connections", nil, nil)
	expect("disabled user's session", status, 401)

	status, _ = owner.do("POST", "/api/v1/auth/logout", nil, nil)
	expect("logout", status, 200)
	status, _ = owner.do("GET", "/api/v1/auth/me", nil, nil)
	expect("after logout", status, 401)
}
