package app

import "dedie/engine/store"

// Permission is one thing a role may do.
type Permission string

const (
	PermViewConnections   Permission = "connections.view"
	PermManageConnections Permission = "connections.manage"
	PermQueryRead         Permission = "query.read"
	PermQueryWrite        Permission = "query.write"
	PermViewMigrations    Permission = "migrations.view"
	PermManageMigrations  Permission = "migrations.manage"
	PermServerAdmin       Permission = "server.admin"
	PermBackup            Permission = "backups.create"
	PermRestore           Permission = "backups.restore"
	PermManageUsers       Permission = "users.manage"
	PermViewAudit         Permission = "audit.view"
	PermManageAlerts      Permission = "alerts.manage"
)

// Roles, from most to least privileged.
const (
	RoleOwner    = "owner"
	RoleAdmin    = "admin"
	RoleOperator = "operator"
	RoleAnalyst  = "analyst"
	RoleViewer   = "viewer"
)

var operatorPerms = []Permission{
	PermViewConnections, PermManageConnections, PermQueryRead, PermQueryWrite,
	PermViewMigrations, PermManageMigrations, PermServerAdmin, PermBackup, PermRestore,
}

var rolePerms = map[string][]Permission{
	RoleOwner:    append([]Permission{PermManageUsers, PermViewAudit, PermManageAlerts}, operatorPerms...),
	RoleAdmin:    append([]Permission{PermManageUsers, PermViewAudit, PermManageAlerts}, operatorPerms...),
	RoleOperator: operatorPerms,
	RoleAnalyst:  {PermViewConnections, PermQueryRead, PermViewMigrations},
	RoleViewer:   {PermViewConnections, PermViewMigrations},
}

func validRole(role string) bool {
	_, ok := rolePerms[role]
	return ok
}

// Actor is the authenticated caller of an application method.
type Actor struct {
	User store.User
	IP   string
	// SessionID identifies the browser session the request came through.
	SessionID string
	// Token names the API token used instead of a session, if any.
	Token string
	// TokenID is that token's id.
	TokenID string
	// MustEnroll is set when the organization requires two-factor sign-in
	// and this user has not set it up yet.
	MustEnroll bool
	// Confirm is what the caller typed to confirm this request: the name
	// of the production connection it is about to change. See
	// confirmProduction.
	Confirm string
}

// roleRank orders roles so that one can be compared with another.
var roleRank = map[string]int{RoleViewer: 1, RoleAnalyst: 2, RoleOperator: 3, RoleAdmin: 4, RoleOwner: 5}

// interactive rejects API tokens for actions that manage the account
// itself: a leaked token must not be able to mint more credentials.
func (a Actor) interactive() error {
	if a.Token != "" {
		return &Error{Status: 403, Message: "this action needs an interactive sign-in, not an API token"}
	}
	return nil
}

func (a Actor) Can(p Permission) bool {
	for _, have := range rolePerms[a.User.Role] {
		if have == p {
			return true
		}
	}
	return false
}

// Permissions lists what the actor may do, for the UI to hide what it cannot.
func (a Actor) Permissions() []Permission { return rolePerms[a.User.Role] }

func (a Actor) require(p Permission) error {
	if !a.Can(p) {
		return ErrForbidden
	}
	return nil
}
