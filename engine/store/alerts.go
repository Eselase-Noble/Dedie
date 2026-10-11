package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// AlertChannel is somewhere alerts are sent: a webhook, a chat room or a
// list of email addresses. Its settings are stored sealed because they
// hold credentials.
type AlertChannel struct {
	ID        string `json:"id"`
	OrgID     string `json:"-"`
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	ConfigEnc []byte `json:"-"`
	// Summary describes the destination without its secrets, for display.
	Summary string `json:"summary"`
	// Events lists the event types delivered; empty means all of them.
	Events     []string  `json:"events"`
	Enabled    bool      `json:"enabled"`
	LastSentAt time.Time `json:"last_sent_at"`
	LastError  string    `json:"last_error"`
	CreatedBy  string    `json:"created_by"`
	CreatedAt  time.Time `json:"created_at"`
}

const channelCols = `id,org_id,name,kind,config_enc,summary,events,enabled,last_sent_at,last_error,created_by,created_at`

func scanChannel(row interface{ Scan(...any) error }) (AlertChannel, error) {
	var c AlertChannel
	var events, sent, created string
	err := row.Scan(&c.ID, &c.OrgID, &c.Name, &c.Kind, &c.ConfigEnc, &c.Summary, &events, &c.Enabled, &sent, &c.LastError, &c.CreatedBy, &created)
	c.LastSentAt, c.CreatedAt = parseTime(sent), parseTime(created)
	c.Events = []string{}
	_ = json.Unmarshal([]byte(events), &c.Events)
	return c, one(err)
}

func (s *Store) SaveAlertChannel(ctx context.Context, c AlertChannel) error {
	events, _ := json.Marshal(append([]string{}, c.Events...))
	_, err := s.db.ExecContext(ctx, `INSERT INTO alert_channels(id,org_id,name,kind,config_enc,summary,events,enabled,created_by,created_at) VALUES(?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET name=excluded.name,config_enc=excluded.config_enc,summary=excluded.summary,events=excluded.events,enabled=excluded.enabled`,
		c.ID, c.OrgID, c.Name, c.Kind, c.ConfigEnc, c.Summary, string(events), c.Enabled, c.CreatedBy, now())
	if err != nil && strings.Contains(err.Error(), "UNIQUE") {
		return errors.New("an alert channel with that name already exists")
	}
	return err
}

func (s *Store) GetAlertChannel(ctx context.Context, orgID, id string) (AlertChannel, error) {
	return scanChannel(s.db.QueryRowContext(ctx, `SELECT `+channelCols+` FROM alert_channels WHERE org_id=? AND id=?`, orgID, id))
}

func (s *Store) ListAlertChannels(ctx context.Context, orgID string) ([]AlertChannel, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+channelCols+` FROM alert_channels WHERE org_id=? ORDER BY name`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AlertChannel{}
	for rows.Next() {
		c, err := scanChannel(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) DeleteAlertChannel(ctx context.Context, orgID, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM alert_channels WHERE org_id=? AND id=?`, orgID, id)
	return err
}

// RecordDelivery notes the outcome of the latest send through a channel.
func (s *Store) RecordDelivery(ctx context.Context, id, errMsg string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE alert_channels SET last_sent_at=?, last_error=? WHERE id=?`, now(), errMsg, id)
	return err
}

// AlertEvent is one alert that was raised.
type AlertEvent struct {
	ID      int64     `json:"id"`
	Time    time.Time `json:"time"`
	Type    string    `json:"type"`
	Subject string    `json:"subject"`
	Message string    `json:"message"`
}

// AddAlertEvent records an alert and trims the history to the most recent
// thousand per organization.
func (s *Store) AddAlertEvent(ctx context.Context, orgID, typ, subject, message string) error {
	if _, err := s.db.ExecContext(ctx, `INSERT INTO alert_events(org_id,ts,type,subject,message) VALUES(?,?,?,?,?)`, orgID, now(), typ, subject, message); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM alert_events WHERE org_id=? AND id <= (SELECT id FROM alert_events WHERE org_id=? ORDER BY id DESC LIMIT 1 OFFSET 1000)`, orgID, orgID)
	return err
}

func (s *Store) ListAlertEvents(ctx context.Context, orgID string, limit int) ([]AlertEvent, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,ts,type,subject,message FROM alert_events WHERE org_id=? ORDER BY id DESC LIMIT ?`, orgID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AlertEvent{}
	for rows.Next() {
		var e AlertEvent
		var ts string
		if err := rows.Scan(&e.ID, &ts, &e.Type, &e.Subject, &e.Message); err != nil {
			return nil, err
		}
		e.Time = parseTime(ts)
		out = append(out, e)
	}
	return out, rows.Err()
}

// HealthSample is one reachability check of a connection.
type HealthSample struct {
	Time      time.Time `json:"time"`
	Online    bool      `json:"online"`
	LatencyMS int64     `json:"latency_ms"`
	Error     string    `json:"error,omitempty"`
}

func (s *Store) AddHealthSample(ctx context.Context, connID string, online bool, latencyMS int64, errMsg string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO health_samples(connection_id,ts,online,latency_ms,error) VALUES(?,?,?,?,?)`, connID, now(), online, latencyMS, errMsg)
	return err
}

// HealthSamples returns a connection's checks since a time, oldest first.
func (s *Store) HealthSamples(ctx context.Context, connID string, since time.Time) ([]HealthSample, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT ts,online,latency_ms,error FROM health_samples WHERE connection_id=? AND ts>=? ORDER BY ts`, connID, stamp(since))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []HealthSample{}
	for rows.Next() {
		var h HealthSample
		var ts string
		if err := rows.Scan(&ts, &h.Online, &h.LatencyMS, &h.Error); err != nil {
			return nil, err
		}
		h.Time = parseTime(ts)
		out = append(out, h)
	}
	return out, rows.Err()
}

// PruneHealth deletes checks older than a time.
func (s *Store) PruneHealth(ctx context.Context, before time.Time) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM health_samples WHERE ts<?`, stamp(before))
	return err
}

// OrgIDs lists every organization.
func (s *Store) OrgIDs(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM orgs`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
