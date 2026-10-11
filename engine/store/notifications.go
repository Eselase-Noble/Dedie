package store

import (
	"context"
	"strings"
	"time"
)

// Notification is something that happened which people in an organization
// should be told about inside the application: a migration finished, a
// backup failed, a database stopped answering.
type Notification struct {
	ID    int64  `json:"id"`
	OrgID string `json:"-"`
	// UserID, when set, addresses the notification to that user alone.
	// Empty addresses everyone who may see its subject.
	UserID string `json:"-"`
	// Topic groups kinds by the part of the product they belong to; it is
	// what decides who may see the notification.
	Topic string `json:"topic"`
	// Kind says what happened, for example "migration.completed".
	Kind string `json:"kind"`
	// Level is info, success, warning or error.
	Level string `json:"level"`
	Title string `json:"title"`
	Body  string `json:"body"`
	// Link is the page inside the application where the reader can act.
	Link string `json:"link"`
	// ConnectionID and TargetID name the connections the notification is
	// about. Someone who may not use either of them does not see it.
	ConnectionID string `json:"connection_id,omitempty"`
	TargetID     string `json:"target_id,omitempty"`
	JobID        string `json:"job_id,omitempty"`
	// Once, when set, makes the notification unique within the
	// organization: a second one with the same key is not recorded.
	Once      string    `json:"-"`
	CreatedAt time.Time `json:"created_at"`
	// Read is filled in per reader when notifications are listed.
	Read bool `json:"read"`
}

// AddNotification records a notification and reports whether it was new.
// One whose Once key has been used before is dropped.
func (s *Store) AddNotification(ctx context.Context, n Notification) (bool, error) {
	res, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO notifications(org_id,user_id,topic,kind,level,title,body,link,connection_id,target_id,job_id,once_key,created_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		n.OrgID, n.UserID, n.Topic, n.Kind, n.Level, n.Title, n.Body, n.Link, n.ConnectionID, n.TargetID, n.JobID, n.Once, now())
	if err != nil {
		return false, err
	}
	added, err := res.RowsAffected()
	return added > 0, err
}

// NotificationReader describes who is reading, which decides what they are
// shown. Read state is kept per reader, so one person reading a
// notification does not hide it from anyone else.
type NotificationReader struct {
	OrgID  string
	UserID string
	// Topics lists the topics the reader's role lets them see.
	Topics []string
	// HiddenConnections lists connections the reader may not use; a
	// notification about any of them is left out.
	HiddenConnections []string
	// Since leaves out everything older, so that a new account does not
	// begin with a backlog from before it existed. Zero shows it all.
	Since time.Time
}

// NotificationFilter narrows a listing.
type NotificationFilter struct {
	Unread bool
	// Topic keeps one topic; empty keeps them all.
	Topic string
	// Before keeps notifications older than this id, for paging.
	Before int64
	Limit  int
}

// visible builds the condition that selects the notifications a reader
// may see from the table aliased n.
func (r NotificationReader) visible() (string, []any) {
	if len(r.Topics) == 0 {
		// Addressed notifications are still the reader's own.
		return `n.org_id=? AND n.user_id=?`, []any{r.OrgID, r.UserID}
	}
	cond := `n.org_id=? AND (n.user_id=? OR (n.user_id='' AND n.topic IN (` + marks(len(r.Topics)) + `)))`
	args := []any{r.OrgID, r.UserID}
	for _, t := range r.Topics {
		args = append(args, t)
	}
	if len(r.HiddenConnections) > 0 {
		in := marks(len(r.HiddenConnections))
		cond += ` AND n.connection_id NOT IN (` + in + `) AND n.target_id NOT IN (` + in + `)`
		for i := 0; i < 2; i++ {
			for _, id := range r.HiddenConnections {
				args = append(args, id)
			}
		}
	}
	if !r.Since.IsZero() {
		cond += ` AND n.created_at>=?`
		args = append(args, stamp(r.Since))
	}
	return cond, args
}

