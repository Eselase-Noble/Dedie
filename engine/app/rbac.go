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
	RoleOwner:    append([]Permission{PermManageUsers, PermViewAudit}, operatorPerms...),
	RoleAdmin:    append([]Permission{PermManageUsers, PermViewAudit}, operatorPerms...),
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
