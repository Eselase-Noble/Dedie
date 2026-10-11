package app

import (
	"context"
	"strings"

	"dedie/engine/secure"
	"dedie/engine/store"
)

// SavedQueryInput is a statement to keep.
type SavedQueryInput struct {
	Name   string `json:"name"`
	SQL    string `json:"sql"`
	Kind   string `json:"kind"`
	Shared bool   `json:"shared"`
}

func (a *App) ListSavedQueries(ctx context.Context, actor Actor) ([]store.SavedQuery, error) {
	if err := actor.require(PermQueryRead); err != nil {
		return nil, err
	}
	return a.Store.ListQueries(ctx, actor.User.OrgID, actor.User.ID)
}

// SaveQuery keeps a statement, or updates the one named by id. A saved
// query can be changed by the person who saved it; a shared one also by an
// administrator.
func (a *App) SaveQuery(ctx context.Context, actor Actor, id string, in SavedQueryInput) (store.SavedQuery, error) {
	if err := actor.require(PermQueryRead); err != nil {
		return store.SavedQuery{}, err
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" || len(in.Name) > 120 {
		return store.SavedQuery{}, invalid("give the query a name of up to 120 characters")
	}
	if strings.TrimSpace(in.SQL) == "" {
		return store.SavedQuery{}, invalid("there is no statement to save")
	}
	if len(in.SQL) > 200_000 {
		return store.SavedQuery{}, invalid("the statement is too long to save")
	}
	q := store.SavedQuery{ID: secure.NewID("qry"), OrgID: actor.User.OrgID, UserID: actor.User.ID, Owner: actor.User.Email}
	if id != "" {
		existing, err := a.ownedQuery(ctx, actor, id)
		if err != nil {
			return q, err
		}
		q = existing
	}
	q.Name, q.SQL, q.Kind, q.Shared = in.Name, in.SQL, in.Kind, in.Shared
	if err := a.Store.SaveQuery(ctx, q); err != nil {
		return q, err
	}
	return a.Store.GetQuery(ctx, q.OrgID, q.ID)
}

func (a *App) DeleteSavedQuery(ctx context.Context, actor Actor, id string) error {
	q, err := a.ownedQuery(ctx, actor, id)
	if err != nil {
		return err
	}
	return a.Store.DeleteQuery(ctx, q.OrgID, q.ID)
}

func (a *App) ownedQuery(ctx context.Context, actor Actor, id string) (store.SavedQuery, error) {
	q, err := a.Store.GetQuery(ctx, actor.User.OrgID, id)
	if err != nil {
		return q, wrap(err)
	}
	switch {
	case q.UserID == actor.User.ID:
	case q.Shared && actor.Can(PermManageUsers):
	case q.Shared:
		return q, ErrForbidden
	default:
		// Someone else's private query: do not confirm that it exists.
		return q, ErrNotFound
	}
	return q, nil
}
