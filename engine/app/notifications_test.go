package app

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"dedie/engine/connector"
	"dedie/engine/migrate"
	"dedie/engine/store"
)

// scripted is a pipeline whose preflight and run do what a test tells
// them to. It is registered for a pair of kinds no real pipeline handles.
type scripted struct {
	report  migrate.Report
	run     func(rep migrate.Reporter) (migrate.Outcome, error)
	started int
}

func (p *scripted) Preflight(context.Context, connector.Config, connector.Config, migrate.Config) (*migrate.Report, error) {
	r := p.report
	return &r, nil
}

func (p *scripted) Run(_ context.Context, _ string, _ migrate.Config, _ json.RawMessage, _, _ connector.Config, rep migrate.Reporter) (migrate.Outcome, error) {
	p.started++
	return p.run(rep)
}

type notifyFixture struct {
	t     *testing.T
	ctx   context.Context
	app   *App
	owner Actor
}

func newNotifyFixture(t *testing.T) *notifyFixture {
	t.Helper()
	a, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	ctx := context.Background()
	u, err := a.Setup(ctx, "Acme", "Ada", "ada@example.com", "a-long-enough-password")
	if err != nil {
		t.Fatal(err)
	}
	// Setup returns the user as it was built; reading it back fills in
	// the creation time that notifications are compared with.
	u, err = a.Store.UserByID(ctx, u.OrgID, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	return &notifyFixture{t: t, ctx: ctx, app: a, owner: Actor{User: u, IP: "10.0.0.1"}}
}

func (f *notifyFixture) user(name, role string) Actor {
	f.t.Helper()
	u, err := f.app.CreateUser(f.ctx, f.owner, name, name+"@example.com", "a-long-enough-password", role)
	if err == nil {
		u, err = f.app.Store.UserByID(f.ctx, u.OrgID, u.ID)
	}
	if err != nil {
		f.t.Fatal(err)
	}
	return Actor{User: u, IP: "10.0.0.2"}
}

func (f *notifyFixture) connection(name, kind string) store.Connection {
	f.t.Helper()
	c, err := f.app.SaveConnection(f.ctx, f.owner, "", ConnectionInput{Name: name, Kind: kind, Host: name + ".internal", Database: "shop", Username: "dedie", Password: "secret"})
	if err != nil {
		f.t.Fatal(err)
	}
	return c
}

// titles lists what an actor is shown, oldest first.
func (f *notifyFixture) titles(actor Actor) []string {
	f.t.Helper()
	list, err := f.app.ListNotifications(f.ctx, actor, NotificationQuery{Limit: 100})
	if err != nil {
		f.t.Fatal(err)
	}
	out := make([]string, len(list))
	for i, n := range list {
		out[len(list)-1-i] = n.Title
	}
	return out
}

func (f *notifyFixture) unread(actor Actor) int {
	f.t.Helper()
	n, err := f.app.UnreadNotifications(f.ctx, actor)
	if err != nil {
		f.t.Fatal(err)
	}
	return n
}

func sameTitles(got []string, want ...string) bool {
	return strings.Join(got, "|") == strings.Join(want, "|")
}

func TestNotificationsFollowTheReadersRole(t *testing.T) {
	f := newNotifyFixture(t)
	operator, analyst := f.user("olga", RoleOperator), f.user("ana", RoleAnalyst)
	org := f.owner.User.OrgID

	f.app.Notify(f.ctx, store.Notification{OrgID: org, Kind: NoteMigrationCompleted, Level: LevelSuccess, Title: "migration"})
	f.app.Notify(f.ctx, store.Notification{OrgID: org, Kind: NoteBackupFailed, Level: LevelError, Title: "backup"})
	f.app.Notify(f.ctx, store.Notification{OrgID: org, Kind: NoteConnectionDown, Level: LevelError, Title: "monitoring"})
	f.app.Notify(f.ctx, store.Notification{OrgID: org, UserID: analyst.User.ID, Kind: NoteTokenCreated, Level: LevelInfo, Title: "security"})
	f.app.Notify(f.ctx, store.Notification{OrgID: "another-org", Kind: NoteMigrationCompleted, Level: LevelSuccess, Title: "elsewhere"})

	if got := f.titles(f.owner); !sameTitles(got, "migration", "backup", "monitoring") {
		t.Errorf("owner sees %v", got)
	}
	if got := f.titles(operator); !sameTitles(got, "migration", "backup", "monitoring") {
		t.Errorf("operator sees %v", got)
	}
	// An analyst may look at migrations but takes no backups and has no
	// part in running the servers. The security notice is theirs alone.
	if got := f.titles(analyst); !sameTitles(got, "migration", "security") {
		t.Errorf("analyst sees %v", got)
	}
	if n := f.unread(analyst); n != 2 {
		t.Errorf("analyst has %d unread, want 2", n)
	}

	// A token never sees more than its own role allows, whoever owns it.
	token := f.owner
	token.User.Role, token.Token = RoleViewer, "ci"
	if got := f.titles(token); !sameTitles(got, "migration") {
		t.Errorf("a viewer token of the owner sees %v", got)
	}

	list, err := f.app.ListNotifications(f.ctx, f.owner, NotificationQuery{Topic: "backup"})
	if err != nil || len(list) != 1 || list[0].Title != "backup" || list[0].Topic != "backup" {
		t.Errorf("filter by topic: %v %v", list, err)
	}

	// Someone who joins later does not inherit the backlog.
	late := f.user("lena", RoleOperator)
	if got := f.titles(late); len(got) != 0 {
		t.Errorf("a new user sees %v", got)
	}
}

func TestNotificationsHideRestrictedConnections(t *testing.T) {
	f := newNotifyFixture(t)
	granted, other := f.user("gina", RoleOperator), f.user("otto", RoleOperator)
	open, vault := f.connection("open", "postgres"), f.connection("vault", "postgres")
	if err := f.app.SetConnectionAccess(f.ctx, f.owner, vault.ID, ConnectionAccess{Restricted: true, UserIDs: []string{granted.User.ID}}); err != nil {
		t.Fatal(err)
	}
	org := f.owner.User.OrgID
	f.app.Notify(f.ctx, store.Notification{OrgID: org, Kind: NoteBackupFailed, Level: LevelError, Title: "open backup", ConnectionID: open.ID})
	f.app.Notify(f.ctx, store.Notification{OrgID: org, Kind: NoteBackupFailed, Level: LevelError, Title: "vault backup", ConnectionID: vault.ID})
	f.app.Notify(f.ctx, store.Notification{OrgID: org, Kind: NoteMigrationCompleted, Level: LevelSuccess, Title: "into the vault", ConnectionID: open.ID, TargetID: vault.ID})

	all := []string{"open backup", "vault backup", "into the vault"}
	if got := f.titles(f.owner); !sameTitles(got, all...) {
		t.Errorf("owner sees %v", got)
	}
	if got := f.titles(granted); !sameTitles(got, all...) {
		t.Errorf("granted operator sees %v", got)
	}
	if got := f.titles(other); !sameTitles(got, "open backup") {
		t.Errorf("operator without a grant sees %v", got)
	}
	if n := f.unread(other); n != 1 {
		t.Errorf("operator without a grant has %d unread, want 1", n)
	}

	// What cannot be seen cannot be marked either, and marking everything
	// leaves the hidden ones unread for when access is given.
	hidden, _ := f.app.ListNotifications(f.ctx, f.owner, NotificationQuery{Limit: 1})
	if err := f.app.MarkNotificationRead(f.ctx, other, hidden[0].ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("marking a hidden notification: %v", err)
	}
	if n, err := f.app.MarkAllNotificationsRead(f.ctx, other); err != nil || n != 1 {
		t.Errorf("mark all read: %d %v", n, err)
	}
	if err := f.app.SetConnectionAccess(f.ctx, f.owner, vault.ID, ConnectionAccess{Restricted: false}); err != nil {
		t.Fatal(err)
	}
	if got, n := f.titles(other), f.unread(other); !sameTitles(got, all...) || n != 2 {
		t.Errorf("after the restriction is lifted: sees %v with %d unread", got, n)
	}
}

func TestNotificationReadStateIsPerUser(t *testing.T) {
	f := newNotifyFixture(t)
	operator := f.user("olga", RoleOperator)
	org := f.owner.User.OrgID
	for _, title := range []string{"one", "two", "three"} {
		f.app.Notify(f.ctx, store.Notification{OrgID: org, Kind: NoteMigrationCompleted, Level: LevelSuccess, Title: title})
	}
	list, _ := f.app.ListNotifications(f.ctx, f.owner, NotificationQuery{})
	if len(list) != 3 || list[0].Title != "three" || list[0].Read {
		t.Fatalf("unexpected list: %+v", list)
	}
	for i := 0; i < 2; i++ { // reading twice changes nothing
		if err := f.app.MarkNotificationRead(f.ctx, f.owner, list[0].ID); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.app.MarkNotificationRead(f.ctx, f.owner, 99999); !errors.Is(err, ErrNotFound) {
		t.Errorf("marking a missing notification: %v", err)
	}
	if a, b := f.unread(f.owner), f.unread(operator); a != 2 || b != 3 {
		t.Errorf("unread: owner %d, operator %d; want 2 and 3", a, b)
	}
	unread, _ := f.app.ListNotifications(f.ctx, f.owner, NotificationQuery{Unread: true})
	if len(unread) != 2 || unread[0].Title != "two" {
		t.Errorf("unread only: %+v", unread)
	}
	again, _ := f.app.ListNotifications(f.ctx, f.owner, NotificationQuery{})
	if !again[0].Read || again[1].Read {
		t.Errorf("read marks: %+v", again)
	}
	page, _ := f.app.ListNotifications(f.ctx, f.owner, NotificationQuery{Before: list[0].ID, Limit: 1})
	if len(page) != 1 || page[0].Title != "two" {
		t.Errorf("second page: %+v", page)
	}
	if n, err := f.app.MarkAllNotificationsRead(f.ctx, operator); err != nil || n != 3 {
		t.Errorf("mark all read: %d %v", n, err)
	}
	if a, b := f.unread(f.owner), f.unread(operator); a != 2 || b != 0 {
		t.Errorf("unread after the operator read everything: owner %d, operator %d", a, b)
	}
}

func TestNotificationsAreNotRepeated(t *testing.T) {
	f := newNotifyFixture(t)
	org := f.owner.User.OrgID
	for i := 0; i < 3; i++ {
		f.app.Notify(f.ctx, store.Notification{OrgID: org, Kind: NoteBackupVerifyFailed, Level: LevelError, Title: "verify", Once: "verify/bak_1"})
	}
	f.app.Notify(f.ctx, store.Notification{OrgID: org, Kind: NoteBackupVerifyFailed, Level: LevelError, Title: "verify another", Once: "verify/bak_2"})
	if got := f.titles(f.owner); !sameTitles(got, "verify", "verify another") {
		t.Errorf("got %v", got)
	}
}

func TestMigrationNotifications(t *testing.T) {
	f := newNotifyFixture(t)
	pipeline := &scripted{}
	migrate.Register("postgres", "mongodb", pipeline)
	src, dst := f.connection("shop 16", "postgres"), f.connection("documents", "mongodb")
	job, err := f.app.CreateMigration(f.ctx, f.owner, MigrationInput{Name: "shop to documents", SourceID: src.ID, TargetID: dst.ID})
	if err != nil {
		t.Fatal(err)
	}
	last := func() store.Notification {
		t.Helper()
		list, err := f.app.ListNotifications(f.ctx, f.owner, NotificationQuery{Limit: 1})
		if err != nil || len(list) == 0 {
			t.Fatalf("no notification: %v", err)
		}
		return list[0]
	}
	run := func() {
		t.Helper()
		if err := f.app.RunJobForeground(f.ctx, job.ID); err != nil {
			t.Fatal(err)
		}
	}

	// Preflight finds objects on the target and nobody has said what to
	// do about them. Running it again does not ask again.
	pipeline.report = migrate.Report{Existing: []migrate.ExistingObject{{Kind: "table", Name: "public.orders"}, {Kind: "table", Name: "public.items"}}}
	for i := 0; i < 2; i++ {
		if _, err := f.app.PreflightMigration(f.ctx, f.owner, job.ID); err != nil {
			t.Fatal(err)
		}
	}
	if got := f.titles(f.owner); !sameTitles(got, "Waiting for your decision: shop to documents") {
		t.Fatalf("after preflight: %v", got)
	}
	if n := last(); n.Level != LevelWarning || n.Link != "/migrations/detail/?id="+job.ID || !strings.Contains(n.Body, "2 objects") {
		t.Errorf("decision notification: %+v", n)
	}
	// Once a policy is chosen there is nothing left to decide.
	if _, err := f.app.SetOnExisting(f.ctx, f.owner, job.ID, migrate.ExistingSkip); err != nil {
		t.Fatal(err)
	}
	if _, err := f.app.PreflightMigration(f.ctx, f.owner, job.ID); err != nil {
		t.Fatal(err)
	}

	// The first run fails; only the first line of the error is carried.
	pipeline.run = func(migrate.Reporter) (migrate.Outcome, error) {
		return "", errors.New("connection refused\nDETAIL: more than anyone needs")
	}
	if err := f.app.MigrationAction(f.ctx, f.owner, job.ID, "start"); err != nil {
		t.Fatal(err)
	}
	run()
	if n := last(); n.Title != "Migration failed: shop to documents" || n.Body != "Connection refused." || n.Level != LevelError {
		t.Errorf("failure notification: %+v", n)
	}

	// Resumed, it gives up a cutover, then is paused.
	pipeline.run = func(rep migrate.Reporter) (migrate.Outcome, error) {
		rep.Event("info", "streaming changes")
		rep.Event("error", "cutover cancelled: validation found differences in 2 tables. If the source is still receiving writes, stop them and try again; streaming continues")
		return migrate.Paused, nil
	}
	if err := f.app.MigrationAction(f.ctx, f.owner, job.ID, "resume"); err != nil {
		t.Fatal(err)
	}
	run()
	// A second process claiming the job again must find nothing to run,
	// so nothing is announced twice.
	if err := f.app.RunJobForeground(f.ctx, job.ID); err == nil {
		t.Error("a paused job was run again")
	}

	pipeline.run = func(migrate.Reporter) (migrate.Outcome, error) { return migrate.Completed, nil }
	if err := f.app.MigrationAction(f.ctx, f.owner, job.ID, "resume"); err != nil {
		t.Fatal(err)
	}
	run()

	want := []string{
		"Waiting for your decision: shop to documents",
		"Migration started: shop to documents",
		"Migration failed: shop to documents",
		"Migration resumed: shop to documents",
		"Cutover cancelled: shop to documents",
		"Migration paused: shop to documents",
		"Migration resumed: shop to documents",
		"Migration completed: shop to documents",
	}
	if got := f.titles(f.owner); !sameTitles(got, want...) {
		t.Errorf("notifications:\n got %s\nwant %s", strings.Join(got, "\n     "), strings.Join(want, "\n     "))
	}
	if n := last(); n.Level != LevelSuccess || !strings.HasPrefix(n.Body, "Cutover complete") || n.JobID != job.ID {
		t.Errorf("completion notification: %+v", n)
	}
	list, _ := f.app.ListNotifications(f.ctx, f.owner, NotificationQuery{Limit: 100})
	for _, n := range list {
		if n.Kind == NoteCutoverCancelled && n.Body != "Cutover did not go through: validation found differences in 2 tables. The migration is still streaming changes." {
			t.Errorf("cutover notification body: %q", n.Body)
		}
	}
	if pipeline.started != 3 {
		t.Errorf("the pipeline ran %d times, want 3", pipeline.started)
	}
}

func TestCompletedTextNamesTheMode(t *testing.T) {
	for mode, prefix := range map[string]string{migrate.ModeMigrate: "Cutover complete", migrate.ModeOffline: "Offline copy complete", migrate.ModeSync: "Sync finished"} {
		if got := completedText(migrate.Config{Mode: mode}); !strings.HasPrefix(got, prefix) {
			t.Errorf("%s: %q", mode, got)
		}
	}
}

func TestSecurityNotificationsAreForTheAccountAlone(t *testing.T) {
	f := newNotifyFixture(t)
	if _, err := f.app.CreateToken(f.ctx, f.owner, TokenInput{Name: "ci"}); err != nil {
		t.Fatal(err)
	}
	admin := f.user("ann", RoleAdmin)
	if got := f.titles(f.owner); !sameTitles(got, "API token created: ci") {
		t.Errorf("owner sees %v", got)
	}
	if got := f.titles(admin); len(got) != 0 {
		t.Errorf("another administrator sees %v", got)
	}

	// A sign-in is remarked on only when other sessions are open and none
	// of them came from the same address.
	login := func(ip string) {
		t.Helper()
		if _, err := f.app.Login(f.ctx, LoginInput{Email: "ann@example.com", Password: "a-long-enough-password", IP: ip}); err != nil {
			t.Fatal(err)
		}
	}
	login("10.0.0.7")
	login("10.0.0.7")
	if got := f.titles(admin); len(got) != 0 {
		t.Errorf("after signing in from one address: %v", got)
	}
	login("203.0.113.9")
	login("203.0.113.9")
	if got := f.titles(admin); !sameTitles(got, "New sign-in from 203.0.113.9") {
		t.Errorf("after signing in from a new address: %v", got)
	}
}

func TestPruneNotificationsKeepsTheNewest(t *testing.T) {
	f := newNotifyFixture(t)
	org := f.owner.User.OrgID
	for _, title := range []string{"one", "two", "three", "four"} {
		f.app.Notify(f.ctx, store.Notification{OrgID: org, Kind: NoteMigrationCompleted, Level: LevelSuccess, Title: title})
	}
	list, _ := f.app.ListNotifications(f.ctx, f.owner, NotificationQuery{})
	f.app.MarkNotificationRead(f.ctx, f.owner, list[3].ID)
	if err := f.app.Store.PruneNotifications(f.ctx, f.owner.User.CreatedAt.AddDate(0, 0, -30), 2); err != nil {
		t.Fatal(err)
	}
	if got := f.titles(f.owner); !sameTitles(got, "three", "four") {
		t.Errorf("after pruning: %v", got)
	}
}
