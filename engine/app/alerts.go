package app

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"dedie/engine/notify"
	"dedie/engine/secure"
	"dedie/engine/store"
)

// ChannelInput is what an administrator submits to create or change an
// alert channel. On update, secrets left empty keep their stored values.
type ChannelInput struct {
	Name    string        `json:"name"`
	Kind    string        `json:"kind"`
	Config  notify.Config `json:"config"`
	Events  []string      `json:"events"`
	Enabled bool          `json:"enabled"`
}

func (a *App) ListAlertChannels(ctx context.Context, actor Actor) ([]store.AlertChannel, error) {
	if err := actor.require(PermServerAdmin); err != nil {
		return nil, err
	}
	return a.Store.ListAlertChannels(ctx, actor.User.OrgID)
}

func (a *App) ListAlertEvents(ctx context.Context, actor Actor, limit int) ([]store.AlertEvent, error) {
	if err := actor.require(PermServerAdmin); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	return a.Store.ListAlertEvents(ctx, actor.User.OrgID, limit)
}

// SaveAlertChannel creates a channel, or updates the one named by id.
func (a *App) SaveAlertChannel(ctx context.Context, actor Actor, id string, in ChannelInput) (store.AlertChannel, error) {
	if err := actor.require(PermManageAlerts); err != nil {
		return store.AlertChannel{}, err
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" {
		return store.AlertChannel{}, invalid("give the channel a name")
	}
	known := map[string]bool{}
	for _, t := range EventTypes {
		known[t.Type] = true
	}
	for _, t := range in.Events {
		if !known[t] {
			return store.AlertChannel{}, invalid("unknown event %q", t)
		}
	}
	ch := store.AlertChannel{ID: secure.NewID("alc"), OrgID: actor.User.OrgID, Kind: in.Kind, CreatedBy: actor.User.Email}
	action := "alert.channel_create"
	if id != "" {
		existing, err := a.Store.GetAlertChannel(ctx, actor.User.OrgID, id)
		if err != nil {
			return ch, wrap(err)
		}
		if existing.Kind != in.Kind {
			return ch, invalid("the kind of a channel cannot be changed")
		}
		// Secrets are never sent back to the browser, so an edit that
		// leaves them blank means "unchanged".
		var old notify.Config
		if raw, err := secure.Open(a.keyring, existing.ConfigEnc, channelAAD(existing.OrgID, existing.ID)); err == nil {
			_ = json.Unmarshal(raw, &old)
		}
		if in.Config.URL == "" {
			in.Config.URL = old.URL
		}
		if in.Config.Secret == "" {
			in.Config.Secret = old.Secret
		}
		if in.Config.Password == "" {
			in.Config.Password = old.Password
		}
		ch, action = existing, "alert.channel_update"
	}
	if err := in.Config.Validate(in.Kind); err != nil {
		return ch, invalid("%s", err.Error())
	}
	raw, _ := json.Marshal(in.Config)
	sealed, err := secure.Seal(a.keyring, raw, channelAAD(ch.OrgID, ch.ID))
	if err != nil {
		return ch, err
	}
	ch.Name, ch.ConfigEnc, ch.Summary, ch.Events, ch.Enabled = in.Name, sealed, in.Config.Summary(in.Kind), in.Events, in.Enabled
	if err := a.Store.SaveAlertChannel(ctx, ch); err != nil {
		return ch, invalid("%s", err.Error())
	}
	a.audit(ctx, actor, action, ch.ID, map[string]any{"name": ch.Name, "kind": ch.Kind, "destination": ch.Summary, "events": ch.Events, "enabled": ch.Enabled})
	return ch, nil
}

func (a *App) DeleteAlertChannel(ctx context.Context, actor Actor, id string) error {
	if err := actor.require(PermManageAlerts); err != nil {
		return err
	}
	ch, err := a.Store.GetAlertChannel(ctx, actor.User.OrgID, id)
	if err != nil {
		return wrap(err)
	}
	if err := a.Store.DeleteAlertChannel(ctx, ch.OrgID, ch.ID); err != nil {
		return err
	}
	a.audit(ctx, actor, "alert.channel_delete", ch.ID, map[string]string{"name": ch.Name})
	return nil
}

// TestAlertChannel sends a test message and reports whether it was
// accepted, so a channel can be checked before it is relied on.
func (a *App) TestAlertChannel(ctx context.Context, actor Actor, id string) error {
	if err := actor.require(PermManageAlerts); err != nil {
		return err
	}
	ch, err := a.Store.GetAlertChannel(ctx, actor.User.OrgID, id)
	if err != nil {
		return wrap(err)
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	ev := Event{Type: eventTest, Subject: "Test alert from Dedie", Message: "This is a test sent by " + actor.User.Email + ". If you can read it, the channel works."}
	raw, err := secure.Open(a.keyring, ch.ConfigEnc, channelAAD(ch.OrgID, ch.ID))
	if err != nil {
		return err
	}
	var cfg notify.Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return err
	}
	org, _ := a.Store.GetOrg(ctx, ch.OrgID)
	err = notify.Send(ctx, ch.Kind, cfg, notify.Message{Type: ev.Type, Subject: ev.Subject, Text: ev.Message, Organization: org.Name, Time: time.Now().UTC()})
	text := ""
	if err != nil {
		text = err.Error()
	}
	a.Store.RecordDelivery(context.WithoutCancel(ctx), ch.ID, text)
	if err != nil {
		return &Error{Status: 502, Message: "the test alert was not delivered: " + text}
	}
	return nil
}
