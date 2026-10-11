package app

import (
	"context"
	"encoding/json"
	"log"
	"time"

	"dedie/engine/notify"
	"dedie/engine/secure"
	"dedie/engine/store"
)

// Event is something that happened in the engine which an organization may
// want to be told about.
type Event struct {
	Type    string `json:"type"`
	Subject string `json:"subject"`
	Message string `json:"message"`
}

// Event types.
const (
	EventBackupFailed       = "backup.failed"
	EventMigrationFailed    = "migration.failed"
	EventMigrationCompleted = "migration.completed"
	EventMigrationLag       = "migration.lag"
	EventConnectionDown     = "connection.down"
	EventConnectionUp       = "connection.up"
	eventTest               = "test"
)

// EventTypes lists the events a channel can subscribe to, with the text
// shown next to each choice.
var EventTypes = []struct{ Type, Label string }{
	{EventMigrationFailed, "A migration fails"},
	{EventMigrationCompleted, "A migration completes"},
	{EventMigrationLag, "A migration falls behind its source"},
	{EventBackupFailed, "A backup fails"},
	{EventConnectionDown, "A database stops answering"},
	{EventConnectionUp, "A database answers again"},
}

func channelAAD(orgID, id string) []byte { return []byte("alert/" + orgID + "/" + id) }

// emit records an event and delivers it to the organization's channels. It
// returns at once; delivery happens in the background.
func (a *App) emit(orgID string, ev Event) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := a.Store.AddAlertEvent(ctx, orgID, ev.Type, ev.Subject, ev.Message); err != nil {
			log.Printf("alerts: record %s: %v", ev.Type, err)
		}
		channels, err := a.Store.ListAlertChannels(ctx, orgID)
		if err != nil {
			log.Printf("alerts: list channels: %v", err)
			return
		}
		for _, ch := range channels {
			if ch.Enabled && subscribed(ch, ev.Type) {
				a.deliver(ctx, ch, ev)
			}
		}
	}()
}

func subscribed(ch store.AlertChannel, typ string) bool {
	if len(ch.Events) == 0 {
		return true
	}
	for _, t := range ch.Events {
		if t == typ {
			return true
		}
	}
	return false
}

// deliver sends one event through one channel, trying three times before
// giving up, and records the outcome on the channel.
func (a *App) deliver(ctx context.Context, ch store.AlertChannel, ev Event) error {
	raw, err := secure.Open(a.keyring, ch.ConfigEnc, channelAAD(ch.OrgID, ch.ID))
	if err != nil {
		return err
	}
	var cfg notify.Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return err
	}
	org, _ := a.Store.GetOrg(ctx, ch.OrgID)
	msg := notify.Message{Type: ev.Type, Subject: ev.Subject, Text: ev.Message, Organization: org.Name, Time: time.Now().UTC()}
	for attempt := 1; ; attempt++ {
		if err = notify.Send(ctx, ch.Kind, cfg, msg); err == nil || attempt == 3 {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(attempt*5) * time.Second):
		}
	}
	text := ""
	if err != nil {
		text = err.Error()
		log.Printf("alerts: %s via %s: %v", ev.Type, ch.Name, err)
	}
	a.Store.RecordDelivery(context.WithoutCancel(ctx), ch.ID, text)
	return err
}