func marks(n int) string { return strings.TrimSuffix(strings.Repeat("?,", n), ",") }

const unreadBy = ` AND NOT EXISTS (SELECT 1 FROM notification_reads r WHERE r.user_id=? AND r.notification_id=n.id)`

// ListNotifications returns what a reader may see, newest first, each
// marked with whether this reader has read it.
func (s *Store) ListNotifications(ctx context.Context, r NotificationReader, f NotificationFilter) ([]Notification, error) {
	cond, where := r.visible()
	args := append([]any{r.UserID}, where...)
	if f.Unread {
		cond += unreadBy
		args = append(args, r.UserID)
	}
	if f.Topic != "" {
		cond += ` AND n.topic=?`
		args = append(args, f.Topic)
	}
	if f.Before > 0 {
		cond += ` AND n.id<?`
		args = append(args, f.Before)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT n.id,n.org_id,n.user_id,n.topic,n.kind,n.level,n.title,n.body,n.link,n.connection_id,n.target_id,n.job_id,n.created_at,
		EXISTS (SELECT 1 FROM notification_reads r WHERE r.user_id=? AND r.notification_id=n.id)
		FROM notifications n WHERE `+cond+` ORDER BY n.id DESC LIMIT ?`, append(args, f.Limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Notification{}
	for rows.Next() {
		var n Notification
		var created string
		if err := rows.Scan(&n.ID, &n.OrgID, &n.UserID, &n.Topic, &n.Kind, &n.Level, &n.Title, &n.Body, &n.Link, &n.ConnectionID, &n.TargetID, &n.JobID, &created, &n.Read); err != nil {
			return nil, err
		}
		n.CreatedAt = parseTime(created)
		out = append(out, n)
	}
	return out, rows.Err()
}

// CountUnreadNotifications counts what a reader may see and has not read.
func (s *Store) CountUnreadNotifications(ctx context.Context, r NotificationReader) (int, error) {
	cond, args := r.visible()
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM notifications n WHERE `+cond+unreadBy, append(args, r.UserID)...).Scan(&n)
	return n, err
}

// MarkNotificationRead marks one notification as read by a reader. A
// notification the reader may not see is reported as not found.
func (s *Store) MarkNotificationRead(ctx context.Context, r NotificationReader, id int64) error {
	cond, args := r.visible()
	res, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO notification_reads(user_id,notification_id)
		SELECT ?, n.id FROM notifications n WHERE n.id=? AND `+cond, append([]any{r.UserID, id}, args...)...)
	if err != nil {
		return err
	}
	if added, _ := res.RowsAffected(); added > 0 {
		return nil
	}
	// Nothing was inserted: either it was read already, or it is not the
	// reader's to see.
	var found int
	err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM notifications n WHERE n.id=? AND `+cond, append([]any{id}, args...)...).Scan(&found)
	if err == nil && found == 0 {
		return ErrNotFound
	}
	return err
}

// MarkAllNotificationsRead marks everything a reader may see as read and
// returns how many were unread.
func (s *Store) MarkAllNotificationsRead(ctx context.Context, r NotificationReader) (int, error) {
	cond, args := r.visible()
	res, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO notification_reads(user_id,notification_id)
		SELECT ?, n.id FROM notifications n WHERE `+cond, append([]any{r.UserID}, args...)...)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// PruneNotifications deletes notifications older than a time, and beyond
// the newest keep of each organization. Their read marks go with them.
func (s *Store) PruneNotifications(ctx context.Context, before time.Time, keep int) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM notifications WHERE created_at<?`, stamp(before)); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM notifications WHERE id IN (
		SELECT id FROM (SELECT id, row_number() OVER (PARTITION BY org_id ORDER BY id DESC) AS position FROM notifications) WHERE position>?)`, keep)
	return err
}
