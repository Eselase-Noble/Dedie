package oraora

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"dedie/engine/connector/oracle"
)

// target is what the pipeline needs to know about the receiving server
// before it decides anything.
type target struct {
	// User is the schema of the account the job connects with.
	User string
	// Schemas holds, of the source schemas in scope, those that exist on
	// the target under the same name.
	Schemas map[string]bool
	Privs   map[string]bool
}

// anyPrivs are what the account must hold to build a schema that is not
// its own, and to copy and apply rows there.
var anyPrivs = []string{
	"CREATE ANY TABLE", "ALTER ANY TABLE", "DROP ANY TABLE", "COMMENT ANY TABLE",
	"SELECT ANY TABLE", "INSERT ANY TABLE", "UPDATE ANY TABLE", "DELETE ANY TABLE",
	"CREATE ANY INDEX", "CREATE ANY SEQUENCE", "ALTER ANY SEQUENCE", "CREATE ANY VIEW", "CREATE ANY SYNONYM",
	"CREATE ANY PROCEDURE", "ALTER ANY PROCEDURE", "CREATE ANY TYPE", "CREATE ANY TRIGGER", "ALTER ANY TRIGGER",
}

// ownPrivs are what it takes to build in the account's own schema.
// Creating a table covers its indexes and constraints.
var ownPrivs = []string{"CREATE TABLE", "CREATE SEQUENCE", "CREATE VIEW", "CREATE SYNONYM", "CREATE PROCEDURE", "CREATE TYPE", "CREATE TRIGGER"}

func loadTarget(ctx context.Context, dst *sql.DB, owners []string) (*target, error) {
	t := &target{Schemas: map[string]bool{}, Privs: map[string]bool{}}
	if err := dst.QueryRowContext(ctx, `SELECT SYS_CONTEXT('USERENV','SESSION_USER') FROM dual`).Scan(&t.User); err != nil {
		return nil, oracle.Friendly(err)
	}
	quoted := make([]string, len(owners))
	for i, o := range owners {
		quoted[i] = literal(o)
	}
	for query, into := range map[string]map[string]bool{
		`SELECT username FROM all_users WHERE username IN (` + strings.Join(quoted, ", ") + `)`: t.Schemas,
		// SESSION_PRIVS includes what the account holds through its roles.
		`SELECT privilege FROM session_privs`: t.Privs,
	} {
		rows, err := dst.QueryContext(ctx, query)
		if err != nil {
			return nil, oracle.Friendly(err)
		}
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				rows.Close()
				return nil, oracle.Friendly(err)
			}
			into[name] = true
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, oracle.Friendly(err)
		}
	}
	return t, nil
}

// missing lists the privileges of a set that the account does not hold.
func (t *target) missing(privs []string) []string {
	var out []string
	for _, p := range privs {
		if !t.Privs[p] {
			out = append(out, p)
		}
	}
	return out
}

// mapSchemas decides which schema on the target receives each source
// schema, and says why in words for the preflight.
//
// A schema's objects go to the schema of the same name, which must exist
// already: an Oracle schema is a user, and the tool does not create
// users. To build in a schema other than its own the account needs the
// ANY privileges, and CREATE ANY TABLE is taken as the sign that it has
// been set up for that. One case is made simple: when a single schema is
// migrated and the account cannot build in a schema of that name, because
// there is none or because it lacks the privileges, the objects go into
// the account's own schema instead. With several schemas there is no such
// way out, and each one that cannot be received is a problem.
func mapSchemas(owners []string, t *target) (to map[string]string, notes, problems []string) {
	to = map[string]string{}
	for _, o := range owners {
		switch {
		case o == t.User:
			to[o] = o
			notes = append(notes, fmt.Sprintf("%s goes to the target account's own schema, which has the same name", o))
		case t.Schemas[o] && t.Privs["CREATE ANY TABLE"]:
			to[o] = o
			notes = append(notes, fmt.Sprintf("%s goes to the schema %s on the target", o, o))
		case len(owners) == 1:
			to[o] = t.User
			why := "the target has no schema named " + o
			if t.Schemas[o] {
				why = "the target account may not create objects in the schema " + o + " there"
			}
			notes = append(notes, fmt.Sprintf("%s goes to %s, the target account's own schema, because %s; definitions are rewritten for the new schema name", o, t.User, why))
		case !t.Schemas[o]:
			problems = append(problems, fmt.Sprintf("the target has no schema named %s: create that user there first (the migration does not create users)", o))
		default:
			problems = append(problems, fmt.Sprintf("the target account may not create objects in the schema %s: grant it the ANY privileges (CREATE ANY TABLE and the others preflight lists), or connect as %s", o, o))
		}
	}
	return to, notes, problems
}
