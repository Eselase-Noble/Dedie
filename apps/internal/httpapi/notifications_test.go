package httpapi

import (
	"context"
	"fmt"
	"net/http/httptest"
	"testing"

	"dedie/engine/app"
	"dedie/engine/store"
)

func TestNotificationsAPI(t *testing.T) {
	a, err := app.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	srv := httptest.NewServer(New(a, Options{}))
	defer srv.Close()
	ctx := context.Background()
	expect := func(what string, got, want any) {
		t.Helper()
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("%s: got %v, want %v", what, got, want)
		}
	}
	const password = "a-long-enough-password"
	signIn := func(email string) *client {
		c := newClient(t, srv.URL)
		status, _ := c.do("POST", "/api/v1/auth/login", map[string]string{"email": email, "password": password}, nil)
		expect("sign in as "+email, status, 200)
		return c
	}
	// titles lists what a client is shown, newest first.
	titles := func(c *client, path string) (unread any, out []string) {
		t.Helper()
		status, body := c.do("GET", path, nil, nil)
		expect("GET "+path, status, 200)
		for _, item := range body["items"].([]any) {
			out = append(out, item.(map[string]any)["title"].(string))
		}
		return body["unread"], out
	}

	anonymous := newClient(t, srv.URL)
	status, _ := anonymous.do("GET", "/api/v1/notifications", nil, nil)
	expect("unauthenticated list", status, 401)

	owner := newClient(t, srv.URL)
	owner.do("POST", "/api/v1/setup", map[string]string{"organization": "Acme", "name": "Ada", "email": "ada@example.com", "password": password}, nil)
	for name, role := range map[string]string{"olga": "operator", "otto": "operator", "vic": "viewer"} {
		status, _ = owner.do("POST", "/api/v1/users", map[string]string{"name": name, "email": name + "@example.com", "password": password, "role": role}, nil)
		expect("create "+name, status, 201)
	}
	_, me := owner.do("GET", "/api/v1/auth/me", nil, nil)
	org := me["user"].(map[string]any)["org_id"].(string)

	// A restricted connection that only Olga has been granted.
	conn := map[string]any{"name": "vault", "kind": "postgres", "host": "127.0.0.1", "database": "d", "username": "u", "password": "p"}
	status, body := owner.do("POST", "/api/v1/connections", conn, nil)
	expect("create connection", status, 201)
	vault := body["id"].(string)
	olga, otto, vic := signIn("olga@example.com"), signIn("otto@example.com"), signIn("vic@example.com")
	_, info := olga.do("GET", "/api/v1/auth/me", nil, nil)
	status, _ = owner.do("PUT", "/api/v1/connections/"+vault+"/access", map[string]any{"restricted": true, "user_ids": []string{info["user"].(map[string]any)["id"].(string)}}, nil)
	expect("restrict connection", status, 200)

	a.Notify(ctx, store.Notification{OrgID: org, Kind: app.NoteMigrationCompleted, Level: app.LevelSuccess, Title: "Migration completed: shop 16 to 18",
		Body: "Cutover complete.", Link: "/migrations/detail/?id=mig_1", JobID: "mig_1"})
	a.Notify(ctx, store.Notification{OrgID: org, Kind: app.NoteBackupFailed, Level: app.LevelError, Title: "Backup failed: vault", ConnectionID: vault, Link: "/backups/"})
	a.Notify(ctx, store.Notification{OrgID: org, Kind: app.NoteConnectionDown, Level: app.LevelError, Title: "reports is not answering", Link: "/monitoring/"})
	// The same event reported twice is kept once.
	for i := 0; i < 2; i++ {
		a.Notify(ctx, store.Notification{OrgID: org, Kind: app.NoteMigrationDecision, Level: app.LevelWarning, Title: "Waiting for your decision: shop 16 to 18", Once: "decision/mig_1"})
	}

	unread, got := titles(owner, "/api/v1/notifications")
	expect("owner unread", unread, 4)
	expect("owner list", got, []string{"Waiting for your decision: shop 16 to 18", "reports is not answering", "Backup failed: vault", "Migration completed: shop 16 to 18"})
	unread, got = titles(olga, "/api/v1/notifications")
	expect("granted operator unread", unread, 4)
	expect("granted operator sees the vault", len(got), 4)
	unread, got = titles(otto, "/api/v1/notifications")
	expect("operator without a grant: unread", unread, 3)
	expect("operator without a grant: list", got, []string{"Waiting for your decision: shop 16 to 18", "reports is not answering", "Migration completed: shop 16 to 18"})
	unread, got = titles(vic, "/api/v1/notifications")
	expect("viewer unread", unread, 2)
	expect("viewer list", got, []string{"Waiting for your decision: shop 16 to 18", "Migration completed: shop 16 to 18"})

	// What the interface needs of an item.
	_, body = owner.do("GET", "/api/v1/notifications?topic=migration&limit=1&before=4", nil, nil)
	item := body["items"].([]any)[0].(map[string]any)
	expect("item id", item["id"], 1)
	expect("item kind", item["kind"], "migration.completed")
	expect("item topic", item["topic"], "migration")
	expect("item level", item["level"], "success")
	expect("item link", item["link"], "/migrations/detail/?id=mig_1")
	expect("item read", item["read"], false)
	if item["created_at"] == nil || item["org_id"] != nil || item["user_id"] != nil {
		t.Errorf("unexpected fields: %v", item)
	}
	_, body = owner.do("GET", "/api/v1/notifications?limit=0", nil, nil)
	expect("count alone", fmt.Sprint(body["unread"], len(body["items"].([]any))), "4 0")

	// Reading is each user's own business.
	status, _ = owner.do("POST", "/api/v1/notifications/1/read", nil, map[string]string{csrfHeader: ""})
	expect("mark read without the CSRF header", status, 403)
	status, body = owner.do("POST", "/api/v1/notifications/1/read", nil, nil)
	expect("mark read", status, 200)
	expect("unread after one", body["unread"], 3)
	status, _ = otto.do("POST", "/api/v1/notifications/2/read", nil, nil)
	expect("mark a hidden notification read", status, 404)
	status, _ = owner.do("POST", "/api/v1/notifications/nonsense/read", nil, nil)
	expect("mark a malformed id read", status, 404)
	unread, got = titles(owner, "/api/v1/notifications?unread=1")
	expect("owner unread after one", unread, 3)
	expect("owner unread list", len(got), 3)
	unread, _ = titles(olga, "/api/v1/notifications")
	expect("another user's unread is untouched", unread, 4)
	status, body = olga.do("POST", "/api/v1/notifications/read-all", nil, nil)
	expect("mark all read", status, 200)
	expect("unread after all", body["unread"], 0)
	unread, _ = titles(owner, "/api/v1/notifications")
	expect("owner unread after another user read everything", unread, 3)

	// An API token reads its owner's notifications, within its own role.
	status, body = owner.do("POST", "/api/v1/auth/tokens", map[string]any{"name": "ci", "role": "viewer"}, nil)
	expect("create token", status, 201)
	bearer := map[string]string{"Authorization": "Bearer " + body["token"].(string), csrfHeader: ""}
	status, body = anonymous.do("GET", "/api/v1/notifications", nil, bearer)
	expect("list with a token", status, 200)
	// Creating the token told the account about it; a viewer token sees
	// that and the two migration notifications, one of them read.
	expect("token unread", body["unread"], 2)
	expect("token list", len(body["items"].([]any)), 3)
	expect("newest for the token", body["items"].([]any)[0].(map[string]any)["title"], "API token created: ci")
	status, body = anonymous.do("POST", "/api/v1/notifications/read-all", nil, bearer)
	expect("mark all read with a token", status, 200)
	expect("unread after the token read everything", body["unread"], 0)
	// The token could not see the backup or monitoring notifications, so
	// they are still unread for its owner.
	unread, _ = titles(owner, "/api/v1/notifications")
	expect("owner unread after the token read what it could", unread, 2)
}
