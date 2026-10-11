package app

import (
	"context"
	"log"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"dedie/engine/store"
)

// Notification levels.
const (
	LevelInfo    = "info"
	LevelSuccess = "success"
	LevelWarning = "warning"
	LevelError   = "error"
)

// Notification kinds. The part before the dot is the topic, which decides
// who may see the notification.
const (
	NoteMigrationStarted   = "migration.started"
	NoteMigrationPaused    = "migration.paused"
	NoteMigrationCompleted = "migration.completed"
	NoteMigrationFailed    = "migration.failed"
	NoteMigrationAborted   = "migration.aborted"
	NoteCutoverCancelled   = "migration.cutover_cancelled"
	NoteMigrationDecision  = "migration.decision"
	NoteMigrationLag       = "migration.lag"

	NoteBackupCompleted    = "backup.completed"
	NoteBackupFailed       = "backup.failed"
	NoteBackupVerifyFailed = "backup.verify_failed"
	NoteRestoreCompleted   = "backup.restore_completed"
	NoteRestoreFailed      = "backup.restore_failed"
	NoteScheduleFailed     = "backup.schedule_failed"
	NoteArchiverStopped    = "backup.archiver_stopped"

	NoteConnectionDown = "monitoring.connection_down"
	NoteConnectionUp   = "monitoring.connection_up"

	NoteTokenCreated = "security.token_created"
	NoteNewSignIn    = "security.sign_in"
)

// NotificationTopics lists the topics with the permission each needs and
// the text shown for it. Security notifications are only ever addressed
// to one user, so no permission opens them to anyone else.
var NotificationTopics = []struct {
	Topic, Label string
	Perm         Permission
}{
	{"migration", "Migrations", PermViewMigrations},
	{"backup", "Backups", PermBackup},
	{"monitoring", "Monitoring", PermServerAdmin},
	{"security", "Security", ""},
}

const (
	// notificationRetention is how long a notification is kept.
	notificationRetention = 30 * 24 * time.Hour
	// notificationCap bounds how many are kept per organization, however
	// recent, so a flapping database cannot grow the table without limit.
	notificationCap = 2000
)

// Notify records a notification for the people of an organization who may
// see its subject, or for one user when UserID is set. It never fails the
// work that raised it: an error is logged and dropped.
func (a *App) Notify(ctx context.Context, n store.Notification) {
	n.Topic, _, _ = strings.Cut(n.Kind, ".")
	n.Title, n.Body = clip(strings.TrimSpace(n.Title), 200), sentence(clip(strings.TrimSpace(n.Body), 500))
	if _, err := a.Store.AddNotification(context.WithoutCancel(ctx), n); err != nil {
		log.Printf("notifications: record %s: %v", n.Kind, err)
	}
}

// firstLine reduces an error to its first line, which is what fits in a
// notification; the page it links to shows the rest.
func firstLine(err error) string { return firstLineOf(err.Error()) }

func firstLineOf(text string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(text), "\n")
	return strings.TrimRight(strings.TrimSpace(line), ".")
}

// sentence makes a body read as one: database errors arrive in lower case
// and without a full stop.
func sentence(s string) string {
	if s == "" {
		return s
	}
	if first, size := utf8.DecodeRuneInString(s); unicode.IsLower(first) {
		s = string(unicode.ToUpper(first)) + s[size:]
	}
	if !strings.ContainsRune(".!?", rune(s[len(s)-1])) {
		s += "."
	}
	return s
}

// reader works out what the actor may be shown: the topics their role
// opens, minus anything about a restricted connection they have not been
// granted. These are the rules the rest of the application applies to the
// same subjects.
func (a *App) reader(ctx context.Context, actor Actor) (store.NotificationReader, error) {
	r := store.NotificationReader{OrgID: actor.User.OrgID, UserID: actor.User.ID, Since: actor.User.CreatedAt}
	for _, t := range NotificationTopics {
		if t.Perm != "" && actor.Can(t.Perm) {
			r.Topics = append(r.Topics, t.Topic)
		}
	}
	if actor.Can(PermManageUsers) {
		return r, nil
	}
	conns, err := a.Store.ListConnections(ctx, actor.User.OrgID)
	if err != nil {
		return r, err
	}
	grants, err := a.Store.UserGrants(ctx, actor.User.ID)
	if err != nil {
		return r, err
	}
	for _, c := range conns {
		if c.Restricted && !grants[c.ID] {
			r.HiddenConnections = append(r.HiddenConnections, c.ID)
		}
	}
	return r, nil
}

// NotificationQuery narrows a listing of notifications.
type NotificationQuery struct {
	// Unread keeps only what the caller has not read.
	Unread bool
	// Topic keeps one topic: migration, backup, monitoring or security.
	Topic string
	// Before keeps notifications older than this id, for the next page.
	Before int64
	// Limit defaults to 30 and is capped at 200.
	Limit int
}

// ListNotifications returns the notifications the actor may see, newest
// first, each marked with whether the actor has read it.
func (a *App) ListNotifications(ctx context.Context, actor Actor, q NotificationQuery) ([]store.Notification, error) {
	r, err := a.reader(ctx, actor)
	if err != nil {
		return nil, err
	}
	if q.Limit <= 0 {
		q.Limit = 30
	}
	if q.Limit > 200 {
		q.Limit = 200
	}
	return a.Store.ListNotifications(ctx, r, store.NotificationFilter{Unread: q.Unread, Topic: q.Topic, Before: q.Before, Limit: q.Limit})
}

// UnreadNotifications counts the notifications the actor has not read.
func (a *App) UnreadNotifications(ctx context.Context, actor Actor) (int, error) {
	r, err := a.reader(ctx, actor)
	if err != nil {
		return 0, err
	}
	return a.Store.CountUnreadNotifications(ctx, r)
}

// MarkNotificationRead marks one notification as read by the actor alone.
func (a *App) MarkNotificationRead(ctx context.Context, actor Actor, id int64) error {
	r, err := a.reader(ctx, actor)
	if err != nil {
		return err
	}
	return wrap(a.Store.MarkNotificationRead(ctx, r, id))
}

// MarkAllNotificationsRead marks everything the actor may see as read and
// returns how many were unread.
func (a *App) MarkAllNotificationsRead(ctx context.Context, actor Actor) (int, error) {
	r, err := a.reader(ctx, actor)
	if err != nil {
		return 0, err
	}
	return a.Store.MarkAllNotificationsRead(ctx, r)
}

// pruneNotifications applies the retention rules. The monitor calls it on
// every pass.
func (a *App) pruneNotifications(ctx context.Context) {
	if err := a.Store.PruneNotifications(ctx, time.Now().Add(-notificationRetention), notificationCap); err != nil && ctx.Err() == nil {
		log.Printf("notifications: prune: %v", err)
	}
}

// migrationLink is the page of one migration.
func migrationLink(jobID string) string { return "/migrations/detail/?id=" + jobID }

// notifyJob records a notification about a migration job.
func (a *App) notifyJob(ctx context.Context, j store.Job, kind, level, title, body string) {
	a.Notify(ctx, store.Notification{OrgID: j.OrgID, Kind: kind, Level: level, Title: title + ": " + j.Name, Body: body,
		Link: migrationLink(j.ID), ConnectionID: j.SourceID, TargetID: j.TargetID, JobID: j.ID})
}
