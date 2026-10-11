package postgres

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"

	"dedie/engine/connector"
)

func (s *session) CreateDatabase(ctx context.Context, name string) error {
	if err := connector.CheckName("database", name); err != nil {
		return err
	}
	// UTF8 holds any text; template0 allows it whatever the server default is.
	_, err := s.conn.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()+" ENCODING 'UTF8' TEMPLATE template0")
	return friendly(err)
}

func (s *session) CreateSchema(ctx context.Context, name string) error {
	if err := connector.CheckName("schema", name); err != nil {
		return err
	}
	_, err := s.conn.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{name}.Sanitize())
	return friendly(err)
}

func (s *session) CreateTable(ctx context.Context, def connector.TableDef) error {
	if err := def.Validate(); err != nil {
		return err
	}
	schema := def.Schema
	if schema == "" {
		schema = "public"
	}
	var cols, pk []string
	for _, c := range def.Columns {
		col := pgx.Identifier{c.Name}.Sanitize() + " " + strings.TrimSpace(c.Type)
		if c.Default != "" {
			col += " DEFAULT " + connector.DefaultSQL(c.Default)
		}
		if !c.Nullable || c.PrimaryKey {
			col += " NOT NULL"
		}
		cols = append(cols, col)
		if c.PrimaryKey {
			pk = append(pk, pgx.Identifier{c.Name}.Sanitize())
		}
	}
	if len(pk) > 0 {
		cols = append(cols, "PRIMARY KEY ("+strings.Join(pk, ", ")+")")
	}
	_, err := s.conn.Exec(ctx, "CREATE TABLE "+pgx.Identifier{schema, def.Name}.Sanitize()+" (\n  "+strings.Join(cols, ",\n  ")+"\n)")
	return friendly(err)
}

// ServerObjects lists the databases that accept connections, with their
// owners, and the roles defined on the server.
func (s *session) ServerObjects(ctx context.Context) (*connector.ServerObjects, error) {
	out := &connector.ServerObjects{Databases: []connector.Database{}, Roles: []string{}, Ownership: true}
	rows, err := s.conn.Query(ctx, `SELECT datname, pg_get_userbyid(datdba) FROM pg_database WHERE NOT datistemplate AND datallowconn ORDER BY 1`)
	if err != nil {
		return nil, friendly(err)
	}
	for rows.Next() {
		var d connector.Database
		if err := rows.Scan(&d.Name, &d.Owner); err != nil {
			return nil, err
		}
		out.Databases = append(out.Databases, d)
	}
	if err := rows.Err(); err != nil {
		return nil, friendly(err)
	}
	// Roles named pg_* are built in and cannot own user objects sensibly.
	rows, err = s.conn.Query(ctx, `SELECT rolname FROM pg_roles WHERE rolname !~ '^pg_' ORDER BY 1`)
	if err != nil {
		return nil, friendly(err)
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out.Roles = append(out.Roles, name)
	}
	return out, friendly(rows.Err())
}

// SetOwner changes who owns a database, a schema or a table. The server
// allows it for a superuser, or for the current owner when they are a
// member of the new owning role.
func (s *session) SetOwner(ctx context.Context, c connector.OwnerChange) error {
	if err := connector.CheckName(c.Kind, c.Name); err != nil {
		return err
	}
	if err := connector.CheckName("role", c.Owner); err != nil {
		return err
	}
	owner := pgx.Identifier{c.Owner}.Sanitize()
	var stmt string
	switch c.Kind {
	case connector.OwnDatabase:
		stmt = "ALTER DATABASE " + pgx.Identifier{c.Name}.Sanitize() + " OWNER TO " + owner
	case connector.OwnSchema:
		stmt = "ALTER SCHEMA " + pgx.Identifier{c.Name}.Sanitize() + " OWNER TO " + owner
	case connector.OwnTable:
		schema := c.Schema
		if schema == "" {
			schema = "public"
		}
		// ALTER TABLE also applies to views, and moves the table's
		// indexes and owned sequences with it.
		stmt = "ALTER TABLE " + pgx.Identifier{schema, c.Name}.Sanitize() + " OWNER TO " + owner
	default:
		return errors.New("the owner of a " + c.Kind + " cannot be changed here")
	}
	_, err := s.conn.Exec(ctx, stmt)
	return friendly(err)
}
