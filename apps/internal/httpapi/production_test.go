package httpapi

import (
	"net/http/httptest"
	"net/url"
	"testing"

	"dedie/engine/app"
)

// The name typed to confirm an action on a production connection travels
// in a header, percent-encoded so that any name survives the journey.
func TestProductionConfirmationHeader(t *testing.T) {
	a, err := app.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	srv := httptest.NewServer(New(a, Options{}))
	defer srv.Close()
	c := newClient(t, srv.URL)
	if status, _ := c.do("POST", "/api/v1/setup", map[string]string{"organization": "Acme", "name": "Ada", "email": "ada@example.com", "password": "a-long-enough-password"}, nil); status != 200 {
		t.Fatalf("setup: %d", status)
	}

	const name = "Abrechnung (März)"
	conn := map[string]any{"name": name, "kind": "postgres", "host": "127.0.0.1", "port": 1, "database": "d", "username": "u", "password": "p", "sslmode": "disable", "environment": "production"}
	status, body := c.do("POST", "/api/v1/connections", conn, nil)
	if status != 201 || body["environment"] != "production" {
		t.Fatalf("creating a production connection: %d %v", status, body)
	}
	path := "/api/v1/connections/" + body["id"].(string)

	conn["environment"], conn["password"] = "development", ""
	status, body = c.do("PUT", path, conn, nil)
	if status != 428 || body["code"] != app.CodeConfirmProduction {
		t.Fatalf("taking the label off without the name: %d %v", status, body)
	}
	if status, _ = c.do("PUT", path, conn, map[string]string{confirmHeader: "something-else"}); status != 428 {
		t.Errorf("taking the label off with another name: %d", status)
	}
	// Ending a session is refused before the server is contacted.
	if status, body = c.do("POST", path+"/backends/7/cancel?terminate=1", nil, nil); status != 428 || body["code"] != app.CodeConfirmProduction {
		t.Errorf("ending a session without the name: %d %v", status, body)
	}
	if status, _ = c.do("POST", path+"/backends/7/cancel?terminate=1", nil, map[string]string{confirmHeader: url.PathEscape(name)}); status != 502 {
		t.Errorf("ending a session with the name: status %d, want the attempt to connect", status)
	}

	status, body = c.do("PUT", path, conn, map[string]string{confirmHeader: url.PathEscape(name)})
	if status != 200 || body["environment"] != "development" {
		t.Errorf("taking the label off with the name: %d %v", status, body)
	}
	if status, _ = c.do("POST", "/api/v1/connections", map[string]any{"name": "x", "kind": "postgres", "host": "h", "database": "d", "username": "u", "password": "p", "environment": "live"}, nil); status != 400 {
		t.Errorf("an unknown environment: %d", status)
	}
}
