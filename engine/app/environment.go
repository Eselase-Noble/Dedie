package app

import (
	"context"

	"dedie/engine/store"
)

// The environments a connection can be labelled with. An empty label means
// nobody has said, and is treated like anything that is not production.
const (
	EnvProduction  = "production"
	EnvStaging     = "staging"
	EnvDevelopment = "development"
)

func validEnvironment(env string) bool {
	switch env {
	case "", EnvProduction, EnvStaging, EnvDevelopment:
		return true
	}
	return false
}

// CodeConfirmProduction marks the error that asks for a production
// connection's name, so that a front end can ask for it and try again.
const CodeConfirmProduction = "confirm_production"

// confirmProduction stands between an action that destroys or disrupts and
// a connection labelled production. The caller has to have typed the
// connection's name, which arrives in actor.Confirm; anything else is
// refused with an error that says what to type. Other connections pass
// untouched.
//
// This is a guard against acting on the wrong database, not an access
// control: the actor's role and the connection's own rules have already
// been checked by the time it is called, and still apply. what describes
// the action for the refusal and for the audit log, as in "restore a backup
// over it".
func (a *App) confirmProduction(ctx context.Context, actor Actor, c store.Connection, action, what string) error {
	if c.Environment != EnvProduction {
		return nil
	}
	if actor.Confirm != c.Name {
		return &Error{Status: 428, Code: CodeConfirmProduction,
			Message: c.Name + " is a production connection. Type its name to " + what + "."}
	}
	a.audit(ctx, actor, "production.confirm", c.ID, map[string]string{"connection": c.Name, "action": action})
	return nil
}
